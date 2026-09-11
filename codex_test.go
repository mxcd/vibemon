package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexKeyAndHome(t *testing.T) {
	if got := codexKey("Max@X.io"); got != "codex:max@x.io" {
		t.Errorf("codexKey: want codex:max@x.io, got %q", got)
	}
	t.Setenv("VIBEMON_CODEX_HOMES", "/tmp/homes")
	if got := codexHome("Max@X.io"); got != "/tmp/homes/max@x.io" {
		t.Errorf("codexHome: want /tmp/homes/max@x.io, got %q", got)
	}
	if keyKind("codex:x@x.io") != kindCodex || keyKind("uuid-1") != kindClaude {
		t.Error("keyKind must read the kind off the vault key alone")
	}
	home := t.TempDir()
	if codexLoggedIn(home) {
		t.Error("an empty home is not logged in")
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !codexLoggedIn(home) {
		t.Error("a home with auth.json is logged in")
	}
}

// The live shape of a spent prolite account: the weekly window arrives in the primary slot and
// there is no secondary at all, which is why windows are mapped by length.
const codexFixtureWeeklySpent = `{
  "email": "spent@example.com", "plan_type": "prolite",
  "rate_limit": {"allowed": false, "limit_reached": true,
    "primary_window": {"used_percent": 100, "limit_window_seconds": 604800, "reset_after_seconds": 371000, "reset_at": 1789495170},
    "secondary_window": null},
  "additional_rate_limits": []
}`

// A Pro seat reports both: a 5 h primary and a weekly secondary.
const codexFixtureProBothWindows = `{
  "email": "pro@example.com", "plan_type": "pro",
  "rate_limit": {"allowed": true, "limit_reached": false,
    "primary_window": {"used_percent": 42, "limit_window_seconds": 18000, "reset_at": 1789150000},
    "secondary_window": {"used_percent": 17, "limit_window_seconds": 604800, "reset_at": 1789640000}}
}`

const codexFixtureAdditional = `{
  "email": "spent@example.com", "plan_type": "prolite",
  "rate_limit": {"allowed": false, "limit_reached": true,
    "primary_window": {"used_percent": 100, "limit_window_seconds": 604800, "reset_at": 1789495170},
    "secondary_window": null},
  "additional_rate_limits": [
    {"limit_name": "GPT-5.3-Codex-Spark", "rate_limit": {"allowed": true, "limit_reached": false,
      "primary_window": {"used_percent": 60, "limit_window_seconds": 18000, "reset_at": 1789160000},
      "secondary_window": {"used_percent": 12, "limit_window_seconds": 604800, "reset_at": 1789660000}}}
  ]
}`

const codexFixtureResetAfterOnly = `{
  "email": "fresh@example.com", "plan_type": "plus",
  "rate_limit": {"allowed": true, "limit_reached": false,
    "primary_window": {"used_percent": 5, "limit_window_seconds": 604800, "reset_after_seconds": 3600, "reset_at": 0}}
}`

func TestNormalizeCodexUsage(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.Local)
	at := func(unix int64) *time.Time { t := time.Unix(unix, 0); return &t }
	cases := []struct {
		name    string
		fixture string
		want    Usage
	}{
		{"weekly spent", codexFixtureWeeklySpent, Usage{
			Session: Gauge{Label: "Session", Severity: "normal"},
			Weekly:  Gauge{Label: "Weekly", Percent: 100, Severity: "critical", ResetsAt: at(1789495170)},
		}},
		{"pro, both windows", codexFixtureProBothWindows, Usage{
			Session: Gauge{Label: "Session", Percent: 42, Severity: "normal", ResetsAt: at(1789150000)},
			Weekly:  Gauge{Label: "Weekly", Percent: 17, Severity: "normal", ResetsAt: at(1789640000)},
		}},
		{"per-model limits", codexFixtureAdditional, Usage{
			Session: Gauge{Label: "Session", Severity: "normal"},
			Weekly:  Gauge{Label: "Weekly", Percent: 100, Severity: "critical", ResetsAt: at(1789495170)},
			Extra:   []Gauge{{Label: "GPT-5.3-Codex-Spark", Percent: 60, Severity: "normal", ResetsAt: at(1789160000)}},
		}},
		{"reset_after only", codexFixtureResetAfterOnly, Usage{
			Session: Gauge{Label: "Session", Severity: "normal"},
			Weekly:  Gauge{Label: "Weekly", Percent: 5, Severity: "normal", ResetsAt: at(now.Add(time.Hour).Unix())},
		}},
	}
	for _, tc := range cases {
		var r codexUsageResponse
		if err := json.Unmarshal([]byte(tc.fixture), &r); err != nil {
			t.Fatalf("%s: fixture: %v", tc.name, err)
		}
		got := r.normalize(now)
		got.FetchedAt = time.Time{}
		tc.want.FetchedAt = time.Time{}
		if gj, wj := mustJSON(t, got), mustJSON(t, tc.want); gj != wj {
			t.Errorf("%s: got %s, want %s", tc.name, gj, wj)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Rule 4 for the Codex path: only a rejected login means re-auth, a 429 means ask later.
func TestFetchCodexUsageClassifiesStatus(t *testing.T) {
	home := t.TempDir()
	if _, err := fetchCodexUsage(home); !errors.Is(err, errNeedsReauth) {
		t.Fatalf("a home without auth.json must be errNeedsReauth, got %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"),
		[]byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"tok","account_id":"acct"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var gotAuth, gotAccount string
	status, body := http.StatusOK, codexFixtureProBothWindows
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotAccount = r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-Id")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	defer srv.Close()
	old := codexUsageURL
	codexUsageURL = srv.URL
	defer func() { codexUsageURL = old }()

	r, err := fetchCodexUsage(home)
	if err != nil {
		t.Fatalf("200: %v", err)
	}
	if r.PlanType != "pro" || r.Email != "pro@example.com" {
		t.Errorf("200 body not decoded: %+v", r)
	}
	if gotAuth != "Bearer tok" || gotAccount != "acct" {
		t.Errorf("headers: got %q %q", gotAuth, gotAccount)
	}

	body = `{}`
	for _, status = range []int{http.StatusUnauthorized, http.StatusForbidden} {
		if _, err := fetchCodexUsage(home); !errors.Is(err, errNeedsReauth) {
			t.Errorf("HTTP %d must mean re-auth, got %v", status, err)
		}
	}
	status = http.StatusTooManyRequests
	var rl *rateLimitError
	if _, err := fetchCodexUsage(home); !errors.As(err, &rl) {
		t.Errorf("429 must be a rate limit, never a dead login, got %v", err)
	}
	status = http.StatusInternalServerError
	if _, err := fetchCodexUsage(home); err == nil || errors.Is(err, errNeedsReauth) {
		t.Errorf("a server fault says nothing about the login, got %v", err)
	}
}

// The typed email and the one the endpoint reports both become a path segment; "../../.codex"
// would join to the user's own interactive home, which vibemon must never touch.
func TestCodexEmailOK(t *testing.T) {
	for _, ok := range []string{"a@x.io", "Max.Partenfelder@aso.nexus", "a+b@x.io"} {
		if err := codexEmailOK(ok); err != nil {
			t.Errorf("%q must be accepted: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "nobody", "../../.codex", "../a@x.io", "a/b@x.io", `a\b@x.io`,
		"..@x.io", " a@x.io", "a@x.io "} {
		if err := codexEmailOK(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
	// The concrete escape the guard exists for.
	t.Setenv("VIBEMON_CODEX_HOMES", "/Users/someone/.vibemon/codex")
	if got := codexHome("../../.codex"); got != "/Users/someone/.codex" {
		t.Fatalf("the traversal this guards is gone, re-check codexHome: %s", got)
	}
}

func TestPlaceCodexHome(t *testing.T) {
	homes := t.TempDir()
	t.Setenv("VIBEMON_CODEX_HOMES", homes)
	taken := filepath.Join(homes, "taken@x.io")
	if err := os.MkdirAll(taken, 0o700); err != nil {
		t.Fatal(err)
	}
	named := filepath.Join(homes, "asked@x.io")
	if err := os.MkdirAll(named, 0o700); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(homes, ".login-1")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}

	// The account is who it said it was: stay put.
	if got, err := placeCodexHome(named, false, "asked@x.io", "asked@x.io"); err != nil || got != named {
		t.Errorf("a matching home must be kept: %q %v", got, err)
	}
	// The browser signed in as somebody else: the named home is not handed over.
	got, err := placeCodexHome(named, false, "other@x.io", "asked@x.io")
	if err == nil || got != "" {
		t.Errorf("a home the user named must never be renamed: %q %v", got, err)
	}
	if !strings.Contains(err.Error(), "other@x.io") || !strings.Contains(err.Error(), "asked@x.io") {
		t.Errorf("the error must name both accounts: %v", err)
	}
	if _, statErr := os.Stat(named); statErr != nil {
		t.Error("the named home must still be there")
	}
	// A temporary login home is moved into place, but never over an existing home.
	if got, err := placeCodexHome(tmp, true, "fresh@x.io", ""); err != nil || got != filepath.Join(homes, "fresh@x.io") {
		t.Errorf("a temporary home must move into place: %q %v", got, err)
	}
	if got, err := placeCodexHome(tmp, true, "taken@x.io", ""); err == nil || got != "" {
		t.Errorf("an existing home must not be overwritten: %q %v", got, err)
	}
	// An address that is not one cannot become a directory name.
	if _, err := placeCodexHome(tmp, true, "../../.codex", ""); err == nil {
		t.Error("a traversing address must be refused")
	}
}
