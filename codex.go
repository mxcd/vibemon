package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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
