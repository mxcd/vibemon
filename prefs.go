package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// prefsPath is a plain file rather than a keychain item: none of this is secret, and a corrupt or
// missing prefs file must never be able to take the credential handling down with it.
func prefsPath() (string, error) {
	return filepath.Join(stateDir(), "prefs.json"), nil
}

// stateDir holds prefs, the fleet ledger and lock files. VIBEMON_STATE overrides it so tests and
// throwaway experiments never touch the real one.
func stateDir() string {
	if d := os.Getenv("VIBEMON_STATE"); d != "" {
		return d
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), "Library", "Application Support")
	}
	return filepath.Join(dir, "vibemon")
}

// projectPolicy pins a directory tree to an ordered list of accounts. exec walks the list and
// takes the first one with headroom, so order is priority, not a pool to spread across.
type projectPolicy struct {
	Path     string   `json:"path"`
	Accounts []string `json:"accounts"` // vault keys (account UUIDs, or the email for token-only entries)
}

type prefs struct {
	Density    density         `json:"density"`
	AutoSwitch bool            `json:"autoSwitch"`
	Preferred  string          `json:"preferred,omitempty"`  // account UUID to come home to
	Order      []string        `json:"order,omitempty"`      // default exec priority for Claude accounts
	CodexOrder []string        `json:"codexOrder,omitempty"` // the same, for ChatGPT (Codex) accounts
	Projects   []projectPolicy `json:"projects,omitempty"`
}

func loadPrefs() prefs {
	p := prefs{Density: densityExtended}
	path, err := prefsPath()
	if err != nil {
		return p
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return p
	}
	var stored prefs
	if json.Unmarshal(raw, &stored) != nil {
		return p
	}
	switch stored.Density {
	case densityCondensed, densityExtended, densityFull:
		p.Density = stored.Density
	}
	p.AutoSwitch = stored.AutoSwitch
	p.Preferred = stored.Preferred
	p.Order = stored.Order
	p.CodexOrder = stored.CodexOrder
	p.Projects = stored.Projects
	return p
}

func savePrefs(p prefs) error {
	path, err := prefsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	// exec reads this file while the settings page may be saving it; a half-written file would
	// load as "no policy" and run under any account.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// migrateAccountKey follows a vault re-key (a token-only stub captured as a real login) through
// every place the old key is referenced, so project policies and benches survive the capture.
func migrateAccountKey(old, next string) {
	if old == next {
		return
	}
	swap := func(list []string) {
		for i, k := range list {
			if k == old {
				list[i] = next
			}
		}
	}
	p := loadPrefs()
	swap(p.Order)
	swap(p.CodexOrder)
	for i := range p.Projects {
		swap(p.Projects[i].Accounts)
	}
	_ = savePrefs(p)
	_, _ = updateFleet(func(st *fleetState) {
		if u, ok := st.Usage[old]; ok {
			st.Usage[next] = u
			delete(st.Usage, old)
		}
		if b, ok := st.Bench[old]; ok {
			st.Bench[next] = b
			delete(st.Bench, old)
		}
		for i := range st.Turns {
			if st.Turns[i].Account == old {
				st.Turns[i].Account = next
			}
		}
	})
}

// projectFor picks the policy whose path is the longest prefix of dir, so a git worktree under a
// project directory inherits the project's accounts. Returns nil when nothing matches.
func (p prefs) projectFor(dir string) *projectPolicy {
	dir = cleanPath(dir)
	var best *projectPolicy
	for i := range p.Projects {
		root := cleanPath(p.Projects[i].Path)
		if root == "" {
			continue
		}
		if dir == root || strings.HasPrefix(dir, root+string(filepath.Separator)) {
			if best == nil || len(root) > len(cleanPath(best.Path)) {
				best = &p.Projects[i]
			}
		}
	}
	return best
}

func cleanPath(p string) string {
	if strings.HasPrefix(p, "~/") || p == "~" {
		p = filepath.Join(os.Getenv("HOME"), strings.TrimPrefix(p, "~"))
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return filepath.Clean(p)
}

// accountOrder is the priority list exec uses for dir and kind: the project's entries of that kind,
// else the global order of that kind, else every stored account of that kind alphabetically. An
// explicit list whose entries have all left the vault yields nothing: a policy that names accounts
// must never widen to "anyone" behind the user's back. A list that names none of the requested kind
// was written before that kind existed and excludes nothing, so the next source down applies.
func (p prefs) accountOrder(v Vault, dir, kind string) (keys []string, project *projectPolicy) {
	if kind == "" {
		kind = kindClaude
	}
	project = p.projectFor(dir)
	global := p.Order
	if kind == kindCodex {
		global = p.CodexOrder
	}
	src := ofKind(global, kind)
	if project != nil {
		if fromProject := ofKind(project.Accounts, kind); len(fromProject) > 0 {
			src = fromProject
		}
	}
	if len(src) == 0 {
		for _, a := range v.sorted() {
			if a.kind() == kind {
				keys = append(keys, a.UUID)
			}
		}
		return keys, project
	}
	seen := map[string]bool{}
	for _, k := range src {
		if a, ok := v[k]; ok && !seen[k] && a.kind() == kind {
			keys, seen[k] = append(keys, k), true
		}
	}
	return keys, project
}

func ofKind(keys []string, kind string) []string {
	var out []string
	for _, k := range keys {
		if keyKind(k) == kind {
			out = append(out, k)
		}
	}
	return out
}
