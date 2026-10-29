package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// fakeJWT builds an unsigned token whose payload carries exp and a tag naming it in assertions.
func fakeJWT(tag string, exp time.Time) string {
	payload, _ := json.Marshal(map[string]any{"tag": tag, "exp": exp.Unix()})
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// writeCodexAuth writes an auth.json shaped like codex's own, plus fields vibemon does not know,
// which a refresh must carry through untouched.
func writeCodexAuth(t *testing.T, home, access string) string {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "auth.json")
	raw := `{
  "auth_mode": "chatgpt",
  "OPENAI_API_KEY": null,
  "tokens": {"id_token": "id-1", "access_token": "` + access + `", "refresh_token": "rt-1", "account_id": "acct-1", "future": {"x": [1, 2]}},
  "last_refresh": "2026-10-01T08:00:00.123456Z",
  "agent_identity": {"kind": "kept <as is> & whole"}
}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// tokenServer stands in for auth.openai.com: it answers every refresh with reply and counts calls.
func tokenServer(t *testing.T, status int, reply string) *int {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var grant map[string]string
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" ||
			json.NewDecoder(r.Body).Decode(&grant) != nil ||
			grant["client_id"] != codexClientID || grant["grant_type"] != "refresh_token" || grant["refresh_token"] == "" || len(grant) != 3 {
			t.Errorf("refresh request is not codex's: %s %s %v", r.Method, r.Header.Get("Content-Type"), grant)
		}
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	old := codexTokenURL
	codexTokenURL = srv.URL
	t.Cleanup(func() { codexTokenURL = old })
	return &calls
}

func TestCodexTokenStale(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		token string
		stale bool
	}{
		{"ten minutes left", fakeJWT("a", now.Add(10*time.Minute)), false},
		{"inside codex's five minute window", fakeJWT("a", now.Add(4*time.Minute)), true},
		{"expired", fakeJWT("a", now.Add(-time.Hour)), true},
		{"not a JWT: upstream's 401 decides", "opaque", false},
		{"no exp", "x." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"a"}`)) + ".y", false},
	}
	for _, tc := range cases {
		if got := codexTokenStale(tc.token, now); got != tc.stale {
			t.Errorf("%s: stale = %v, want %v", tc.name, got, tc.stale)
		}
	}
}

// The write-back is the risky half: codex reads this file on every start, and anything lost in it
// (the account id, a field a newer codex added) breaks the login for good.
func TestRefreshCodexAuthWritesLikeCodex(t *testing.T) {
	home := filepath.Join(t.TempDir(), "a@x.io")
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	stale := fakeJWT("old", now.Add(time.Minute))
	path := writeCodexAuth(t, home, stale)
	fresh := fakeJWT("new", now.Add(10*24*time.Hour))
	calls := tokenServer(t, http.StatusOK, `{"id_token":"id-2","access_token":"`+fresh+`","refresh_token":"rt-2"}`)

	a, err := refreshCodexAuth(home, stale, now)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 1 || a.Tokens.AccessToken != fresh || a.Tokens.AccountID != "acct-1" {
		t.Fatalf("calls %d, got %+v", *calls, a.Tokens)
	}
	var doc map[string]any
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	tok := doc["tokens"].(map[string]any)
	want := map[string]any{"id_token": "id-2", "access_token": fresh, "refresh_token": "rt-2", "account_id": "acct-1"}
	for k, v := range want {
		if tok[k] != v {
			t.Errorf("tokens.%s = %v, want %v", k, tok[k], v)
		}
	}
	if fmt.Sprint(tok["future"]) != "map[x:[1 2]]" || doc["auth_mode"] != "chatgpt" ||
		doc["OPENAI_API_KEY"] != nil || len(doc) != 5 ||
		fmt.Sprint(doc["agent_identity"]) != "map[kind:kept <as is> & whole]" {
		t.Errorf("a field vibemon does not own changed: %s", raw)
	}
	if doc["last_refresh"] != "2026-10-27T12:00:00Z" {
		t.Errorf("last_refresh = %v", doc["last_refresh"])
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("auth.json mode %v, want 0600", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(home)
	for _, e := range entries {
		if e.Name() != "auth.json" && e.Name() != codexRefreshLock {
			t.Errorf("left behind: %s", e.Name())
		}
	}

	// A refresh that does not rotate the refresh token, or sends no id_token, keeps the ones on disk.
	newer := fakeJWT("newer", now.Add(20*24*time.Hour))
	calls = tokenServer(t, http.StatusOK, `{"access_token":"`+newer+`"}`)
	if a, err = refreshCodexAuth(home, fresh, now); err != nil || a.Tokens.AccessToken != newer || a.Tokens.RefreshToken != "rt-2" {
		t.Fatalf("second refresh: %v %+v", err, a.Tokens)
	}
	raw, _ = os.ReadFile(path)
	if !strings.Contains(string(raw), `"id_token": "id-2"`) {
		t.Errorf("an id_token the endpoint did not send was dropped: %s", raw)
	}
}

// Two refreshers on one refresh token: the second would spend a rotation that is already gone and
// log the home out. Under the lock the file is read again, and a token that changed meanwhile wins.
func TestRefreshCodexAuthUsesWhatChangedUnderTheLock(t *testing.T) {
	home := filepath.Join(t.TempDir(), "a@x.io")
	now := time.Now()
	stale := fakeJWT("old", now.Add(time.Minute))
	writeCodexAuth(t, home, stale)
	calls := tokenServer(t, http.StatusOK, `{"access_token":"must-not-be-used"}`)

	release, err := lockFile(filepath.Join(home, codexRefreshLock))
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		a   codexAuth
		err error
	}
	done := make(chan result, 1)
	go func() {
		a, err := refreshCodexAuth(home, stale, now)
		done <- result{a, err}
	}()
	select {
	case <-done:
		t.Fatal("the refresh ran while another holder had the home's lock")
	case <-time.After(150 * time.Millisecond):
	}
	byCodex := fakeJWT("codex", now.Add(10*24*time.Hour))
	writeCodexAuth(t, home, byCodex)
	release()

	r := <-done
	if r.err != nil || r.a.Tokens.AccessToken != byCodex {
		t.Fatalf("want the token written meanwhile, got %v %q", r.err, r.a.Tokens.AccessToken)
	}
	if *calls != 0 {
		t.Errorf("the endpoint was called %d times although the file had already changed", *calls)
	}
}

// Rule 4 on the refresh: a rejected grant is a dead login, anything else is temporary, and neither
// touches the file.
func TestRefreshCodexAuthClassifiesFailures(t *testing.T) {
	cases := []struct {
		status int
		reauth bool
	}{
		{http.StatusBadRequest, true},
		{http.StatusUnauthorized, true},
		{http.StatusTooManyRequests, false},
		{http.StatusInternalServerError, false},
	}
	for _, tc := range cases {
		home := filepath.Join(t.TempDir(), "a@x.io")
		stale := fakeJWT("old", time.Now())
		path := writeCodexAuth(t, home, stale)
		before, _ := os.ReadFile(path)
		tokenServer(t, tc.status, `{"error":"refresh_token_reused"}`)
		_, err := refreshCodexAuth(home, stale, time.Now())
		if err == nil || errors.Is(err, errNeedsReauth) != tc.reauth {
			t.Errorf("HTTP %d: err %v, want needs re-auth %v", tc.status, err, tc.reauth)
		}
		if after, _ := os.ReadFile(path); string(after) != string(before) {
			t.Errorf("HTTP %d: a failed refresh rewrote auth.json", tc.status)
		}
	}
}

// ~/.codex is MaPa's own login: no code path may refresh it, whatever home it is handed.
func TestRefreshCodexAuthNeverTouchesTheOwnLogin(t *testing.T) {
	own := filepath.Join(sandbox(t), ".codex")
	stale := fakeJWT("own", time.Now())
	path := writeCodexAuth(t, own, stale)
	before, _ := os.ReadFile(path)
	calls := tokenServer(t, http.StatusOK, `{"access_token":"x"}`)
	if _, err := refreshCodexAuth(own+"/", stale, time.Now()); err == nil {
		t.Error("refreshing ~/.codex must be refused")
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) || *calls != 0 {
		t.Error("~/.codex was touched")
	}
}
