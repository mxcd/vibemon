package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Linux store is plain files, so these run on macOS too, where TestMain has put files laid out
// the Linux way in place of the keychain. secretFiles gives the test a fresh sandbox and says where
// its two secrets live; on Linux that also checks secretPath's layout.
func secretFiles(t *testing.T) (credentials, vault string) {
	t.Helper()
	home := sandbox(t)
	return filepath.Join(home, ".claude", ".credentials.json"), filepath.Join(home, "state", "vault.json")
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

// A reader that opened the file before a write must still read the whole old file: the write
// replaces it by rename and never truncates it in place, which is what Claude Code relies on too.
func TestFileSecretsReplaceByRename(t *testing.T) {
	_, vault := secretFiles(t)
	if err := secrets.write(vaultService, "one"); err != nil {
		t.Fatal(err)
	}
	if got := modeOf(t, vault); got != 0o600 {
		t.Errorf("new secret file: mode %o, want 600", got)
	}
	if got := modeOf(t, filepath.Dir(vault)); got != 0o700 {
		t.Errorf("its new directory: mode %o, want 700", got)
	}
	reader, err := os.Open(vault)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	if err := secrets.write(vaultService, "two, and longer"); err != nil {
		t.Fatal(err)
	}
	old := make([]byte, 64)
	n, _ := reader.Read(old)
	if string(old[:n]) != "one" {
		t.Errorf("an open reader saw %q: the file was rewritten in place", old[:n])
	}
	if got, _ := secrets.read(vaultService); got != "two, and longer" {
		t.Errorf("read back %q", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(vault))
	if len(entries) != 1 {
		t.Errorf("temp files left behind: %v", entries)
	}
}

// Claude Code writes its file 0600; if the user has a reason to set it otherwise, a switch must
// not quietly change it. The vault is ours and goes back to 0600 on every save, whatever it was.
func TestFileSecretsModes(t *testing.T) {
	credentials, vault := secretFiles(t)
	for _, p := range []string{credentials, vault} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(credentials, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(vault, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := secrets.write(ccService, `{"claudeAiOauth":{}}`); err != nil {
		t.Fatal(err)
	}
	if err := secrets.write(vaultService, `{}`); err != nil {
		t.Fatal(err)
	}
	if got := modeOf(t, credentials); got != 0o640 {
		t.Errorf("credentials mode %o after a write, want the 640 it had", got)
	}
	if got := modeOf(t, vault); got != 0o600 {
		t.Errorf("vault mode %o after a save, want 600", got)
	}
}

// Rule 2 on Linux: what is on disk after the write is what the caller asked for, or the write fails.
// The path moving under the read-back stands in for another writer landing in between.
func TestFileSecretsVerifyWhatTheyWrote(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "other"), []byte("someone else's"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	f := fileSecrets{path: func(string) string {
		calls++
		if calls == 1 {
			return filepath.Join(dir, "vault.json")
		}
		return filepath.Join(dir, "other")
	}}
	err := f.write(vaultService, "ours")
	if err == nil || !strings.Contains(err.Error(), "verification failed") {
		t.Errorf("a read-back that disagrees must fail the write, got %v", err)
	}
	if err := f.write(vaultService, ""); err == nil {
		t.Error("an empty secret must be refused")
	}
}

// switchFixture is a box signed in as a, with b parked: Claude Code's multi-line credentials file,
// a ~/.claude.json naming a, and a vault whose copy of a is stale (Claude Code refreshed it since),
// so the active account is found through ~/.claude.json, as in real use.
func switchFixture(t *testing.T) (credentials, vault, claudeJSON string, v Vault) {
	t.Helper()
	credentials, vault = secretFiles(t)
	claudeJSON = filepath.Join(os.Getenv("HOME"), ".claude.json")
	if err := os.MkdirAll(filepath.Dir(credentials), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentials, []byte(ccBlobFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	identity := `{"oauthAccount":{"accountUuid":"uuid-a","emailAddress":"a@x.io"},"userID":"user-a","projects":{"/p":{"x":1}}}`
	if err := os.WriteFile(claudeJSON, []byte(identity), 0o600); err != nil {
		t.Fatal(err)
	}
	v = Vault{
		"uuid-a": {UUID: "uuid-a", Email: "a@x.io", OAuth: OAuth{AccessToken: "STALE-A", RefreshToken: "STALE-A-R"}},
		"uuid-b": {UUID: "uuid-b", Email: "b@x.io", OAuth: OAuth{AccessToken: "B-ACCESS", RefreshToken: "B-REFRESH"},
			UserID: "user-b", OAuthAccount: json.RawMessage(`{"accountUuid":"uuid-b","emailAddress":"b@x.io"}`)},
	}
	if err := saveVault(v); err != nil {
		t.Fatal(err)
	}
	return credentials, vault, claudeJSON, v
}

// Rule 1 through the whole Linux path: a switch rewrites claudeAiOauth in .credentials.json and
// nothing else, keeps the file's mode, re-captures the outgoing account's refreshed tokens, patches
// ~/.claude.json, and the auth proxy's next read of the file carries the new account.
func TestFileSecretsSwitchKeepsMCPOAuth(t *testing.T) {
	credentials, vault, claudeJSON, v := switchFixture(t)
	if c, err := activeCreds(); err != nil || c.token != "OLD-ACCESS" || c.email != "a@x.io" {
		t.Fatalf("the auth proxy should read a's live token from the file: %+v, %v", c, err)
	}

	if err := switchTo(v, "uuid-b"); err != nil {
		t.Fatalf("switchTo: %v", err)
	}
	if c, _ := activeCreds(); c.token != "B-ACCESS" || c.email != "b@x.io" {
		t.Errorf("the auth proxy's next read must carry the switched-in account: %+v", c)
	}

	blob, err := readCCBlob()
	if err != nil {
		t.Fatal(err)
	}
	if o, _ := oauthFromBlob(blob); o.AccessToken != "B-ACCESS" || o.RefreshToken != "B-REFRESH" {
		t.Errorf("credentials not switched: %+v", o)
	}
	var before map[string]json.RawMessage
	_ = json.Unmarshal([]byte(ccBlobFixture), &before)
	for _, k := range []string{"mcpOAuth", "someFutureKey"} {
		var want, got bytes.Buffer
		_ = json.Compact(&want, before[k])
		_ = json.Compact(&got, blob[k])
		if got.String() != want.String() {
			t.Errorf("%s was altered:\n before: %s\n  after: %s", k, want.String(), got.String())
		}
	}
	if got := modeOf(t, credentials); got != 0o600 {
		t.Errorf("credentials mode %o, want 600", got)
	}

	userID, account := readClaudeJSONIdentity()
	if userID != "user-b" || !strings.Contains(string(account), "uuid-b") {
		t.Errorf("~/.claude.json identity not switched: %s %s", userID, account)
	}
	raw, _ := os.ReadFile(claudeJSON)
	if !strings.Contains(string(raw), `"projects":{"/p":{"x":1}}`) {
		t.Errorf("~/.claude.json lost its other keys: %s", raw)
	}

	stored, err := loadVault()
	if err != nil {
		t.Fatal(err)
	}
	if got := stored["uuid-a"].OAuth.AccessToken; got != "OLD-ACCESS" {
		t.Errorf("outgoing account's live tokens not re-captured: vault has %q", got)
	}
	if got := modeOf(t, vault); got != 0o600 {
		t.Errorf("vault mode %o, want 600", got)
	}
}

// Only "no file" is an empty vault. Anything else unreadable must stop the caller, or its next save
// would replace every stored account; and forgetting the last account leaves no file behind.
func TestFileSecretsVaultLifecycle(t *testing.T) {
	_, vault := secretFiles(t)
	v, err := loadVault()
	if err != nil || len(v) != 0 {
		t.Fatalf("no vault file must read as an empty vault, got %v, %v", v, err)
	}
	if err := saveVault(Vault{"x": {UUID: "x", Email: "x@x.io"}}); err != nil {
		t.Fatal(err)
	}
	v, err = loadVault()
	if err != nil || v["x"] == nil {
		t.Fatalf("saved vault did not load: %v, %v", v, err)
	}
	if err := forget(v, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(vault); !os.IsNotExist(err) {
		t.Errorf("forgetting the last account must remove the vault file, stat says %v", err)
	}

	if err := os.WriteFile(vault, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadVault(); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Errorf("a corrupt vault must be an error, got %v", err)
	}
	if err := os.Remove(vault); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(vault, 0o700); err != nil { // unreadable as a file, and root cannot read past this
		t.Fatal(err)
	}
	if _, err := loadVault(); err == nil {
		t.Error("an unreadable vault must be an error, not an empty vault")
	}
}

// failingWrites stands in for a credentials write that dies halfway: the next n writes of Claude
// Code's file leave garbage behind and fail.
type failingWrites struct {
	secretStore
	n int
}

func (f *failingWrites) write(service, secret string) error {
	if service == ccService && f.n > 0 {
		f.n--
		_ = f.secretStore.write(service, `{"claudeAiOauth":`)
		return errors.New("disk full")
	}
	return f.secretStore.write(service, secret)
}

// A switch whose write fails puts back the exact bytes Claude Code wrote, multi-line as they are,
// and touches neither ~/.claude.json nor the vault. When the restore fails too, it says to log in.
func TestFileSecretsFailedSwitchRestores(t *testing.T) {
	for _, tc := range []struct {
		failures int
		want     string
	}{{1, "previous credentials restored"}, {2, "AND restore failed"}} {
		credentials, vault, claudeJSON, v := switchFixture(t)
		identityBefore, _ := os.ReadFile(claudeJSON)
		vaultBefore, _ := os.ReadFile(vault)
		files := secrets
		secrets = &failingWrites{secretStore: files, n: tc.failures}

		err := switchTo(v, "uuid-b")
		secrets = files
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%d failing writes: want %q, got %v", tc.failures, tc.want, err)
		}
		if after, _ := os.ReadFile(claudeJSON); string(after) != string(identityBefore) {
			t.Errorf("%d failing writes: ~/.claude.json was patched for a switch that did not happen", tc.failures)
		}
		if after, _ := os.ReadFile(vault); string(after) != string(vaultBefore) {
			t.Errorf("%d failing writes: the vault was saved for a switch that did not happen", tc.failures)
		}
		if after, _ := os.ReadFile(credentials); tc.failures == 1 && string(after) != ccBlobFixture {
			t.Errorf("credentials not restored byte for byte:\n%s", after)
		}
	}
}

// Forgetting the last account must fail loudly when the vault cannot be deleted: the forgotten
// account's refresh token would still be on disk.
func TestFileSecretsForgetReportsAFailedDelete(t *testing.T) {
	_, vault := secretFiles(t)
	if err := os.MkdirAll(filepath.Join(vault, "in-the-way"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := forget(Vault{"x": {UUID: "x", Email: "x@x.io"}}, "x"); err == nil {
		t.Error("a vault that could not be deleted must be an error")
	}
}
