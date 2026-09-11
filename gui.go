package main

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

//go:embed all:frontend
var assets embed.FS

const (
	// A 5-hour and a 7-day window do not move fast enough to justify polling hard, and the usage
	// endpoint rate limits. The countdowns tick locally in the panel between fetches anyway.
	activePollInterval = 3 * time.Minute
	parkedPollInterval = 20 * time.Minute

	// Per-account backoff after a failed fetch, doubling up to the cap.
	backoffBase = 2 * time.Minute
	backoffCap  = 30 * time.Minute

	// Auto-rotation thresholds. 95 rather than 99 on purpose: at 99% the next turn is likely to be
	// refused mid-flight, and the switch only helps sessions started after it. Raise it here if you
	// would rather squeeze the last few percent out of an account.
	autoSwitchAt       = 95.0
	autoSwitchHeadroom = 80.0 // a candidate must be below this on both windows to be worth moving to
	autoSwitchCooldown = 5 * time.Minute
)

type panelAccount struct {
	UUID        string `json:"uuid"`
	Email       string `json:"email"`
	Label       string `json:"label"`
	OrgName     string `json:"orgName,omitempty"`
	Plan        string `json:"plan,omitempty"`
	Active      bool   `json:"active"`
	Preferred   bool   `json:"preferred"`
	NeedsReauth bool   `json:"needsReauth"`
	Usage       *Usage `json:"usage,omitempty"`
	Error       string `json:"error,omitempty"`
	// Fleet view: whether exec can use this account, how busy it has been, and whether a limit
	// benched it. Fed by the ledger exec writes.
	HasToken     bool       `json:"hasToken"`
	HasLogin     bool       `json:"hasLogin"`
	Turns        int        `json:"turns"`
	BenchedUntil *time.Time `json:"benchedUntil,omitempty"`
	BenchReason  string     `json:"benchReason,omitempty"`
}

type panelState struct {
	Accounts  []panelAccount  `json:"accounts"`
	UpdatedAt time.Time       `json:"updatedAt"`
	Notice    string          `json:"notice,omitempty"`
	Order     []string        `json:"order"`
	Projects  []projectPolicy `json:"projects"`
}

// density controls how much of the usage picture the menu bar itself carries.
type density string

const (
	densityCondensed density = "condensed" // icon only
	densityExtended  density = "extended"  // icon + percentages
	densityFull      density = "full"      // icon + percentages + reset countdowns
)

var densityLabels = []struct {
	value density
	label string
}{
	{densityCondensed, "Condensed — icon only"},
	{densityExtended, "Extended — with percentages"},
	{densityFull, "Extra extended — with reset times"},
}

// monitor owns the vault. Every mutation goes through its lock: the poll loop writes refreshed
// tokens while tray and panel handlers can switch accounts at any moment.
type monitor struct {
	app  *application.App
	tray *application.SystemTray

	mu             sync.Mutex
	vault          Vault
	state          panelState
	lastParkedPoll time.Time
	density        density
	autoSwitch     bool
	preferred      string
	order          []string
	projects       []projectPolicy
	settings       *application.WebviewWindow
	lastAutoSwitch time.Time
	// Set when rotation moved off the preferred account, so vibemon knows it may come home later.
	// A manual switch clears it: an explicit choice outranks the preference.
	roamed bool
	// Per-account "do not call before" state, so one failing account cannot keep hammering the API.
	backoff map[string]backoffState
}

type backoffState struct {
	until    time.Time
	failures int
	reason   string
}

// penalise records a failed fetch and returns how long that account is now benched. The server's
// own Retry-After wins over our guess whenever it sends one.
func (m *monitor) penalise(uuid string, err error) time.Duration {
	if m.backoff == nil {
		m.backoff = map[string]backoffState{}
	}
	s := m.backoff[uuid]
	s.failures++

	wait := backoffBase << min(s.failures-1, 8)
	var rl *rateLimitError
	if errors.As(err, &rl) {
		s.reason = "rate limited"
		if rl.RetryAfter > 0 {
			wait = rl.RetryAfter
		}
	} else {
		s.reason = truncate(err.Error(), 90)
	}
	wait = min(wait, backoffCap)

	s.until = time.Now().Add(wait)
	m.backoff[uuid] = s
	return wait
}

// benched reports whether an account is still serving a backoff, and for how much longer.
func (m *monitor) benched(uuid string) (backoffState, bool) {
	s, ok := m.backoff[uuid]
	if !ok || time.Now().After(s.until) {
		return backoffState{}, false
	}
	return s, true
}

// prefs snapshots the preferences that live on the monitor. Caller must hold m.mu.
func (m *monitor) prefs() prefs {
	return prefs{Density: m.density, AutoSwitch: m.autoSwitch, Preferred: m.preferred,
		Order: m.order, Projects: m.projects}
}

func runGUI() error {
	frontend, err := fs.Sub(assets, "frontend")
	if err != nil {
		return err
	}

	saved := loadPrefs()
	m := &monitor{density: saved.Density, autoSwitch: saved.AutoSwitch, preferred: saved.Preferred,
		order: saved.Order, projects: saved.Projects}
	m.app = application.New(application.Options{
		Name:        "vibemon",
		Description: "Claude Code usage monitor",
		Assets:      application.AssetOptions{Handler: application.AssetFileServerFS(frontend)},
		Mac: application.MacOptions{
			// Accessory keeps vibemon out of the Dock and the app switcher — it lives in the menu bar.
			ActivationPolicy: application.ActivationPolicyAccessory,
		},
	})

	window := m.app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:             "panel",
		Width:            360,
		Height:           500,
		Frameless:        true,
		AlwaysOnTop:      true,
		Hidden:           true,
		DisableResize:    true,
		HideOnFocusLost:  true,
		BackgroundColour: application.RGBA{Red: 8, Green: 14, Blue: 10, Alpha: 255},
	})

	// The settings window is a real window: it holds forms, so it must survive losing focus.
	m.settings = m.app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:             "settings",
		Title:            "vibemon settings",
		URL:              "/settings.html",
		Width:            640,
		Height:           620,
		MinWidth:         520,
		MinHeight:        400,
		Hidden:           true,
		BackgroundColour: application.RGBA{Red: 8, Green: 14, Blue: 10, Alpha: 255},
	})

	// Closing the settings window would destroy it and leave a dead handle; hide it instead.
	m.settings.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		e.Cancel()
		m.settings.Hide()
	})

	m.tray = m.app.SystemTray.New()
	m.tray.SetTemplateIcon(crtIcon())
	m.tray.AttachWindow(window).WindowOffset(6)

	m.app.Event.On("panel:ready", func(*application.CustomEvent) { m.emit() })
	m.app.Event.On("panel:refresh", func(*application.CustomEvent) { go m.poll(true) })
	m.app.Event.On("panel:capture", func(*application.CustomEvent) { go m.capture() })
	m.app.Event.On("panel:switch", func(e *application.CustomEvent) {
		uuid, _ := firstString(e.Data)
		go m.switchTo(uuid)
	})
	m.app.Event.On("panel:remove", func(e *application.CustomEvent) {
		uuid, _ := firstString(e.Data)
		m.confirmRemove(uuid)
	})
	m.app.Event.On("panel:quit", func(*application.CustomEvent) { m.app.Quit() })
	m.app.Event.On("panel:settings", func(*application.CustomEvent) { m.showSettings() })
	m.app.Event.On("settings:policy", func(e *application.CustomEvent) { go m.setPolicy(e.Data) })
	m.app.Event.On("settings:token", func(e *application.CustomEvent) { go m.setToken(e.Data) })

	// Start polling only once the tray actually exists — publishing before Run() would push the
	// first label into a systray that has not been created yet, leaving it stuck until the next tick.
	m.app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		go m.loop()
	})

	return m.app.Run()
}

// firstString unwraps the payload of a JS-side Events.Emit(name, data), which arrives either as a
// bare value or wrapped in a single-element slice depending on how it was sent.
func firstString(data any) (string, bool) {
	switch v := data.(type) {
	case string:
		return v, true
	case []any:
		if len(v) > 0 {
			if s, ok := v[0].(string); ok {
				return s, true
			}
		}
	}
	return "", false
}

func (m *monitor) loop() {
	m.poll(true)
	for range time.Tick(activePollInterval) {
		m.poll(false)
	}
}

// poll refreshes usage and, when auto-rotation is armed, may swap the active account. A switch
// invalidates everything just gathered (the active flags above all), so it re-polls once.
func (m *monitor) poll(includeParked bool) {
	if !m.pollOnce(includeParked) {
		return
	}
	m.mu.Lock()
	notice := m.state.Notice // the re-poll rebuilds state from scratch and would drop it
	m.mu.Unlock()

	m.pollOnce(true)

	m.mu.Lock()
	m.state.Notice = notice
	m.publish()
	m.mu.Unlock()
}

// pollOnce refreshes usage for the active account every tick and for parked accounts far less
// often — each parked poll may burn a token refresh, and their numbers barely move while unused.
// It reports whether it auto-switched accounts.
func (m *monitor) pollOnce(includeParked bool) (switched bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock := lockVault() // a CLI add-token during this tick's HTTP calls must not be clobbered by the save below
	defer unlock()

	v, err := loadVault()
	if err != nil {
		m.state = panelState{Notice: err.Error(), UpdatedAt: time.Now()}
		m.publish()
		return false
	}
	m.vault = v
	if len(v) == 0 {
		m.state = panelState{Notice: "No accounts yet — log in with Claude Code, then Capture.", UpdatedAt: time.Now()}
		m.publish()
		return false
	}

	if includeParked || time.Since(m.lastParkedPoll) >= parkedPollInterval {
		m.lastParkedPoll = time.Now()
		includeParked = true
	}

	active := activeUUID(v)
	previous := map[string]*Usage{}
	for _, a := range m.state.Accounts {
		previous[a.UUID] = a.Usage
	}
	fleet := readFleet()
	polled := map[string]Usage{}

	accounts := make([]panelAccount, 0, len(v))
	for _, a := range v.sorted() {
		isActive := a.UUID == active
		pa := panelAccount{
			UUID: a.UUID, Email: a.Email, Label: a.Label, OrgName: a.OrgName,
			Plan: a.planLabel(), Active: isActive, NeedsReauth: a.NeedsReauth,
			Preferred: a.UUID == m.preferred,
			HasToken:  a.HeadlessToken != "", HasLogin: a.OAuth.AccessToken != "",
			Turns: fleet.turnsLastHour(a.UUID),
		}
		if b, ok := fleet.benched(a.UUID); ok {
			until := b.Until
			pa.BenchedUntil, pa.BenchReason = &until, b.Reason
		}
		switch state, waiting := m.benched(a.UUID); {
		case !pa.HasLogin:
			pa.Error = "headless token only, no usage"
		case waiting:
			// Still serving a backoff: show the last good numbers rather than hammering the API.
			pa.Usage = previous[a.UUID]
			pa.Error = fmt.Sprintf("%s — retrying in %s", state.reason,
				time.Until(state.until).Round(time.Second))
		case isActive || includeParked:
			u, err := usageFor(v, a, isActive)
			if err != nil {
				wait := m.penalise(a.UUID, err)
				pa.Usage = previous[a.UUID] // keep the last good numbers on screen
				pa.Error = fmt.Sprintf("%s — retrying in %s",
					truncate(err.Error(), 90), wait.Round(time.Second))
			} else {
				delete(m.backoff, a.UUID)
				pa.Usage = &u
				polled[a.UUID] = u
			}
		default:
			pa.Usage = previous[a.UUID]
		}
		pa.NeedsReauth = a.NeedsReauth
		accounts = append(accounts, pa)
	}
	cacheUsage(polled)

	m.state = panelState{Accounts: accounts, UpdatedAt: time.Now(), Order: m.order, Projects: m.projects}
	if err := saveVault(v); err != nil {
		m.state.Notice = "vault save failed: " + err.Error()
	}
	switched = m.autoRotate()
	m.publish()
	return switched
}

// worstOf is the number that decides whether an account is usable: whichever limit bites first.
func worstOf(u *Usage) float64 {
	if u == nil {
		return 0
	}
	return math.Max(u.Session.Percent, u.Weekly.Percent)
}

// autoRotate moves to a fresher account once the active one is nearly spent. Caller must hold m.mu.
//
// This only changes which account the *next* Claude Code start uses — running sessions hold their
// token in memory and are unaffected, so rotation cannot rescue a session that is already blocked.
func (m *monitor) autoRotate() bool {
	if !m.autoSwitch || time.Since(m.lastAutoSwitch) < autoSwitchCooldown {
		return false
	}
	var active *panelAccount
	for i := range m.state.Accounts {
		if m.state.Accounts[i].Active {
			active = &m.state.Accounts[i]
		}
	}
	if active == nil {
		return false
	}

	// Come home first: if rotation moved off the preferred account and it has recovered, go back
	// before considering anything else.
	if m.roamed && m.preferred != "" && active.UUID != m.preferred {
		for _, a := range m.state.Accounts {
			if a.UUID != m.preferred || a.NeedsReauth || a.Usage == nil || a.BenchedUntil != nil {
				continue
			}
			if worstOf(a.Usage) < autoSwitchHeadroom {
				if err := switchTo(m.vault, m.preferred); err != nil {
					m.state.Notice = "auto-switch failed: " + err.Error()
					return false
				}
				m.lastAutoSwitch, m.roamed = time.Now(), false
				m.state.Notice = fmt.Sprintf("Back on %s (%.0f%% used). Restart Claude Code to use it.",
					a.Email, worstOf(a.Usage))
				return true
			}
		}
	}

	if active.Usage == nil || worstOf(active.Usage) < autoSwitchAt {
		return false
	}

	best, bestScore := "", autoSwitchHeadroom
	for _, a := range m.state.Accounts {
		// An account a headless runner just drove into a limit is not a refuge, whatever its
		// last polled numbers say.
		if a.Active || a.NeedsReauth || a.Usage == nil || a.BenchedUntil != nil {
			continue
		}
		score := worstOf(a.Usage)
		if score >= autoSwitchHeadroom {
			continue
		}
		// The preferred account wins outright whenever it has room; otherwise take the freshest.
		if a.UUID == m.preferred {
			best, bestScore = a.UUID, score
			break
		}
		if best == "" || score < bestScore {
			best, bestScore = a.UUID, score
		}
	}
	if best == "" {
		m.state.Notice = fmt.Sprintf("%s is at %.0f%% and no other account has headroom.",
			active.Email, worstOf(active.Usage))
		return false
	}

	target := m.vault[best]
	if err := switchTo(m.vault, best); err != nil {
		m.state.Notice = "auto-switch failed: " + err.Error()
		return false
	}
	m.lastAutoSwitch = time.Now()
	if m.preferred != "" && active.UUID == m.preferred {
		m.roamed = true
	}
	m.state.Notice = fmt.Sprintf("Auto-switched to %s (%.0f%% used). Restart Claude Code to use it.",
		target.Email, bestScore)
	return true
}

func (m *monitor) setAutoSwitch(on bool) {
	m.mu.Lock()
	m.autoSwitch = on
	p := m.prefs()
	m.mu.Unlock()
	if err := savePrefs(p); err != nil {
		m.notify("could not save preference: " + err.Error())
		return
	}
	m.poll(true)
}

func (m *monitor) setPreferred(uuid string) {
	m.mu.Lock()
	if m.preferred == uuid {
		uuid = "" // clicking the current preference clears it
	}
	m.preferred, m.roamed = uuid, false
	p := m.prefs()
	m.mu.Unlock()
	if err := savePrefs(p); err != nil {
		m.notify("could not save preference: " + err.Error())
		return
	}
	m.poll(true)
}

func (m *monitor) capture() {
	m.mu.Lock()
	a, err := capture()
	m.mu.Unlock()
	if err != nil {
		m.notify("capture failed: " + err.Error())
		return
	}
	m.notify("captured " + a.Email)
	m.poll(true)
}

func (m *monitor) switchTo(uuid string) {
	if uuid == "" {
		return
	}
	m.mu.Lock()
	unlock := lockVault()
	v, err := loadVault()
	if err != nil {
		unlock()
		m.mu.Unlock()
		m.notify(err.Error())
		return
	}
	m.vault = v
	target, ok := v[uuid]
	if !ok {
		unlock()
		m.mu.Unlock()
		m.notify("unknown account")
		return
	}
	running := claudeSessionsRunning()
	err = switchTo(v, uuid)
	m.roamed = false // an explicit choice outranks the preferred-account preference
	unlock()
	m.mu.Unlock()

	if err != nil {
		m.notify("switch failed: " + err.Error())
		return
	}
	msg := "Switched to " + target.Email + " — restart Claude Code."
	if running > 0 {
		msg = fmt.Sprintf("Switched to %s — %d running session(s) keep the old account until restarted.", target.Email, running)
	}
	m.notify(msg)
	m.poll(true)
}

// confirmRemove asks before forgetting an account: the vault holds the only copy of that account's
// refresh token, so removing it means a real `claude auth login` round trip to get it back.
func (m *monitor) confirmRemove(uuid string) {
	if uuid == "" {
		return
	}
	m.mu.Lock()
	target, ok := m.vault[uuid]
	if !ok {
		m.mu.Unlock()
		return
	}
	email := target.Email
	m.mu.Unlock()

	dialog := m.app.Dialog.Question()
	dialog.SetTitle("Remove account")
	dialog.SetMessage(fmt.Sprintf(
		"Forget %s?\n\nvibemon stops tracking it. Claude Code stays signed in as whoever it is signed in as now; "+
			"to use this account again you will need to run `claude auth login` and capture it.", email))
	cancel := dialog.AddButton("Cancel")
	cancel.SetAsCancel()
	remove := dialog.AddButton("Remove")
	remove.OnClick(func() { go m.remove(uuid) })
	dialog.Show()
}

func (m *monitor) remove(uuid string) {
	m.mu.Lock()
	unlock := lockVault()
	v, err := loadVault()
	if err != nil {
		unlock()
		m.mu.Unlock()
		m.notify(err.Error())
		return
	}
	m.vault = v
	email := ""
	if a, ok := v[uuid]; ok {
		email = a.Email
	}
	err = forget(v, uuid)
	unlock()
	m.mu.Unlock()

	if err != nil {
		m.notify("remove failed: " + err.Error())
		return
	}
	m.notify("removed " + email)
	m.poll(true)
}

func (m *monitor) notify(msg string) {
	m.mu.Lock()
	m.state.Notice = msg
	m.mu.Unlock()
	m.emit()
}

// emit re-sends the current state without re-polling — used when the panel opens or a notice changes.
func (m *monitor) emit() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.publish()
}

// publish pushes state to the panel and repaints the menu bar. Caller must hold m.mu.
func (m *monitor) publish() {
	m.app.Event.Emit("state", m.state)
	m.tray.SetLabel(m.trayLabel())
	m.tray.SetMenu(m.buildMenu())
}

// trayLabel renders the text beside the icon. In condensed mode it stays empty so only the glyph
// shows — except for conditions the user must not miss, which override the setting.
func (m *monitor) trayLabel() string {
	for _, a := range m.state.Accounts {
		if !a.Active {
			continue
		}
		if a.NeedsReauth {
			return "⚠ login"
		}
		if m.density == densityCondensed {
			return ""
		}
		if a.Usage == nil {
			return "…"
		}
		label := fmt.Sprintf("%.0f%% · %.0f%%", a.Usage.Session.Percent, a.Usage.Weekly.Percent)
		if m.density == densityFull {
			label = fmt.Sprintf("%.0f%% %s · %.0f%% %s",
				a.Usage.Session.Percent, until(a.Usage.Session.ResetsAt),
				a.Usage.Weekly.Percent, until(a.Usage.Weekly.ResetsAt))
		}
		if a.Usage.Session.Severity != "normal" || a.Usage.Weekly.Severity != "normal" {
			label = "⚠ " + label
		}
		return label
	}
	if m.density == densityCondensed {
		return ""
	}
	if len(m.state.Accounts) > 0 {
		return "?" // signed into an account vibemon has not captured
	}
	return "—"
}

func (m *monitor) setDensity(d density) {
	m.mu.Lock()
	m.density = d
	p := m.prefs()
	m.mu.Unlock()
	if err := savePrefs(p); err != nil {
		m.notify("could not save preference: " + err.Error())
		return
	}
	m.emit()
}

// buildMenu is the right-click menu; the left click opens the panel instead. Caller must hold m.mu.
func (m *monitor) buildMenu() *application.Menu {
	menu := m.app.Menu.New()
	for _, a := range m.state.Accounts {
		label := a.Email
		if a.NeedsReauth {
			label += "  ⚠ needs login"
		}
		uuid := a.UUID
		item := menu.AddCheckbox(label, a.Active)
		item.OnClick(func(*application.Context) { go m.switchTo(uuid) })
	}
	if len(m.state.Accounts) > 0 {
		menu.AddSeparator()
	}
	display := menu.AddSubmenu("Menu bar display")
	for _, choice := range densityLabels {
		value := choice.value
		display.AddRadio(choice.label, value == m.density).
			OnClick(func(*application.Context) { go m.setDensity(value) })
	}

	if len(m.state.Accounts) > 1 {
		menu.AddCheckbox("Auto-switch when exhausted", m.autoSwitch).
			OnClick(func(*application.Context) { go m.setAutoSwitch(!m.autoSwitch) })
		preferred := menu.AddSubmenu("Preferred account")
		preferred.AddRadio("None", m.preferred == "").
			OnClick(func(*application.Context) { go m.setPreferred("") })
		for _, a := range m.state.Accounts {
			uuid, email := a.UUID, a.Email
			preferred.AddRadio(email, uuid == m.preferred).
				OnClick(func(*application.Context) { go m.setPreferred(uuid) })
		}
	}
	menu.AddSeparator()
	menu.Add("Settings…").OnClick(func(*application.Context) { m.showSettings() })
	menu.Add("Capture current account").OnClick(func(*application.Context) { go m.capture() })
	menu.Add("Refresh now").OnClick(func(*application.Context) { go m.poll(true) })
	menu.AddSeparator()
	menu.Add("Quit vibemon").OnClick(func(*application.Context) { m.app.Quit() })
	return menu
}

func (m *monitor) showSettings() {
	m.settings.Show()
	m.settings.Focus()
	m.emit()
}

// setPolicy stores the exec priority order and the per-project account lists from the settings
// page. Nothing here is secret; it goes to prefs.json, where exec reads it.
func (m *monitor) setPolicy(data any) {
	raw, err := json.Marshal(unwrap(data))
	if err != nil {
		m.notify("settings: " + err.Error())
		return
	}
	var in struct {
		Order    []string        `json:"order"`
		Projects []projectPolicy `json:"projects"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		m.notify("settings: " + err.Error())
		return
	}
	m.mu.Lock()
	m.order, m.projects = in.Order, in.Projects
	m.state.Order, m.state.Projects = in.Order, in.Projects
	p := m.prefs()
	m.mu.Unlock()
	if err := savePrefs(p); err != nil {
		m.notify("could not save settings: " + err.Error())
		return
	}
	m.notify("settings saved")
}

// setToken attaches a pasted `claude setup-token` token to an account, or creates a token-only one.
func (m *monitor) setToken(data any) {
	raw, err := json.Marshal(unwrap(data))
	if err != nil {
		m.notify("token: " + err.Error())
		return
	}
	var in struct {
		Email string `json:"email"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		m.notify("token: " + err.Error())
		return
	}
	m.mu.Lock()
	unlock := lockVault()
	v, err := loadVault()
	var a *Account
	if err == nil {
		m.vault = v
		a, err = addHeadlessToken(v, in.Email, in.Token)
	}
	unlock()
	m.mu.Unlock()
	if err != nil {
		m.notify("token: " + err.Error())
		return
	}
	m.notify("stored headless token for " + a.Email)
	m.poll(true)
}

// unwrap mirrors firstString for object payloads: JS-side Emit may wrap the value in a slice.
func unwrap(data any) any {
	if v, ok := data.([]any); ok && len(v) == 1 {
		return v[0]
	}
	return data
}

func fatal(err error) {
	log.Fatalf("vibemon: %v", err)
}
