//go:build darwin

package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// The menu bar app: tray, panel and settings windows around a monitor. Wails needs cgo and, on
// Linux, GTK, so this half of vibemon is macOS only; Linux runs the monitor headless instead.

//go:embed all:frontend
var assets embed.FS

// menuBar is the monitor with its windows. Handlers that only touch the monitor stay monitor methods.
type menuBar struct {
	*monitor
	app      *application.App
	tray     *application.SystemTray
	settings *application.WebviewWindow
}

var densityLabels = []struct {
	value density
	label string
}{
	{densityCondensed, "Condensed — icon only"},
	{densityExtended, "Extended — with percentages"},
	{densityFull, "Extra extended — with reset times"},
}

// runApp is what a bare `vibemon` does on macOS.
func runApp() error {
	frontend, err := fs.Sub(assets, "frontend")
	if err != nil {
		return err
	}

	m := &menuBar{monitor: newMonitor()}
	m.onPublish = m.paint
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
		go m.run()
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
	msg := "Switched to " + target.Email + ". " + switchHint
	if running > 0 {
		msg = fmt.Sprintf("Switched to %s. %d running session(s) keep the old account until restarted, unless on the auth proxy.",
			target.Email, running)
	}
	m.notify(msg)
	m.poll(true)
}

// confirmRemove asks before forgetting an account: the vault holds the only copy of that account's
// refresh token, so removing it means a real `claude auth login` round trip to get it back.
func (m *menuBar) confirmRemove(uuid string) {
	if uuid == "" {
		return
	}
	m.mu.Lock()
	target, ok := m.vault[uuid]
	if !ok {
		m.mu.Unlock()
		return
	}
	email, isCodex := target.Email, target.kind() == kindCodex
	m.mu.Unlock()

	message := fmt.Sprintf(
		"Forget %s?\n\nvibemon stops tracking it. Claude Code stays signed in as whoever it is signed in as now; "+
			"to use this account again you will need to run `claude auth login` and capture it.", email)
	if isCodex {
		message = fmt.Sprintf(
			"Forget %s?\n\nvibemon stops tracking it. The login stays in %s; delete that directory to log the account out.",
			email, codexHome(email))
	}
	dialog := m.app.Dialog.Question()
	dialog.SetTitle("Remove account")
	dialog.SetMessage(message)
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

// paint pushes state to the panel and repaints the menu bar. Caller must hold m.mu (it runs from
// publish).
func (m *menuBar) paint() {
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
		// Ultra has no session window: the label shows the weekly one alone.
		var parts []string
		for _, g := range []Gauge{a.Usage.Session, a.Usage.Weekly} {
			if g.Label == "Session" && !g.Live() {
				continue
			}
			p := fmt.Sprintf("%.0f%%", g.Percent)
			if m.density == densityFull {
				p += " " + until(g.ResetsAt)
			}
			parts = append(parts, p)
		}
		label := strings.Join(parts, " · ")
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
func (m *menuBar) buildMenu() *application.Menu {
	menu := m.app.Menu.New()
	switchable := 0
	for _, a := range m.state.Accounts {
		if a.Kind == kindCodex {
			continue // there is nothing to switch to: codex picks its account per run
		}
		switchable++
		label := a.Email
		if a.NeedsReauth {
			label += "  ⚠ needs login"
		}
		uuid := a.UUID
		item := menu.AddCheckbox(label, a.Active)
		item.OnClick(func(*application.Context) { go m.switchTo(uuid) })
	}
	if switchable > 0 {
		menu.AddSeparator()
	}
	display := menu.AddSubmenu("Menu bar display")
	for _, choice := range densityLabels {
		value := choice.value
		display.AddRadio(choice.label, value == m.density).
			OnClick(func(*application.Context) { go m.setDensity(value) })
	}

	if switchable > 1 {
		menu.AddCheckbox("Auto-switch when exhausted", m.autoSwitch).
			OnClick(func(*application.Context) { go m.setAutoSwitch(!m.autoSwitch) })
		preferred := menu.AddSubmenu("Preferred account")
		preferred.AddRadio("None", m.preferred == "").
			OnClick(func(*application.Context) { go m.setPreferred("") })
		for _, a := range m.state.Accounts {
			if a.Kind == kindCodex {
				continue
			}
			uuid, email := a.UUID, a.Email
			preferred.AddRadio(email, uuid == m.preferred).
				OnClick(func(*application.Context) { go m.setPreferred(uuid) })
		}
	}
	menu.AddSeparator()
	menu.Add("Settings…").OnClick(func(*application.Context) { m.showSettings() })
	menu.Add("Open dashboard").OnClick(func(*application.Context) {
		if err := m.app.Browser.OpenURL("http://" + webAddr()); err != nil {
			m.notify("could not open the dashboard: " + err.Error())
		}
	})
	menu.Add("Capture current account").OnClick(func(*application.Context) { go m.capture() })
	menu.Add("Refresh now").OnClick(func(*application.Context) { go m.poll(true) })
	menu.AddSeparator()
	menu.Add("Quit vibemon").OnClick(func(*application.Context) { m.app.Quit() })
	return menu
}

func (m *menuBar) showSettings() {
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
		Order      []string        `json:"order"`
		CodexOrder []string        `json:"codexOrder"`
		Projects   []projectPolicy `json:"projects"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		m.notify("settings: " + err.Error())
		return
	}
	m.mu.Lock()
	m.order, m.codexOrder, m.projects = in.Order, in.CodexOrder, in.Projects
	m.state.Order, m.state.CodexOrder, m.state.Projects = in.Order, in.CodexOrder, in.Projects
	p := m.prefs()
	p.Order, p.CodexOrder, p.Projects = in.Order, in.CodexOrder, in.Projects
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
