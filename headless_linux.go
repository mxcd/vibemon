package main

import "log"

// runApp is what a bare `vibemon` does on Linux, where there is no menu bar to live in: the menu bar
// app's background work in the foreground, for a systemd user unit to keep alive. Both auth proxies,
// the poll loop that keeps fleet.json and the history fresh (and rotates accounts when prefs.json
// arms autoSwitch), and the dashboard on 127.0.0.1:6660. Logs go to stderr, which is the journal.
func runApp() error {
	m := newMonitor()
	// No panel to show notices in, so each one is logged, once while it repeats: poll re-publishes a
	// switch notice after its re-poll, and a "no headroom" one comes back every tick.
	last := ""
	m.onPublish = func() {
		if n := m.state.Notice; n != "" && n != last {
			log.Printf("vibemon: %s", n)
			last = n
		}
	}
	m.run()
	return nil
}
