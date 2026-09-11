package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The bug this guards: every 4xx used to mean "needs re-authentication", so a throttle told the user
// to log in again on an account that was perfectly healthy — and cleared its usage in the process.
func TestRefreshTokenErrorClassification(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantReauth bool
		wantLimit  bool
	}{
		{"rejected grant", 400, `{"error":"invalid_grant"}`, true, false},
		{"unauthorized", 401, `{"error":"unauthorized"}`, true, false},
		{"forbidden", 403, `{"error":"forbidden"}`, true, false},
		{"throttled", 429, `{"error":{"type":"rate_limit_error"}}`, false, true},
		{"server fault", 503, `upstream unavailable`, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			old := tokenURL
			tokenURL = srv.URL
			defer func() { tokenURL = old }()

			_, err := refreshToken("some-refresh-token")
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := errors.Is(err, errNeedsReauth); got != tc.wantReauth {
				t.Errorf("needsReauth = %v, want %v (err: %v)", got, tc.wantReauth, err)
			}
			var rl *rateLimitError
			if got := errors.As(err, &rl); got != tc.wantLimit {
				t.Errorf("rateLimited = %v, want %v (err: %v)", got, tc.wantLimit, err)
			}
		})
	}
}

func TestRetryAfterParsing(t *testing.T) {
	mk := func(v string) *http.Response {
		r := &http.Response{Header: http.Header{}}
		if v != "" {
			r.Header.Set("Retry-After", v)
		}
		return r
	}
	if got := retryAfter(mk("")); got != 0 {
		t.Errorf("absent header: want 0, got %v", got)
	}
	if got := retryAfter(mk("120")); got != 2*time.Minute {
		t.Errorf("seconds form: want 2m, got %v", got)
	}
	if got := retryAfter(mk("nonsense")); got != 0 {
		t.Errorf("garbage: want 0, got %v", got)
	}
	// HTTP-date form, and a date already in the past must not yield a negative wait.
	future := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	if got := retryAfter(mk(future)); got <= 0 || got > 95*time.Second {
		t.Errorf("date form: want ~90s, got %v", got)
	}
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	if got := retryAfter(mk(past)); got != 0 {
		t.Errorf("past date: want 0, got %v", got)
	}
}

// Without backoff, a rate-limited account gets retried on every tick and the throttle never lifts.
func TestBackoffGrowsAndRespectsRetryAfter(t *testing.T) {
	m := &monitor{}

	first := m.penalise("acct", errors.New("boom"))
	if first != backoffBase {
		t.Errorf("first failure: want %v, got %v", backoffBase, first)
	}
	if second := m.penalise("acct", errors.New("boom")); second != 2*backoffBase {
		t.Errorf("second failure: want %v, got %v", 2*backoffBase, second)
	}

	// It must plateau rather than grow without bound.
	for range 20 {
		m.penalise("acct", errors.New("boom"))
	}
	if got := m.penalise("acct", errors.New("boom")); got != backoffCap {
		t.Errorf("saturated backoff: want cap %v, got %v", backoffCap, got)
	}

	// The server's own instruction wins over our guess.
	if got := m.penalise("other", &rateLimitError{RetryAfter: 45 * time.Second}); got != 45*time.Second {
		t.Errorf("Retry-After should win: want 45s, got %v", got)
	}
	if s, waiting := m.benched("other"); !waiting || s.reason != "rate limited" {
		t.Errorf("expected a rate-limited bench, got %+v waiting=%v", s, waiting)
	}
}

func TestBenchedExpires(t *testing.T) {
	m := &monitor{backoff: map[string]backoffState{
		"stale": {until: time.Now().Add(-time.Second), reason: "old"},
		"fresh": {until: time.Now().Add(time.Minute), reason: "rate limited"},
	}}
	if _, waiting := m.benched("stale"); waiting {
		t.Error("an elapsed backoff must not hold the account back")
	}
	if _, waiting := m.benched("fresh"); !waiting {
		t.Error("a live backoff must hold the account back")
	}
	if _, waiting := m.benched("unknown"); waiting {
		t.Error("an account that never failed must not be benched")
	}
}

// vibemon codex add writes the Codex order while the menu bar app is running; a tray toggle must
// not save its own stale copy back over it.
func TestMonitorPrefsKeepsOrdersWrittenBehindItsBack(t *testing.T) {
	t.Setenv("VIBEMON_STATE", t.TempDir())
	if err := savePrefs(prefs{Order: []string{"a"}, CodexOrder: []string{"codex:x@x.io"},
		Projects: []projectPolicy{{Path: "/p", Accounts: []string{"a"}}}}); err != nil {
		t.Fatal(err)
	}
	m := &monitor{density: densityFull, autoSwitch: true, preferred: "a"}
	p := m.prefs()
	if len(p.CodexOrder) != 1 || p.CodexOrder[0] != "codex:x@x.io" {
		t.Errorf("the codex order written by the CLI was lost: %+v", p.CodexOrder)
	}
	if len(p.Order) != 1 || len(p.Projects) != 1 {
		t.Errorf("order and projects must survive too: %+v", p)
	}
	if p.Density != densityFull || !p.AutoSwitch || p.Preferred != "a" {
		t.Errorf("the monitor's own preferences must win: %+v", p)
	}
}
