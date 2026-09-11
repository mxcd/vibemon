package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testVault() Vault {
	return Vault{
		"a": {UUID: "a", Email: "a@x.io", HeadlessToken: "sk-ant-oat01-a", OAuth: OAuth{AccessToken: "la"}},
		"b": {UUID: "b", Email: "b@x.io", HeadlessToken: "sk-ant-oat01-b", OAuth: OAuth{AccessToken: "lb"}},
		"c": {UUID: "c", Email: "c@x.io", HeadlessToken: "sk-ant-oat01-c"},
		"d": {UUID: "d", Email: "d@x.io", OAuth: OAuth{AccessToken: "ld"}}, // login only, no token
	}
}

func TestRankFollowsProjectOrderAndSkipsSpent(t *testing.T) {
	v := testVault()
	p := prefs{
		Order:    []string{"c", "b", "a"},
		Projects: []projectPolicy{{Path: "/work/proj", Accounts: []string{"a", "b", "c"}}},
	}
	reset := time.Now().Add(2 * time.Hour)
	st := &fleetState{
		Usage: map[string]Usage{
			"a": {Session: Gauge{Percent: 95, ResetsAt: &reset}, Weekly: Gauge{Percent: 50}, FetchedAt: time.Now()},
			"b": {Session: Gauge{Percent: 10}, Weekly: Gauge{Percent: 20},
				Scoped: &Gauge{Label: "Fable", Percent: 99}, FetchedAt: time.Now()},
		},
		Bench: map[string]benchEntry{},
	}

	// Inside the project: a is spent, b runs opus fine, c has no numbers and is assumed fresh.
	run, skip, proj := rank(v, p, st, pickOptions{Dir: "/work/proj/worktrees/x", Model: "opus", Ceiling: 90})
	if proj == nil || proj.Path != "/work/proj" {
		t.Fatalf("project not matched through a subdirectory: %+v", proj)
	}
	if len(run) != 2 || run[0].Account.UUID != "b" || run[1].Account.UUID != "c" {
		t.Fatalf("want [b c], got %v", emails(run))
	}
	if len(skip) != 1 || skip[0].Account.UUID != "a" || skip[0].Until.IsZero() {
		t.Fatalf("a must be skipped with its reset time, got %+v", skip)
	}

	// The scoped Fable window only bites when Fable is the model asked for.
	run, _, _ = rank(v, p, st, pickOptions{Dir: "/work/proj", Model: "fable", Ceiling: 90})
	if len(run) != 1 || run[0].Account.UUID != "c" {
		t.Fatalf("fable: want [c], got %v", emails(run))
	}

	// Outside any project the global order applies, and a login-only account is never runnable.
	run, skip, proj = rank(v, p, st, pickOptions{Dir: "/elsewhere", Model: "opus", Ceiling: 90})
	if proj != nil {
		t.Fatalf("no project expected, got %+v", proj)
	}
	if len(run) != 2 || run[0].Account.UUID != "c" || run[1].Account.UUID != "b" {
		t.Fatalf("global order: want [c b], got %v", emails(run))
	}
	for _, c := range skip {
		if c.Account.UUID == "d" {
			t.Fatal("d has no headless token and must not be listed as skipped-by-order (it is not in the order at all)")
		}
	}

	// A bench from a limit overrides good numbers; --account narrows to one.
	st.Bench["b"] = benchEntry{Until: time.Now().Add(time.Hour), Reason: "session limit"}
	run, _, _ = rank(v, p, st, pickOptions{Dir: "/elsewhere", Model: "opus", Ceiling: 90})
	if len(run) != 1 || run[0].Account.UUID != "c" {
		t.Fatalf("benched b must drop out: got %v", emails(run))
	}
	run, _, _ = rank(v, prefs{}, st, pickOptions{Dir: "/elsewhere", Only: "C@X.IO", Ceiling: 90})
	if len(run) != 1 || run[0].Account.UUID != "c" {
		t.Fatalf("--account: want [c], got %v", emails(run))
	}
	// A forced account outside the project's list is still the one that runs.
	run, _, _ = rank(v, p, st, pickOptions{Dir: "/work/proj", Only: "c@x.io", Ceiling: 90})
	if len(run) != 1 || run[0].Account.UUID != "c" {
		t.Fatalf("--account outside policy: want [c], got %v", emails(run))
	}

	// Cached numbers whose window already reset no longer count, so a recovered account runs even
	// when the monitor has not polled since.
	past := time.Now().Add(-time.Hour)
	st.Usage["a"] = Usage{Session: Gauge{Percent: 95, ResetsAt: &past}, FetchedAt: time.Now().Add(-8 * time.Hour)}
	st.Bench = map[string]benchEntry{}
	run, _, _ = rank(v, p, st, pickOptions{Dir: "/work/proj", Model: "opus", Ceiling: 90})
	if len(run) == 0 || run[0].Account.UUID != "a" {
		t.Fatalf("a's window reset an hour ago, it must run first again: got %v", emails(run))
	}
}

// A policy that names accounts must fail closed when they are gone, never widen to everyone.
func TestAccountOrderFailsClosed(t *testing.T) {
	v := testVault()
	p := prefs{Projects: []projectPolicy{{Path: "/p", Accounts: []string{"gone"}}}}
	keys, proj := p.accountOrder(v, "/p")
	if proj == nil || len(keys) != 0 {
		t.Fatalf("want no keys under a stale project policy, got %v", keys)
	}
}

func emails(cs []candidate) []string {
	out := []string{}
	for _, c := range cs {
		out = append(out, c.Account.Email)
	}
	return out
}

func TestAccountOrderFallsBackToEveryone(t *testing.T) {
	v := testVault()
	keys, _ := prefs{}.accountOrder(v, "/x")
	if len(keys) != 4 {
		t.Fatalf("no policy at all must mean every account, got %v", keys)
	}
	keys, _ = prefs{Order: []string{"b", "b", "a"}}.accountOrder(v, "/x")
	if len(keys) != 2 || keys[0] != "b" || keys[1] != "a" {
		t.Fatalf("duplicates must collapse, got %v", keys)
	}
}

func TestFleetLedgerRoundTripAndPruning(t *testing.T) {
	t.Setenv("VIBEMON_STATE", t.TempDir())
	old := time.Now().Add(-25 * time.Hour)
	st, err := updateFleet(func(st *fleetState) {
		st.Turns = append(st.Turns,
			turn{Account: "a", At: old, Outcome: "ok"},
			turn{Account: "a", At: time.Now(), Outcome: "limit"})
		st.Bench["a"] = benchEntry{Until: time.Now().Add(-time.Minute), Reason: "expired"}
		st.Bench["b"] = benchEntry{Until: time.Now().Add(time.Hour), Reason: "live"}
		st.Usage["a"] = Usage{Session: Gauge{Percent: 42}}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Turns) != 1 || st.turnsLastHour("a") != 1 {
		t.Errorf("day-old turns must be pruned: %+v", st.Turns)
	}
	if _, ok := st.Bench["a"]; ok {
		t.Error("an elapsed bench must be dropped")
	}
	again := readFleet()
	if _, ok := again.benched("b"); !ok {
		t.Error("a live bench must survive the round trip")
	}
	if again.Usage["a"].Session.Percent != 42 {
		t.Error("usage cache did not survive the round trip")
	}
}

func TestLockSerialisesAndReleases(t *testing.T) {
	t.Setenv("VIBEMON_STATE", t.TempDir())
	release, err := acquireLock("session-1")
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan struct{})
	go func() {
		r2, err := acquireLock("session-1")
		if err != nil {
			t.Error(err)
		}
		r2()
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("second holder got the lock while the first still held it")
	case <-time.After(150 * time.Millisecond):
	}
	release()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("second holder never got the lock after release")
	}
	if _, err := os.Stat(filepath.Join(stateDir(), "locks", "session-1")); err != nil {
		t.Error("lock file should persist between holders")
	}
}
