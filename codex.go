package main

import (
	"os"
	"path/filepath"
	"strings"
)

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
