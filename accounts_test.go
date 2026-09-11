package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

// A realistic Claude Code blob: the oauth object we replace, plus MCP server tokens that must not
// be touched, plus an unknown future key we have never seen.
const ccBlobFixture = `{
  "claudeAiOauth": {
    "accessToken": "OLD-ACCESS",
    "refreshToken": "OLD-REFRESH",
    "expiresAt": 1784944325443,
    "refreshTokenExpiresAt": 1787536325443,
    "scopes": ["user:inference", "user:profile"],
    "subscriptionType": "max",
    "rateLimitTier": "default_claude_max_20x"
  },
  "mcpOAuth": {
    "internal-docs|1d80f372de5ef0b9": {
      "serverName": "internal-docs",
      "serverUrl": "https://mcp.example.com",
      "accessToken": "MCP-TOKEN-A",
      "clientId": "client-a",
      "discoveryState": {"oauthMetadataFound": true}
    },
    "plugin:example:Tracker|0103109044858fae": {
      "serverName": "Ironclad",
      "accessToken": "MCP-TOKEN-B",
      "redirectUri": "http://localhost:1455/callback"
    }
  },
  "someFutureKey": {"nested": [1, 2, 3]}
}`

// The destructive path. Overwriting the blob wholesale would silently drop every MCP server token
// the user has authorised, and they would only find out next time a connector failed.
func TestSwapOAuthPreservesEverythingElse(t *testing.T) {
	var before map[string]json.RawMessage
	if err := json.Unmarshal([]byte(ccBlobFixture), &before); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}

	next, err := swapOAuth(before, OAuth{
		AccessToken:  "NEW-ACCESS",
		RefreshToken: "NEW-REFRESH",
		ExpiresAt:    1799999999999,
		Scopes:       []string{"user:inference"},
	})
	if err != nil {
		t.Fatalf("swapOAuth: %v", err)
	}

	var after map[string]json.RawMessage
	if err := json.Unmarshal(next, &after); err != nil {
		t.Fatalf("swapOAuth produced invalid JSON: %v", err)
	}

	if len(after) != len(before) {
		t.Fatalf("key count changed: %d -> %d", len(before), len(after))
	}
	// Re-encoding may reformat whitespace; what must not change is the data. Compare compacted.
	for k, want := range before {
		if k == oauthKey {
			continue
		}
		got, ok := after[k]
		if !ok {
			t.Fatalf("key %q was dropped", k)
		}
		var wantBuf, gotBuf bytes.Buffer
		if err := json.Compact(&wantBuf, want); err != nil {
			t.Fatalf("compact %q (before): %v", k, err)
		}
		if err := json.Compact(&gotBuf, got); err != nil {
			t.Fatalf("compact %q (after): %v", k, err)
		}
		if gotBuf.String() != wantBuf.String() {
			t.Errorf("key %q was altered:\n before: %s\n  after: %s", k, wantBuf.String(), gotBuf.String())
		}
	}

	// And the swap actually happened.
	o, err := oauthFromBlob(after)
	if err != nil {
		t.Fatalf("oauthFromBlob after swap: %v", err)
	}
	if o.AccessToken != "NEW-ACCESS" || o.RefreshToken != "NEW-REFRESH" {
		t.Errorf("oauth not swapped: got %+v", o)
	}

	// Spot-check that a nested MCP token survived verbatim, not just that the bytes matched.
	var mcp map[string]struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(after["mcpOAuth"], &mcp); err != nil {
		t.Fatalf("mcpOAuth unreadable after swap: %v", err)
	}
	if mcp["internal-docs|1d80f372de5ef0b9"].AccessToken != "MCP-TOKEN-A" {
		t.Error("MCP token A did not survive the swap")
	}
	if mcp["plugin:example:Tracker|0103109044858fae"].AccessToken != "MCP-TOKEN-B" {
		t.Error("MCP token B did not survive the swap")
	}
}

func TestOAuthFromBlobRejectsGarbage(t *testing.T) {
	for name, blob := range map[string]string{
		"missing key": `{"mcpOAuth":{}}`,
		"empty token": `{"claudeAiOauth":{"accessToken":"","refreshToken":"r"}}`,
		"wrong shape": `{"claudeAiOauth":"not-an-object"}`,
	} {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(blob), &m); err != nil {
			t.Fatalf("%s: bad fixture: %v", name, err)
		}
		if _, err := oauthFromBlob(m); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}
}

const usageFixture = `{
  "five_hour": {"utilization": 7.0, "resets_at": "2026-07-24T20:10:00.604452+00:00"},
  "seven_day": {"utilization": 25.0, "resets_at": "2026-07-28T15:00:00.604497+00:00"},
  "limits": [
    {"kind":"session","group":"session","percent":7,"severity":"normal","resets_at":"2026-07-24T20:10:00.604452+00:00","is_active":false},
    {"kind":"weekly_all","group":"weekly","percent":25,"severity":"normal","resets_at":"2026-07-28T15:00:00.604497+00:00","is_active":false},
    {"kind":"weekly_scoped","group":"weekly","percent":45,"severity":"warning","resets_at":"2026-07-28T15:00:00.604766+00:00","is_active":true,
     "scope":{"model":{"display_name":"Fable"}}}
  ]
}`

func TestNormalizePrefersLimits(t *testing.T) {
	var r usageResponse
	if err := json.Unmarshal([]byte(usageFixture), &r); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	u := r.normalize(time.Now())

	if u.Session.Percent != 7 {
		t.Errorf("session: want 7, got %v", u.Session.Percent)
	}
	if u.Weekly.Percent != 25 {
		t.Errorf("weekly: want 25, got %v", u.Weekly.Percent)
	}
	if u.Scoped == nil {
		t.Fatal("scoped limit was dropped")
	}
	if u.Scoped.Label != "Fable" || u.Scoped.Percent != 45 || u.Scoped.Severity != "warning" {
		t.Errorf("scoped: got %+v", *u.Scoped)
	}
	if u.Session.ResetsAt == nil || u.Weekly.ResetsAt == nil {
		t.Error("reset timestamps were not parsed")
	}
}

// Older or degraded responses may carry no limits array at all.
func TestNormalizeFallsBackToFlatWindows(t *testing.T) {
	var r usageResponse
	if err := json.Unmarshal([]byte(`{
	  "five_hour": {"utilization": 61.5, "resets_at": "2026-07-24T20:10:00Z"},
	  "seven_day": {"utilization": 88.0, "resets_at": "2026-07-28T15:00:00Z"},
	  "limits": []
	}`), &r); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	u := r.normalize(time.Now())
	if u.Session.Percent != 61.5 {
		t.Errorf("session fallback: want 61.5, got %v", u.Session.Percent)
	}
	if u.Weekly.Percent != 88 {
		t.Errorf("weekly fallback: want 88, got %v", u.Weekly.Percent)
	}
	if u.Scoped != nil {
		t.Errorf("no scoped limit expected, got %+v", *u.Scoped)
	}
	if u.Session.ResetsAt == nil {
		t.Error("session reset not parsed in fallback path")
	}
}

// When several scoped limits are present, the one actually in play wins over a higher idle one.
func TestNormalizePicksActiveScopedLimit(t *testing.T) {
	var r usageResponse
	if err := json.Unmarshal([]byte(`{"limits":[
	  {"kind":"weekly_scoped","group":"weekly","percent":90,"is_active":false,"scope":{"model":{"display_name":"Opus"}}},
	  {"kind":"weekly_scoped","group":"weekly","percent":45,"is_active":true,"scope":{"model":{"display_name":"Fable"}}}
	]}`), &r); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	u := r.normalize(time.Now())
	if u.Scoped == nil || u.Scoped.Label != "Fable" {
		t.Errorf("want the active scoped limit (Fable), got %+v", u.Scoped)
	}
}

func TestExpiresSoon(t *testing.T) {
	if !(OAuth{}).expiresSoon() {
		t.Error("a zero expiry must count as expiring")
	}
	if !(OAuth{ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}).expiresSoon() {
		t.Error("one minute out must count as expiring soon")
	}
	if (OAuth{ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}).expiresSoon() {
		t.Error("an hour out must not count as expiring soon")
	}
}

// An email can name both a Claude and a ChatGPT account; picking the wrong one would attach a
// setup-token to a login that cannot use it.
func TestFindByEmailAcrossKinds(t *testing.T) {
	v := Vault{
		"uuid-1":       {UUID: "uuid-1", Email: "a@x.io"},
		"codex:a@x.io": {UUID: "codex:a@x.io", Email: "a@x.io", Kind: kindCodex},
	}
	if _, err := findByEmail(v, "", "a@x.io"); err == nil {
		t.Error("an ambiguous email must be an error, not a coin flip")
	}
	a, err := findByEmail(v, kindCodex, "a@x.io")
	if err != nil || a.UUID != "codex:a@x.io" {
		t.Errorf("--kind codex must resolve the Codex entry, got %+v %v", a, err)
	}
	a, err = findByEmail(v, kindClaude, "A@X.IO")
	if err != nil || a.UUID != "uuid-1" {
		t.Errorf("--kind claude must resolve the Claude entry, got %+v %v", a, err)
	}
	if a, err := findByEmail(v, "", "codex:a@x.io"); err != nil || a.Kind != kindCodex {
		t.Errorf("the vault key itself must always resolve, got %+v %v", a, err)
	}

	// A token-only Claude account is keyed by its own email. That key must not win over the
	// ambiguity: `remove a@x.io` would silently forget one of the two.
	stub := Vault{
		"a@x.io":       {UUID: "a@x.io", Email: "a@x.io", HeadlessToken: "sk-ant-oat01-a"},
		"codex:a@x.io": {UUID: "codex:a@x.io", Email: "a@x.io", Kind: kindCodex},
	}
	if a, err := findByEmail(stub, "", "a@x.io"); err == nil {
		t.Errorf("the email is ambiguous and must be an error, got %+v", a)
	}
	if a, err := findByEmail(stub, kindClaude, "a@x.io"); err != nil || a.UUID != "a@x.io" {
		t.Errorf("--kind claude must still resolve the stub, got %+v %v", a, err)
	}
}

// The vault is shared with accounts stored before Codex existed; their JSON must not change.
func TestVaultRoundTripKeepsClaudeEntriesUnchanged(t *testing.T) {
	raw, err := json.Marshal(Vault{"uuid-1": {UUID: "uuid-1", Email: "a@x.io", OAuth: OAuth{AccessToken: "t"}}})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"kind"`)) {
		t.Errorf("a Claude entry gained a kind field on disk: %s", raw)
	}
}
