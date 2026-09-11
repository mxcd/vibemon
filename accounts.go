package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	ccService    = "Claude Code-credentials"
	vaultService = "vibemon-accounts"
	oauthKey     = "claudeAiOauth"
)

// Account kinds. A Claude account carries Anthropic credentials; a Codex account is a ChatGPT
// login that lives in its own CODEX_HOME and carries no secret here at all.
const (
	kindClaude = "claude"
	kindCodex  = "codex"
)

// OAuth mirrors the claudeAiOauth object inside Claude Code's keychain blob. Field names and
// casing must match exactly — Claude Code reads this back.
type OAuth struct {
	AccessToken           string   `json:"accessToken"`
	RefreshToken          string   `json:"refreshToken"`
	ExpiresAt             int64    `json:"expiresAt,omitempty"`
	RefreshTokenExpiresAt int64    `json:"refreshTokenExpiresAt,omitempty"`
	Scopes                []string `json:"scopes,omitempty"`
	SubscriptionType      string   `json:"subscriptionType,omitempty"`
	RateLimitTier         string   `json:"rateLimitTier,omitempty"`
}

func (o OAuth) expiresSoon() bool {
	if o.ExpiresAt == 0 {
		return true
	}
	return time.UnixMilli(o.ExpiresAt).Before(time.Now().Add(5 * time.Minute))
}

// Account is one stored identity: a Claude login or a ChatGPT (Codex) one.
type Account struct {
	UUID     string `json:"uuid"`
	Email    string `json:"email"`
	Label    string `json:"label"`
	OrgName  string `json:"orgName,omitempty"`
	Plan     string `json:"plan,omitempty"`
	RateTier string `json:"rateTier,omitempty"`
	SeatTier string `json:"seatTier,omitempty"`
	OAuth    OAuth  `json:"oauth"`
	// HeadlessToken is a `claude setup-token` token: inference-only, valid for a year, and what
	// `vibemon exec` hands to child processes. It cannot poll usage or profile (403), so an account
	// added by token alone is identified by the email the user typed.
	HeadlessToken string          `json:"headlessToken,omitempty"`
	UserID        string          `json:"userID,omitempty"`
	OAuthAccount  json.RawMessage `json:"oauthAccount,omitempty"`
	NeedsReauth   bool            `json:"needsReauth,omitempty"`
	CapturedAt    time.Time       `json:"capturedAt"`
	// Kind is empty on every entry stored before Codex support and reads as claude, so the vault
	// on disk did not change.
	Kind string `json:"kind,omitempty"`
}

func (a *Account) kind() string {
	if a.Kind == "" {
		return kindClaude
	}
	return a.Kind
}

// planLabel renders the subscription in the terms Anthropic bills in: a team seat tier when there
// is one, otherwise the rate limit multiplier that actually governs the numbers on screen.
func (a *Account) planLabel() string {
	if a.kind() == kindCodex {
		return "ChatGPT · " + a.Plan // the raw plan_type: pro, prolite, plus, team
	}
	if seat := strings.ToLower(a.SeatTier); seat != "" {
		switch seat {
		case "premium":
			return "Team · Premium seat"
		case "standard":
			return "Team · Standard seat"
		default:
			return "Team · " + seat + " seat"
		}
	}
	tier := strings.ToLower(a.RateTier)
	switch {
	case strings.Contains(tier, "max_20x"):
		return "Max 20×"
	case strings.Contains(tier, "max_5x"):
		return "Max 5×"
	case strings.Contains(tier, "pro"):
		return "Pro"
	case strings.Contains(tier, "zero"), strings.Contains(tier, "free"):
		return "Free"
	}
	// Unknown tier: fall back to the org type rather than invent a label.
	switch strings.ToLower(a.Plan) {
	case "claude_max":
		return "Max"
	case "claude_pro":
		return "Pro"
	case "claude_team":
		return "Team"
	case "claude_enterprise":
		return "Enterprise"
	}
	return a.Plan
}

type Vault map[string]*Account

func loadVault() (Vault, error) {
	raw, err := keychainRead(vaultService)
	if err != nil {
		// Only a missing item means "no vault yet". Any other failure (locked keychain, prompt
		// denied) must not read as empty: the next save would replace every stored account.
		if strings.Contains(err.Error(), "could not be found") {
			return Vault{}, nil
		}
		return nil, err
	}
	v := Vault{}
	if strings.TrimSpace(raw) == "" {
		return v, nil
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, fmt.Errorf("vault is corrupt (%w) — discard it with "+
			"`security delete-generic-password -s %s` and re-run `vibemon capture`", err, vaultService)
	}
	return v, nil
}

func saveVault(v Vault) error {
	blob, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return keychainWrite(vaultService, string(blob))
}

func (v Vault) sorted() []*Account {
	out := make([]*Account, 0, len(v))
	for _, a := range v {
		out = append(out, a)
	}
	// Kind first so Claude rows come before Codex ones and each kind forms a group in every list.
	sort.Slice(out, func(i, j int) bool {
		if out[i].kind() != out[j].kind() {
			return out[i].kind() < out[j].kind()
		}
		return out[i].Email < out[j].Email
	})
	return out
}

// --- Claude Code state -------------------------------------------------------

// readCCBlob returns Claude Code's keychain payload as raw keys so that everything we do not
// understand — most importantly the mcpOAuth map holding every MCP server token — survives a
// round trip untouched.
func readCCBlob() (map[string]json.RawMessage, error) {
	raw, err := keychainRead(ccService)
	if err != nil {
		return nil, fmt.Errorf("read Claude Code credentials: %w", err)
	}
	blob := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(raw), &blob); err != nil {
		return nil, fmt.Errorf("Claude Code credentials are not valid JSON: %w", err)
	}
	return blob, nil
}

func ccOAuth() (OAuth, error) {
	blob, err := readCCBlob()
	if err != nil {
		return OAuth{}, err
	}
	return oauthFromBlob(blob)
}

func oauthFromBlob(blob map[string]json.RawMessage) (OAuth, error) {
	raw, ok := blob[oauthKey]
	if !ok {
		return OAuth{}, fmt.Errorf("no %s in Claude Code credentials — is Claude Code logged in?", oauthKey)
	}
	var o OAuth
	if err := json.Unmarshal(raw, &o); err != nil {
		return OAuth{}, fmt.Errorf("decode %s: %w", oauthKey, err)
	}
	if o.AccessToken == "" {
		return OAuth{}, fmt.Errorf("%s carries no accessToken", oauthKey)
	}
	return o, nil
}

// swapOAuth replaces only the claudeAiOauth key and re-encodes. Every other key — mcpOAuth above
// all — is copied through as the exact bytes it arrived as. This is the destructive path: getting
// it wrong drops every MCP server token the user has authorised.
func swapOAuth(blob map[string]json.RawMessage, o OAuth) ([]byte, error) {
	encoded, err := json.Marshal(o)
	if err != nil {
		return nil, err
	}
	next := make(map[string]json.RawMessage, len(blob)+1)
	for k, v := range blob {
		next[k] = v
	}
	next[oauthKey] = encoded
	return json.Marshal(next)
}

func claudeJSONPath() string {
	return filepath.Join(os.Getenv("HOME"), ".claude.json")
}

// patchClaudeJSON rewrites only oauthAccount and userID, preserving every other key byte-for-byte.
// The file is re-read immediately before writing because live Claude Code sessions rewrite it
// constantly, and it is replaced atomically so a crash mid-write cannot truncate it.
func patchClaudeJSON(a *Account) error {
	path := claudeJSONPath()
	current, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // nothing to patch; Claude Code will recreate it
		}
		return err
	}
	doc := map[string]json.RawMessage{}
	if err := json.Unmarshal(current, &doc); err != nil {
		return fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	if len(a.OAuthAccount) > 0 {
		doc["oauthAccount"] = a.OAuthAccount
	}
	if a.UserID != "" {
		encoded, err := json.Marshal(a.UserID)
		if err != nil {
			return err
		}
		doc["userID"] = encoded
	}
	next, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".claude.json.vibemon-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(next); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func readClaudeJSONIdentity() (userID string, oauthAccount json.RawMessage) {
	current, err := os.ReadFile(claudeJSONPath())
	if err != nil {
		return "", nil
	}
	doc := map[string]json.RawMessage{}
	if json.Unmarshal(current, &doc) != nil {
		return "", nil
	}
	json.Unmarshal(doc["userID"], &userID)
	return userID, doc["oauthAccount"]
}

func claudeSessionsRunning() int {
	out, err := exec.Command("/usr/bin/pgrep", "-f", "claude").Output()
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// --- operations --------------------------------------------------------------

// errHeadlessOnly: the account was added by setup-token alone, so there is nothing to poll with.
// Capture a login for the same email and the numbers appear.
var errHeadlessOnly = errors.New("headless token only, no usage: capture a login for this email")

// findByEmail resolves the user-facing identifier used by every command that takes one. kind ""
// matches any kind but only when the email is unambiguous; the vault key itself always resolves.
func findByEmail(v Vault, kind, email string) (*Account, error) {
	if a, ok := v[email]; ok && (kind == "" || a.kind() == kind) {
		return a, nil
	}
	var found *Account
	for _, a := range v {
		if !strings.EqualFold(a.Email, email) || (kind != "" && a.kind() != kind) {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("%q is both a Claude and a ChatGPT account: say --kind claude or --kind codex", email)
		}
		found = a
	}
	if found == nil {
		return nil, fmt.Errorf("no stored account matching %q — run `vibemon list`", email)
	}
	return found, nil
}

// addHeadlessToken stores a setup-token on the account matching email, creating a token-only entry
// keyed by the email when no captured login exists yet.
func addHeadlessToken(v Vault, email, token string) (*Account, error) {
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, "sk-ant-oat") {
		return nil, fmt.Errorf("that does not look like a `claude setup-token` token (expected sk-ant-oat…)")
	}
	email = strings.TrimSpace(email)
	if !strings.Contains(email, "@") {
		return nil, fmt.Errorf("%q is not an email address", email)
	}
	a, err := findByEmail(v, kindClaude, email)
	if err != nil {
		key := strings.ToLower(email)
		a = &Account{UUID: key, Email: email, Label: email, CapturedAt: time.Now()}
		v[key] = a
	}
	a.HeadlessToken = token
	// A fresh token lifts any "token rejected" bench the old one earned.
	_, _ = updateFleet(func(st *fleetState) {
		if b, ok := st.Bench[a.UUID]; ok && strings.HasPrefix(b.Reason, "token rejected") {
			delete(st.Bench, a.UUID)
		}
	})
	return a, saveVault(v)
}

// capture files whatever account Claude Code is currently logged into away in the vault.
func capture() (*Account, error) {
	unlock := lockVault()
	defer unlock()
	o, err := ccOAuth()
	if err != nil {
		return nil, err
	}
	p, err := fetchProfile(o.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("identify current account: %w", err)
	}
	userID, oauthAccount := readClaudeJSONIdentity()
	label := p.Account.DisplayName
	if label == "" {
		label = p.Account.Email
	}
	a := &Account{
		UUID:         p.Account.UUID,
		Email:        p.Account.Email,
		Label:        label,
		OrgName:      p.Organization.Name,
		Plan:         p.Organization.OrganizationType,
		RateTier:     p.Organization.RateLimitTier,
		SeatTier:     p.Organization.SeatTier,
		OAuth:        o,
		UserID:       userID,
		OAuthAccount: oauthAccount,
		CapturedAt:   time.Now(),
	}
	v, err := loadVault()
	if err != nil {
		return nil, err
	}
	// A login capture for an email that was previously added by token alone: keep the token, drop
	// the stub so the account exists once, under its real UUID.
	for key, existing := range v {
		// A Codex account may share the email; it is a different login and must survive untouched.
		if existing.kind() == kindClaude && strings.EqualFold(existing.Email, a.Email) {
			a.HeadlessToken = existing.HeadlessToken
			if key != a.UUID {
				delete(v, key)
				migrateAccountKey(key, a.UUID)
			}
		}
	}
	v[a.UUID] = a
	if err := saveVault(v); err != nil {
		return nil, err
	}
	return a, nil
}

// activeUUID reports which stored account Claude Code is currently using. The access token is
// the exact match, but Claude Code refreshes its own tokens, so the vault's copy goes stale within
// hours; the identity ~/.claude.json records then says which account it is. Without this fallback
// the active account looked parked and got refreshed from here, which rule 3 forbids.
func activeUUID(v Vault) string {
	o, err := ccOAuth()
	if err != nil {
		return ""
	}
	for uuid, a := range v {
		if a.OAuth.AccessToken == o.AccessToken {
			return uuid
		}
	}
	_, raw := readClaudeJSONIdentity()
	var id struct {
		AccountUUID string `json:"accountUuid"`
	}
	if json.Unmarshal(raw, &id) == nil {
		if _, ok := v[id.AccountUUID]; ok {
			return id.AccountUUID
		}
	}
	return ""
}

// switchTo makes target the account Claude Code will use on its next start.
func switchTo(v Vault, uuid string) error {
	target, ok := v[uuid]
	if !ok {
		return fmt.Errorf("no stored account with uuid %s", uuid)
	}
	if target.kind() == kindCodex {
		return fmt.Errorf("%s is a ChatGPT account; Claude Code cannot log into it", target.Email)
	}
	if target.OAuth.AccessToken == "" {
		return fmt.Errorf("%s has a headless token only; Claude Code's own login needs a full-scope token, so run `claude auth login` and capture it", target.Email)
	}
	blob, err := readCCBlob()
	if err != nil {
		return err
	}
	original, err := keychainRead(ccService)
	if err != nil {
		return err
	}

	// Claude Code refreshes the outgoing account's tokens behind our back; re-capture them before
	// they are overwritten, or the vault keeps a refresh token that is already dead.
	if outgoing, err := oauthFromBlob(blob); err == nil {
		if id := activeUUID(v); id != "" && id != uuid {
			v[id].OAuth = outgoing
		}
	}

	next, err := swapOAuth(blob, target.OAuth)
	if err != nil {
		return err
	}
	if err := keychainWrite(ccService, string(next)); err != nil {
		// keychainWrite verifies before returning, so a failure here means the item may be in an
		// unknown state. Put back exactly what was there.
		if restoreErr := keychainWrite(ccService, original); restoreErr != nil {
			return fmt.Errorf("switch failed (%w) AND restore failed (%v) — run `claude /login`", err, restoreErr)
		}
		return fmt.Errorf("switch failed, previous credentials restored: %w", err)
	}
	if err := patchClaudeJSON(target); err != nil {
		return fmt.Errorf("credentials switched but ~/.claude.json patch failed: %w", err)
	}
	return saveVault(v)
}

// forget drops an account from the vault. Claude Code's own credentials are untouched: forgetting
// the account you are signed into only means vibemon stops tracking it, not that you are logged out.
func forget(v Vault, uuid string) error {
	if _, ok := v[uuid]; !ok {
		return fmt.Errorf("no stored account with uuid %s", uuid)
	}
	delete(v, uuid)
	if len(v) == 0 {
		// An empty map would round-trip as "{}", which loadVault reads back fine — but dropping the
		// item entirely leaves no stale secret sitting in the keychain.
		_ = exec.Command("/usr/bin/security", "delete-generic-password", "-s", vaultService).Run()
		return nil
	}
	return saveVault(v)
}

// usageFor fetches usage for one account, refreshing parked tokens as needed. The active account
// is never refreshed here — Claude Code owns those tokens and rotation could log the user out.
func usageFor(v Vault, a *Account, isActive bool) (Usage, error) {
	if a.kind() == kindCodex {
		return codexUsageFor(a)
	}
	if a.OAuth.AccessToken == "" {
		return Usage{}, errHeadlessOnly
	}
	token := a.OAuth.AccessToken
	if isActive {
		if o, err := ccOAuth(); err == nil {
			token = o.AccessToken
			a.OAuth = o
		}
	} else if a.OAuth.expiresSoon() {
		refreshed, err := refreshToken(a.OAuth.RefreshToken)
		if err != nil {
			// Only a rejected grant flips this on. A throttle or a network blip says nothing about
			// whether the account is still good, and must not clear a flag already raised.
			if errors.Is(err, errNeedsReauth) {
				a.NeedsReauth = true
			}
			return Usage{}, err
		}
		refreshed.Scopes = a.OAuth.Scopes
		refreshed.SubscriptionType = a.OAuth.SubscriptionType
		refreshed.RateLimitTier = a.OAuth.RateLimitTier
		a.OAuth = refreshed
		a.NeedsReauth = false
		token = refreshed.AccessToken
		if err := saveVault(v); err != nil {
			return Usage{}, err
		}
	}
	u, err := fetchUsage(token)
	if err != nil {
		if errors.Is(err, errNeedsReauth) {
			a.NeedsReauth = true
		}
		return Usage{}, err
	}
	a.NeedsReauth = false
	return u, nil
}
