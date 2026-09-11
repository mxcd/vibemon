package main

import (
	"os"
	"path/filepath"
	"testing"
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
