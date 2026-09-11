package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// A var so tests can point the usage fetch at a local server.
var codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

// Codex (ChatGPT) accounts. One CODEX_HOME per login: codex keeps auth.json, config.toml and its
// sessions there, so a directory is the whole identity. vibemon reads auth.json and never writes
// it — codex owns those tokens and refreshes them in place.

// codexHomesDir is where vibemon keeps the homes it manages. VIBEMON_CODEX_HOMES overrides it for
// tests, mirroring VIBEMON_STATE. The user's own ~/.codex is never touched.
func codexHomesDir() string {
	if d := os.Getenv("VIBEMON_CODEX_HOMES"); d != "" {
		return d
	}
	return filepath.Join(os.Getenv("HOME"), ".vibemon", "codex")
}

func codexHome(email string) string {
	return filepath.Join(codexHomesDir(), strings.ToLower(email))
}

// codexKey is the vault key of a Codex account. The prefix also tells the kind of a key whose
// account has already left the vault, which is what keeps account policies failing closed.
func codexKey(email string) string { return "codex:" + strings.ToLower(email) }

func keyKind(key string) string {
	if strings.HasPrefix(key, "codex:") {
		return kindCodex
	}
	return kindClaude
}

// codexLoggedIn reports whether a home has a login at all. Whether that login still works is a
// question only the usage endpoint answers.
func codexLoggedIn(home string) bool {
	_, err := os.Stat(filepath.Join(home, "auth.json"))
	return err == nil
}

// --- auth.json ---------------------------------------------------------------

// codexAuth is the part of a home's auth.json vibemon reads. Everything else in that file, the
// refresh token above all, is codex's business.
type codexAuth struct {
	Tokens struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
	} `json:"tokens"`
}

// readCodexAuth reads a home's auth.json. A missing file or an empty token is errNeedsReauth: the
// account exists but has no usable login.
func readCodexAuth(home string) (codexAuth, error) {
	var a codexAuth
	raw, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return a, fmt.Errorf("%s has no login: %w", home, errNeedsReauth)
		}
		return a, err
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("%s/auth.json is not valid JSON: %w", home, err)
	}
	if a.Tokens.AccessToken == "" {
		return a, fmt.Errorf("%s/auth.json carries no access token: %w", home, errNeedsReauth)
	}
	if a.Tokens.AccountID == "" {
		return a, fmt.Errorf("%s/auth.json carries no account_id", home)
	}
	return a, nil
}

// --- usage -------------------------------------------------------------------

type codexWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAfterSeconds  int64   `json:"reset_after_seconds"`
	ResetAt            int64   `json:"reset_at"` // unix seconds
}

type codexRateLimit struct {
	Allowed      bool         `json:"allowed"`
	LimitReached bool         `json:"limit_reached"`
	Primary      *codexWindow `json:"primary_window"`
	Secondary    *codexWindow `json:"secondary_window"`
}

type codexUsageResponse struct {
	Email      string         `json:"email"`
	PlanType   string         `json:"plan_type"`
	RateLimit  codexRateLimit `json:"rate_limit"`
	Additional []struct {
		LimitName string         `json:"limit_name"`
		RateLimit codexRateLimit `json:"rate_limit"`
	} `json:"additional_rate_limits"`
}

const weekSeconds = 7 * 24 * 60 * 60

func codexGauge(w *codexWindow, reached bool, now time.Time) Gauge {
	g := Gauge{Percent: w.UsedPercent, Severity: "normal"}
	if reached || w.UsedPercent >= 100 {
		g.Severity = "critical"
	}
	switch {
	case w.ResetAt > 0:
		t := time.Unix(w.ResetAt, 0)
		g.ResetsAt = &t
	case w.ResetAfterSeconds > 0:
		t := now.Add(time.Duration(w.ResetAfterSeconds) * time.Second)
		g.ResetsAt = &t
	}
	return g
}

// normalize maps the two windows by their length, never by their slot: the prolite account reports
// its weekly window as the primary one and no secondary at all, while a per-model limit reports a
// 5 h primary and a weekly secondary. Length is the only thing that says which is which.
func (r *codexUsageResponse) normalize(now time.Time) Usage {
	u := Usage{
		Session:   Gauge{Label: "Session", Severity: "normal"},
		Weekly:    Gauge{Label: "Weekly", Severity: "normal"},
		FetchedAt: now,
	}
	for _, w := range []*codexWindow{r.RateLimit.Primary, r.RateLimit.Secondary} {
		if w == nil {
			continue
		}
		g := codexGauge(w, r.RateLimit.LimitReached, now)
		if w.LimitWindowSeconds >= weekSeconds {
			g.Label = "Weekly"
			u.Weekly = g
		} else {
			g.Label = "Session"
			u.Session = g
		}
	}
	for _, a := range r.Additional {
		w := a.RateLimit.Primary
		if w == nil || (a.RateLimit.Secondary != nil && a.RateLimit.Secondary.UsedPercent > w.UsedPercent) {
			w = a.RateLimit.Secondary
		}
		if w == nil {
			continue
		}
		g := codexGauge(w, a.RateLimit.LimitReached, now)
		g.Label = a.LimitName
		u.Extra = append(u.Extra, g)
	}
	return u
}

// fetchCodexUsage asks the wham endpoint what one home's login has left. It never refreshes the
// access token: codex owns it and rotates it inside the same home, and two refreshers would race.
func fetchCodexUsage(home string) (codexUsageResponse, error) {
	var r codexUsageResponse
	auth, err := readCodexAuth(home)
	if err != nil {
		return r, err
	}
	req, err := http.NewRequest(http.MethodGet, codexUsageURL, nil)
	if err != nil {
		return r, err
	}
	req.Header.Set("Authorization", "Bearer "+auth.Tokens.AccessToken)
	req.Header.Set("ChatGPT-Account-Id", auth.Tokens.AccountID)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "vibemon")
	return r, doJSON(req, &r)
}

// codexUsageFor is usageFor for the codex kind. The plan label follows whatever the endpoint says
// today, and NeedsReauth flips on a rejected login only: a throttle or a network blip says nothing
// about whether the login is still good.
func codexUsageFor(a *Account) (Usage, error) {
	r, err := fetchCodexUsage(codexHome(a.Email))
	if err != nil {
		if errors.Is(err, errNeedsReauth) {
			a.NeedsReauth = true
		}
		return Usage{}, err
	}
	if r.PlanType != "" {
		a.Plan = r.PlanType
	}
	a.NeedsReauth = false
	return r.normalize(time.Now()), nil
}

// --- running a child ---------------------------------------------------------

// codexChildEnv hands the child one home. A CODEX_HOME inherited from the caller's shell would
// override ours, and OPENAI_API_KEY outranks the ChatGPT login: the run would bill the API instead
// of the plan and never show up in this account's numbers.
func codexChildEnv(home string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		switch strings.SplitN(kv, "=", 2)[0] {
		case "CODEX_HOME", "OPENAI_API_KEY":
			continue
		}
		env = append(env, kv)
	}
	return append(env, "CODEX_HOME="+home)
}

// --- registration ------------------------------------------------------------

// codexLogin runs `codex login` in home with stdio inherited: the browser flow needs a terminal to
// print its URL to and a human to finish it.
func codexLogin(home string) error {
	fmt.Fprintf(os.Stderr, "vibemon: running `codex login` in %s — finish it in the browser\n", home)
	cmd := exec.Command("codex", "login")
	cmd.Env = codexChildEnv(home)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("codex login in %s: %w", home, err)
	}
	if !codexLoggedIn(home) {
		return fmt.Errorf("codex login left no auth.json in %s", home)
	}
	return nil
}

// linkConfig shares the user's own codex settings (model, effort, trust levels) with a new home by
// symlink. A failure is a note, not a fault: the home works without it, on codex's defaults.
func linkConfig(home string) {
	src := filepath.Join(os.Getenv("HOME"), ".codex", "config.toml")
	dst := filepath.Join(home, "config.toml")
	if _, err := os.Stat(src); err != nil {
		return
	}
	if _, err := os.Lstat(dst); err == nil {
		return
	}
	if err := os.Symlink(src, dst); err != nil {
		fmt.Fprintf(os.Stderr, "vibemon: could not link %s into %s: %v\n", src, home, err)
	}
}

// registerCodex identifies the login in home through the usage endpoint and files it in the vault.
// The email is read back from the endpoint rather than typed, and the home must already sit where
// vibemon expects it: moving or symlinking a login the user set up elsewhere is not vibemon's call.
func registerCodex(v Vault, home string) (*Account, Usage, error) {
	r, err := fetchCodexUsage(home)
	if err != nil {
		if errors.Is(err, errNeedsReauth) {
			return nil, Usage{}, fmt.Errorf("%s has no usable login: %w", home, err)
		}
		return nil, Usage{}, err
	}
	if !strings.Contains(r.Email, "@") {
		return nil, Usage{}, fmt.Errorf("%s reported no email; cannot file it", home)
	}
	if want := codexHome(r.Email); cleanPath(home) != cleanPath(want) {
		return nil, Usage{}, fmt.Errorf("%s is logged in as %s, whose home belongs at %s", home, r.Email, want)
	}
	key := codexKey(r.Email)
	a := v[key]
	if a == nil {
		a = &Account{UUID: key, CapturedAt: time.Now()}
		v[key] = a
	}
	a.Kind, a.Email, a.Label, a.Plan, a.NeedsReauth = kindCodex, r.Email, r.Email, r.PlanType, false
	if err := saveVault(v); err != nil {
		return nil, Usage{}, err
	}
	appendCodexOrder(key)
	return a, r.normalize(time.Now()), nil
}

// appendCodexOrder puts a newly registered account at the end of the Codex priority list, so the
// order accounts were added in is the order exec fills them up in and picking works without a trip
// to the settings window. An account already in the list keeps its place.
func appendCodexOrder(key string) {
	p := loadPrefs()
	if slices.Contains(p.CodexOrder, key) {
		return
	}
	p.CodexOrder = append(p.CodexOrder, key)
	_ = savePrefs(p)
}
