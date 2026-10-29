package main

import (
	"errors"
	"fmt"
	"log"
	"math"
	"sync"
	"time"
)

// The monitor is the app minus its menu bar: the poll loop that keeps usage, fleet.json and the
// history fresh, auto-rotation, and the state the dashboard renders. The menu bar app (gui.go)
// drives one.

const (
	// A 5-hour and a 7-day window do not move fast enough to justify polling hard, and the usage
	// endpoint rate limits. The countdowns tick locally in the panel between fetches anyway.
	activePollInterval = 3 * time.Minute
	parkedPollInterval = 20 * time.Minute

	// Per-account backoff after a failed fetch, doubling up to the cap.
	backoffBase = 2 * time.Minute
	backoffCap  = 30 * time.Minute

	// Auto-rotation thresholds. 95 rather than 99 on purpose: at 99% the next turn is likely to be
	// refused mid-flight, and the switch only helps sessions started after it or running through
	// the auth proxy. Raise it here if you would rather squeeze the last few percent out of an account.
	autoSwitchAt       = 95.0
	autoSwitchHeadroom = 80.0 // a candidate must be below this on both windows to be worth moving to
	autoSwitchCooldown = 5 * time.Minute

	// Appended to every switch notice: only sessions outside the auth proxy hold their token.
	switchHint = "Sessions on the auth proxy follow now, others after a restart."
)

type panelAccount struct {
	UUID        string `json:"uuid"`
	Kind        string `json:"kind"`
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
	Accounts   []panelAccount  `json:"accounts"`
	UpdatedAt  time.Time       `json:"updatedAt"`
	Notice     string          `json:"notice,omitempty"`
	Order      []string        `json:"order"`
	CodexOrder []string        `json:"codexOrder"`
	Projects   []projectPolicy `json:"projects"`
}

// density controls how much of the usage picture the menu bar itself carries.
type density string

const (
	densityCondensed density = "condensed" // icon only
	densityExtended  density = "extended"  // icon + percentages
	densityFull      density = "full"      // icon + percentages + reset countdowns
)

// monitor owns the vault. Every mutation goes through its lock: the poll loop writes refreshed
// tokens while tray and panel handlers, or the dashboard's refresh, can act at any moment.
type monitor struct {
	// onPublish shows the state wherever else it is shown: the menu bar app repaints the tray and the
	// panel.
	onPublish func()

	mu             sync.Mutex
	vault          Vault
	state          panelState
	lastParkedPoll time.Time
	density        density
	autoSwitch     bool
	preferred      string
	order          []string
	codexOrder     []string
	projects       []projectPolicy
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

// prefs lays the preferences the monitor owns over whatever is on disk right now. The account
// orders and project policies are written by the CLI too (`vibemon codex add` appends to the Codex
// one), so saving an in-memory copy from a tray toggle would quietly undo them.
// Caller must hold m.mu.
func (m *monitor) prefs() prefs {
	p := loadPrefs()
	p.Density, p.AutoSwitch, p.Preferred = m.density, m.autoSwitch, m.preferred
	return p
}

func newMonitor() *monitor {
	saved := loadPrefs()
	return &monitor{density: saved.Density, autoSwitch: saved.AutoSwitch, preferred: saved.Preferred,
		order: saved.Order, codexOrder: saved.CodexOrder, projects: saved.Projects}
}

// run is everything the app does besides showing itself: the dashboard, both auth proxies and the
// poll loop, which never returns. A taken port (a foreground `vibemon proxy`, say) only costs that
// one server, not the app.
func (m *monitor) run() {
	go m.serveWeb()
	for _, serve := range []func() error{serveProxy, serveCodexProxy} {
		go func() {
			if err := serve(); err != nil {
				log.Printf("vibemon: %v", err)
			}
		}()
	}
	m.loop()
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
	// The CLI writes these while the app runs; the panel and the settings window show them.
	saved := loadPrefs()
	m.order, m.codexOrder, m.projects = saved.Order, saved.CodexOrder, saved.Projects
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
			UUID: a.UUID, Kind: a.kind(), Email: a.Email, Label: a.Label, OrgName: a.OrgName,
			Plan: a.planLabel(), Active: isActive, NeedsReauth: a.NeedsReauth,
			Preferred: a.UUID == m.preferred,
			HasToken:  a.HeadlessToken != "", HasLogin: a.OAuth.AccessToken != "",
			Turns: fleet.turnsLastHour(a.UUID),
		}
		if a.kind() == kindCodex {
			// A Codex account has one thing instead of a login and a token: its home.
			pa.HasLogin = codexLoggedIn(codexHome(a.Email))
			pa.HasToken = pa.HasLogin
		}
		if b, ok := fleet.benched(a.UUID); ok {
			until := b.Until
			pa.BenchedUntil, pa.BenchReason = &until, b.Reason
		}
		switch state, waiting := m.benched(a.UUID); {
		case !pa.HasLogin && a.kind() == kindCodex:
			pa.Error = "not logged in: vibemon codex add " + a.Email
		case !pa.HasLogin:
			pa.Error = "headless token only, no usage"
		case waiting:
			// Still serving a backoff: show the last good numbers rather than hammering the API.
			pa.Usage = previous[a.UUID]
			pa.Error = fmt.Sprintf("%s — retrying in %s", state.reason,
				time.Until(state.until).Round(time.Second))
		// A Codex poll is a plain GET that refreshes nothing, and these numbers drive the rotation
		// of every headless review, so they are refreshed on every tick rather than the parked one.
		case isActive || includeParked || a.kind() == kindCodex:
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

	m.state = panelState{Accounts: accounts, UpdatedAt: time.Now(), Order: m.order,
		CodexOrder: m.codexOrder, Projects: m.projects}
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
			if a.UUID != m.preferred || a.Kind == kindCodex || a.NeedsReauth || a.Usage == nil || a.BenchedUntil != nil {
				continue
			}
			if worstOf(a.Usage) < autoSwitchHeadroom {
				if err := switchTo(m.vault, m.preferred); err != nil {
					m.state.Notice = "auto-switch failed: " + err.Error()
					return false
				}
				m.lastAutoSwitch, m.roamed = time.Now(), false
				m.state.Notice = fmt.Sprintf("Back on %s (%.0f%% used). %s",
					a.Email, worstOf(a.Usage), switchHint)
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
		// last polled numbers say. Claude Code cannot log into a ChatGPT account at all.
		if a.Active || a.Kind == kindCodex || a.NeedsReauth || a.Usage == nil || a.BenchedUntil != nil {
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
	m.state.Notice = fmt.Sprintf("Auto-switched to %s (%.0f%% used). %s",
		target.Email, bestScore, switchHint)
	return true
}

// publish shows the current state everywhere it is shown. Caller must hold m.mu.
func (m *monitor) publish() {
	if m.onPublish != nil {
		m.onPublish()
	}
	webWake()
}
