package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every test runs in a throwaway home. TestMain points each variable a path resolver reads into one
// temp dir before the first test, and on macOS swaps the login keychain for files in it, so a test
// that sets nothing still cannot reach the real ~/.claude, ~/.claude.json, keychain or state, even
// with CLAUDE_CONFIG_DIR or XDG_CONFIG_HOME exported in the shell running `just check`. A test that
// writes any of those paths calls sandbox(t) for a home of its own.

var testRoot string

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "vibemon-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testRoot = root
	for k, v := range sandboxEnv(root) {
		os.Setenv(k, v)
	}
	if _, ok := secrets.(fileSecrets); !ok {
		secrets = fileSecrets{path: sandboxSecretPath}
	}
	code := m.Run()
	os.RemoveAll(root)
	os.Exit(code)
}

// sandboxEnv is every variable that moves a path vibemon reads or writes. A resolver that starts
// reading a new one adds it here, and itself to TestEveryPathResolvesInsideTheSandbox.
func sandboxEnv(home string) map[string]string {
	return map[string]string{
		"HOME":                home,
		"CLAUDE_CONFIG_DIR":   "",
		"XDG_CONFIG_HOME":     filepath.Join(home, ".config"),
		"VIBEMON_STATE":       filepath.Join(home, "state"),
		"VIBEMON_CODEX_HOMES": "",
	}
}

// sandbox gives the test a home of its own, inside t.TempDir().
func sandbox(t *testing.T) (home string) {
	t.Helper()
	home = t.TempDir()
	for k, v := range sandboxEnv(home) {
		t.Setenv(k, v)
	}
	return home
}

// sandboxSecretPath is the Linux layout, for the test run on macOS.
func sandboxSecretPath(service string) string {
	if service == ccService {
		return filepath.Join(os.Getenv("HOME"), ".claude", ".credentials.json")
	}
	return filepath.Join(stateDir(), "vault.json")
}

// The guard behind every other test: with nothing set, and with sandbox(t), each path vibemon
// resolves lies inside the sandbox, and the secret store is files there, never the keychain.
func TestEveryPathResolvesInsideTheSandbox(t *testing.T) {
	check := func(root string) {
		t.Helper()
		store, ok := secrets.(fileSecrets)
		if !ok {
			t.Fatalf("tests must never reach the keychain, secrets is %T", secrets)
		}
		prefs, _ := prefsPath()
		for name, p := range map[string]string{
			"credentials": store.path(ccService), "vault": store.path(vaultService),
			".claude.json": claudeJSONPath(), "state": stateDir(), "prefs": prefs,
			"fleet": fleetPath(), "history": historyPath(), "codex homes": codexHomesDir(),
		} {
			if rel, err := filepath.Rel(root, p); err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
				t.Errorf("%s resolves to %s, outside %s", name, p, root)
			}
		}
	}
	check(testRoot)
	check(sandbox(t))
}
