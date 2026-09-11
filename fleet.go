package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// The fleet ledger is what several concurrent `vibemon exec` processes and the menu bar app share:
// who ran what recently, which accounts a limit has benched, and the last usage numbers the monitor
// polled. It is one JSON file under one flock, because the writers are few and the alternative is a
// daemon. Turns older than a day are dropped on every write.
//
// ponytail: whole-file read-modify-write under flock. Fine for a handful of runners; move to a
// daemon with a socket if exec calls ever number in the hundreds per minute.

type turn struct {
	Account string     `json:"account"`
	Email   string     `json:"email"`
	Model   string     `json:"model,omitempty"`
	At      time.Time  `json:"at"`
	Outcome string     `json:"outcome"`
	Reset   *time.Time `json:"reset,omitempty"`
	Project string     `json:"project,omitempty"`
}

type benchEntry struct {
	Until  time.Time `json:"until"`
	Reason string    `json:"reason"`
}

type fleetState struct {
	Usage   map[string]Usage      `json:"usage,omitempty"` // by vault key, written by the monitor
	Turns   []turn                `json:"turns,omitempty"`
	Bench   map[string]benchEntry `json:"bench,omitempty"`
	Updated time.Time             `json:"updated"`
}

func fleetPath() string { return filepath.Join(stateDir(), "fleet.json") }

func readFleet() fleetState {
	var st fleetState
	raw, err := os.ReadFile(fleetPath())
	if err == nil {
		_ = json.Unmarshal(raw, &st)
	}
	if st.Usage == nil {
		st.Usage = map[string]Usage{}
	}
	if st.Bench == nil {
		st.Bench = map[string]benchEntry{}
	}
	return st
}

// updateFleet applies fn to the ledger under an exclusive lock and writes it back atomically.
func updateFleet(fn func(*fleetState)) (fleetState, error) {
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return fleetState{}, err
	}
	lock, err := os.OpenFile(fleetPath()+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fleetState{}, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fleetState{}, err
	}
	st := readFleet()
	fn(&st)
	cutoff := time.Now().Add(-24 * time.Hour)
	kept := st.Turns[:0]
	for _, t := range st.Turns {
		if t.At.After(cutoff) {
			kept = append(kept, t)
		}
	}
	st.Turns = kept
	for k, b := range st.Bench {
		if time.Now().After(b.Until) {
			delete(st.Bench, k)
		}
	}
	st.Updated = time.Now()
	raw, err := json.Marshal(st)
	if err != nil {
		return st, err
	}
	tmp := fleetPath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return st, err
	}
	return st, os.Rename(tmp, fleetPath())
}

func (st *fleetState) turnsLastHour(key string) int {
	n := 0
	for _, t := range st.Turns {
		if t.Account == key && time.Since(t.At) < time.Hour {
			n++
		}
	}
	return n
}

func (st *fleetState) benched(key string) (benchEntry, bool) {
	b, ok := st.Bench[key]
	if !ok || time.Now().After(b.Until) {
		return benchEntry{}, false
	}
	return b, true
}

// --- locks -------------------------------------------------------------------

// acquireLock serialises headless turns on one session id or one worktree. flock rather than a pid
// file: a crashed holder's lock evaporates with its file descriptor, so there is nothing to reclaim.
func acquireLock(name string) (release func(), err error) {
	dir := filepath.Join(stateDir(), "locks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		holder, _ := os.ReadFile(f.Name())
		fmt.Fprintf(os.Stderr, "vibemon: queued behind pid %s on %s\n", strings.TrimSpace(string(holder)), name)
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			f.Close()
			return nil, err
		}
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(fmt.Sprint(os.Getpid())), 0)
	return func() { f.Close() }, nil
}

func worktreeLockName(dir string) string {
	sum := sha1.Sum([]byte(cleanPath(dir)))
	return "wt-" + hex.EncodeToString(sum[:8])
}

// sessionLockName hashes the id: it is caller-supplied and must never become a path component.
func sessionLockName(sid string) string {
	sum := sha1.Sum([]byte(sid))
	return "s-" + hex.EncodeToString(sum[:8])
}

// lockVault serialises vault read-modify-write cycles across processes: the menu bar app polls
// and saves while a CLI command may be adding a token. Callers hold it around load and save.
func lockVault() func() {
	_ = os.MkdirAll(stateDir(), 0o755)
	f, err := os.OpenFile(filepath.Join(stateDir(), "vault.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	return func() { f.Close() }
}

// --- picking ----------------------------------------------------------------

type candidate struct {
	Account *Account
	Usage   *Usage
	Reason  string    // why it was passed over, empty when runnable
	Until   time.Time // when a passed-over account comes back, zero if unknown
}

type pickOptions struct {
	Dir     string
	Kind    string // kindClaude (default) or kindCodex; rank handles exactly one
	Model   string
	Only    string   // force one account by email
	Exclude []string // emails
	Ceiling float64  // usage percent at which an account counts as spent
}

// rank walks the priority list for dir and returns every account with its verdict, runnable ones
// first in priority order. The first runnable entry is what exec uses. A forced --account is
// ranked on its own, whether or not the policy lists it.
func rank(v Vault, p prefs, st *fleetState, o pickOptions) (runnable, skipped []candidate, project *projectPolicy) {
	now := time.Now()
	if o.Kind == "" {
		o.Kind = kindClaude
	}
	keys, project := p.accountOrder(v, o.Dir, o.Kind)
	if o.Only != "" {
		keys = nil
		if a, err := findByEmail(v, o.Kind, o.Only); err == nil {
			keys = []string{a.UUID}
		}
	}
	for _, key := range keys {
		a := v[key]
		c := candidate{Account: a}
		if u, ok := st.Usage[key]; ok {
			c.Usage = &u
		}
		switch {
		case containsFold(o.Exclude, a.Email):
			c.Reason = "excluded"
		case a.kind() == kindCodex && !codexLoggedIn(codexHome(a.Email)):
			c.Reason = "not logged in (vibemon codex add " + a.Email + ")"
		case a.kind() == kindClaude && a.HeadlessToken == "":
			c.Reason = "no headless token (vibemon add-token)"
		default:
			if b, ok := st.benched(key); ok {
				c.Reason, c.Until = b.Reason, b.Until
			} else if c.Usage != nil {
				if pct, gauge := spentGauge(c.Usage, o.Model, o.Ceiling, now); gauge != nil {
					c.Reason = fmt.Sprintf("%s at %.0f%%", gauge.Label, pct)
					if gauge.ResetsAt != nil {
						c.Until = *gauge.ResetsAt
					}
				}
			}
		}
		if c.Reason == "" {
			runnable = append(runnable, c)
		} else {
			skipped = append(skipped, c)
		}
	}
	return runnable, skipped, project
}

// spentGauge returns the first window at or over the ceiling, counting the per-model window only
// when it scopes the model about to run. Cached numbers are only trusted while their window is
// still open; without a reset time, for six hours after the poll. The monitor may not be running.
func spentGauge(u *Usage, model string, ceiling float64, now time.Time) (float64, *Gauge) {
	live := func(g *Gauge) bool {
		if g.ResetsAt != nil {
			return g.ResetsAt.After(now)
		}
		return now.Sub(u.FetchedAt) < 6*time.Hour
	}
	if u.Session.Percent >= ceiling && live(&u.Session) {
		return u.Session.Percent, &u.Session
	}
	if u.Weekly.Percent >= ceiling && live(&u.Weekly) {
		return u.Weekly.Percent, &u.Weekly
	}
	if u.Scoped != nil && u.Scoped.Percent >= ceiling && live(u.Scoped) && (model == "" || strings.EqualFold(u.Scoped.Label, model)) {
		return u.Scoped.Percent, u.Scoped
	}
	return 0, nil
}

func earliestReturn(skipped []candidate) (time.Time, bool) {
	var best time.Time
	for _, c := range skipped {
		if !c.Until.IsZero() && (best.IsZero() || c.Until.Before(best)) {
			best = c.Until
		}
	}
	return best, !best.IsZero()
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}
