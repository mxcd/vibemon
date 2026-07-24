package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	oauthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	oauthBeta     = "oauth-2025-04-20"
	usageURL      = "https://api.anthropic.com/api/oauth/usage"
	profileURL    = "https://api.anthropic.com/api/oauth/profile"
	tokenURL      = "https://platform.claude.com/v1/oauth/token"
)

// errNeedsReauth means the refresh token is dead — only a fresh `claude /login` fixes it.
var errNeedsReauth = errors.New("account needs re-authentication")

var httpClient = &http.Client{Timeout: 20 * time.Second}

type apiWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

type apiLimit struct {
	Kind     string  `json:"kind"`
	Group    string  `json:"group"`
	Percent  float64 `json:"percent"`
	Severity string  `json:"severity"`
	ResetsAt string  `json:"resets_at"`
	IsActive bool    `json:"is_active"`
	Scope    *struct {
		Model *struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

type usageResponse struct {
	FiveHour *apiWindow `json:"five_hour"`
	SevenDay *apiWindow `json:"seven_day"`
	Limits   []apiLimit `json:"limits"`
}

// Gauge is one bar in the panel.
type Gauge struct {
	Label    string     `json:"label"`
	Percent  float64    `json:"percent"`
	Severity string     `json:"severity"`
	ResetsAt *time.Time `json:"resetsAt,omitempty"`
}

// Usage is the normalised, UI-facing view of one account's limits.
type Usage struct {
	Session   Gauge     `json:"session"`
	Weekly    Gauge     `json:"weekly"`
	Scoped    *Gauge    `json:"scoped,omitempty"`
	FetchedAt time.Time `json:"fetchedAt"`
}

func parseResetsAt(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return &t
}

// normalize prefers limits[] because it carries severity and per-model scoping, and falls back to
// the flat five_hour/seven_day windows when the array is absent or incomplete.
func (r *usageResponse) normalize(now time.Time) Usage {
	u := Usage{
		Session:   Gauge{Label: "Session", Severity: "normal"},
		Weekly:    Gauge{Label: "Weekly", Severity: "normal"},
		FetchedAt: now,
	}
	var haveSession, haveWeekly, scopedActive bool
	for i := range r.Limits {
		l := r.Limits[i]
		g := Gauge{Percent: l.Percent, Severity: l.Severity, ResetsAt: parseResetsAt(l.ResetsAt)}
		switch {
		case l.Group == "session":
			g.Label = "Session"
			u.Session, haveSession = g, true
		case l.Kind == "weekly_all":
			g.Label = "Weekly"
			u.Weekly, haveWeekly = g, true
		case l.Kind == "weekly_scoped":
			g.Label = "Weekly"
			if l.Scope != nil && l.Scope.Model != nil && l.Scope.Model.DisplayName != "" {
				g.Label = l.Scope.Model.DisplayName
			}
			// Several scoped limits can coexist; surface the one actually in play, else the worst.
			if u.Scoped == nil || (l.IsActive && !scopedActive) || (l.IsActive == scopedActive && g.Percent > u.Scoped.Percent) {
				scoped := g
				u.Scoped = &scoped
				scopedActive = l.IsActive
			}
		}
	}
	if !haveSession && r.FiveHour != nil {
		u.Session.Percent = r.FiveHour.Utilization
		u.Session.ResetsAt = parseResetsAt(r.FiveHour.ResetsAt)
	}
	if !haveWeekly && r.SevenDay != nil {
		u.Weekly.Percent = r.SevenDay.Utilization
		u.Weekly.ResetsAt = parseResetsAt(r.SevenDay.ResetsAt)
	}
	return u
}

func authGet(url, token string, into any) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", oauthBeta)
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%s: %w (HTTP %d)", url, errNeedsReauth, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d: %s", url, resp.StatusCode, truncate(string(body), 200))
	}
	return json.Unmarshal(body, into)
}

func fetchUsage(token string) (Usage, error) {
	var r usageResponse
	if err := authGet(usageURL, token, &r); err != nil {
		return Usage{}, err
	}
	return r.normalize(time.Now()), nil
}

// Profile identifies whoever owns a token — used to label captured accounts.
type Profile struct {
	Account struct {
		UUID        string `json:"uuid"`
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
	} `json:"account"`
	Organization struct {
		UUID             string `json:"uuid"`
		Name             string `json:"name"`
		OrganizationType string `json:"organization_type"`
		RateLimitTier    string `json:"rate_limit_tier"`
		SeatTier         string `json:"seat_tier"`
	} `json:"organization"`
}

func fetchProfile(token string) (Profile, error) {
	var p Profile
	err := authGet(profileURL, token, &p)
	return p, err
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

// refreshToken exchanges a refresh token for a fresh pair.
//
// Only ever called for *parked* accounts. The active account's tokens belong to Claude Code, which
// keeps them fresh itself; refreshing those from here risks tripping refresh-token rotation and
// logging the user out of their live session.
func refreshToken(refresh string) (OAuth, error) {
	payload, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refresh,
		"client_id":     oauthClientID,
	})
	if err != nil {
		return OAuth{}, err
	}
	req, err := http.NewRequest(http.MethodPost, tokenURL, bytes.NewReader(payload))
	if err != nil {
		return OAuth{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return OAuth{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return OAuth{}, err
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return OAuth{}, fmt.Errorf("refresh: %w (HTTP %d: %s)", errNeedsReauth, resp.StatusCode, truncate(string(body), 200))
	}
	if resp.StatusCode != http.StatusOK {
		return OAuth{}, fmt.Errorf("refresh: HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return OAuth{}, fmt.Errorf("refresh: decode response: %w", err)
	}
	if tr.AccessToken == "" {
		return OAuth{}, fmt.Errorf("refresh: response carried no access_token")
	}
	out := OAuth{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second).UnixMilli(),
	}
	if out.RefreshToken == "" {
		out.RefreshToken = refresh // server did not rotate; keep the one we have
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
