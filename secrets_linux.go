package main

import (
	"os"
	"path/filepath"
)

// On Linux there is no keychain to share with Claude Code: it keeps its login in a file, and the
// vault sits in the state dir beside prefs and the fleet ledger.
var secrets secretStore = fileSecrets{path: secretPath}

func secretPath(service string) string {
	if service == ccService {
		return claudeCredentialsPath()
	}
	return vaultPath()
}

func vaultPath() string { return filepath.Join(stateDir(), "vault.json") }

func vaultDiscardHint() string { return "`rm " + vaultPath() + "`" }

// Claude Code resolves both of its files against CLAUDE_CONFIG_DIR when it is set, so vibemon has to
// as well, or a switch would write one account's tokens and patch another file's identity.
// (Its undocumented CLAUDE_SECURESTORAGE_CONFIG_DIR, which moves only the credentials, is not
// followed.)

func claudeCredentialsPath() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, ".credentials.json")
	}
	return filepath.Join(os.Getenv("HOME"), ".claude", ".credentials.json")
}

func claudeJSONPath() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, ".claude.json")
	}
	return filepath.Join(os.Getenv("HOME"), ".claude.json")
}
