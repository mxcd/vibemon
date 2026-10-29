package main

import (
	"path/filepath"
	"testing"
)

// Claude Code 2.1.312 on Linux: <config dir>/.credentials.json and <config dir or $HOME>/.claude.json,
// the config dir being CLAUDE_CONFIG_DIR or ~/.claude. Following one but not the other would switch
// the tokens of one setup and the identity of another.
func TestLinuxPathsFollowClaudeConfigDir(t *testing.T) {
	home := sandbox(t)
	state := filepath.Join(home, "state")
	cases := []struct{ got, want string }{
		{secretPath(ccService), filepath.Join(home, ".claude", ".credentials.json")},
		{claudeJSONPath(), filepath.Join(home, ".claude.json")},
		{secretPath(vaultService), filepath.Join(state, "vault.json")},
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "/srv/claude")
	cases = append(cases,
		struct{ got, want string }{secretPath(ccService), "/srv/claude/.credentials.json"},
		struct{ got, want string }{claudeJSONPath(), "/srv/claude/.claude.json"},
		struct{ got, want string }{secretPath(vaultService), filepath.Join(state, "vault.json")},
	)
	for i, c := range cases {
		if c.got != c.want {
			t.Errorf("case %d: %s, want %s", i, c.got, c.want)
		}
	}
}
