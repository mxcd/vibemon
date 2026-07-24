package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The real file is ~500KB across 178 project entries plus a pile of account-scoped caches. A switch
// must rewrite two identity keys and leave every one of those bytes alone — losing `projects` would
// wipe the user's per-directory Claude Code history.
func TestPatchClaudeJSONTouchesOnlyIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".claude.json")

	const before = `{
	  "oauthAccount": {"accountUuid": "OLD-UUID", "emailAddress": "old@example.com"},
	  "userID": "OLD-USER-ID",
	  "projects": {"/some/path": {"history": [1, 2, 3]}},
	  "numStartups": 412,
	  "mcpServers": {"foo": {"command": "bar"}}
	}`
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}

	acct := &Account{
		UserID:       "NEW-USER-ID",
		OAuthAccount: json.RawMessage(`{"accountUuid":"NEW-UUID","emailAddress":"new@example.com"}`),
	}
	if err := patchClaudeJSON(acct); err != nil {
		t.Fatalf("patchClaudeJSON: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("patched file is not valid JSON: %v", err)
	}

	var userID string
	if err := json.Unmarshal(got["userID"], &userID); err != nil || userID != "NEW-USER-ID" {
		t.Errorf("userID: want NEW-USER-ID, got %q (%v)", userID, err)
	}
	var oa struct {
		AccountUUID string `json:"accountUuid"`
	}
	if err := json.Unmarshal(got["oauthAccount"], &oa); err != nil || oa.AccountUUID != "NEW-UUID" {
		t.Errorf("oauthAccount: want NEW-UUID, got %q (%v)", oa.AccountUUID, err)
	}

	var original map[string]json.RawMessage
	if err := json.Unmarshal([]byte(before), &original); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"projects", "numStartups", "mcpServers"} {
		var want, have bytes.Buffer
		if err := json.Compact(&want, original[key]); err != nil {
			t.Fatal(err)
		}
		if err := json.Compact(&have, got[key]); err != nil {
			t.Fatalf("key %q missing after patch", key)
		}
		if want.String() != have.String() {
			t.Errorf("key %q was altered:\n before: %s\n  after: %s", key, want.String(), have.String())
		}
	}
	if len(got) != len(original) {
		t.Errorf("key count changed: %d -> %d", len(original), len(got))
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("permissions widened to %v — this file holds account identity", info.Mode().Perm())
	}
}

// A missing file is normal on a fresh machine; Claude Code recreates it on next start.
func TestPatchClaudeJSONMissingFileIsNotAnError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := patchClaudeJSON(&Account{UserID: "x"}); err != nil {
		t.Errorf("want nil for a missing file, got %v", err)
	}
}

// Never half-write a corrupt config: bail out and leave the original in place.
func TestPatchClaudeJSONRefusesCorruptFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := patchClaudeJSON(&Account{UserID: "x"}); err == nil {
		t.Fatal("want an error for a corrupt file, got nil")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "{not json" {
		t.Errorf("original file was modified: %q", raw)
	}
}
