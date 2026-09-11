# Plan: Codex (ChatGPT) accounts beside Claude accounts

Task `aso/vibemon/codex-accounts`, branch `task/codex-accounts`, written 11.09.2026.

> **Review status.** The Codex adversarial review rounds the planner brief calls for were skipped
> on MaPa's instruction: the only Codex login on this machine is at 100 percent of its weekly
> window (the exact problem this task fixes). One self-review pass was done instead; its findings
> are folded in and listed under "Self-review" at the end. Fable and Opus review the
> implementation.

## Goal

vibemon tracks ChatGPT logins the way it tracks Claude accounts: one `CODEX_HOME` per login,
usage from the wham endpoint, `pick` and `exec --kind codex` rotate a single-turn `codex exec`
review across the Codex accounts with headroom, and `list`, `usage`, `pick --json`, the panel and
the settings window show both kinds. Riker only needs the `kind` field on `pick --json` rows and
the `vibemon exec --kind codex -- codex exec ...` form to switch its two call sites over.

## Scope

- Account kind on the vault entry, `claude` (default, on-disk unchanged) or `codex`.
- `codex.go`: home directory layout, `auth.json` reader, wham usage fetch and normalisation, the
  `codex login` wrapper, child environment.
- `limits.go`: the Codex limit and not-logged-in phrasing, the `try again at <date>` reset form.
- `exec.go`, `fleet.go`, `prefs.go`: `--kind`, kind-partitioned ranking and account order, no
  Claude flag surgery on a Codex argv, `CODEX_HOME` on the child.
- `main.go`: `vibemon codex add`, `remove --kind`, both kinds in `list`, `usage`, `pick`.
- `gui.go`, `frontend/index.html`, `frontend/settings.html`: Codex group, same numbers, polled on
  the same tick, same penalise path.
- Tests, `CLAUDE.md`, `README.md`.

## Non-goals

No Codex token refresh (codex refreshes inside its own home). No cross-account session resume (a
review is one turn; the whole command reruns under the next account). No API-key auth mode. No
changes to Riker's scripts or the bridge (follow-up `private/riker/codex-accounts-bridge`). No
per-model fallback for Codex (see risk 5). No moving of the existing `~/.codex` login.

## Verified facts (11.09.2026, codex-cli 0.153.4)

Read the brain note `codex-multi-account-headless-codex-home-isolation-and-the-wham-usage-endpoint`
first. Additions verified while planning:

1. **Live wham response for the prolite account.** `primary_window` is the *weekly* window
   (`limit_window_seconds: 604800`, `used_percent: 100`, `reset_at: 1789495170`) and
   `secondary_window` is `null`. The `additional_rate_limits[]` entry (`limit_name:
   "GPT-5.3-Codex-Spark"`) carries a primary of 18000 s and a secondary of 604800 s. So the slot
   does not tell you which window it is; the window length does. `reset_at` is unix seconds.
   Other fields present and ignored: `model_usage`, `credits`, `spend_control`,
   `rate_limit_upsell`, `code_review_rate_limit`.
2. **The real limit line** (Riker job `codex-image-style-learning`, 11.09.2026 04:08, exit 1, on
   stderr):
   `ERROR: You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at Sep 15th, 2026 7:59 PM.`
   The existing `reLimit` already classifies it as `outLimit` (`usage limit`), but `reReset` only
   accepts `resets ...`, so today the bench would fall back to one hour. Other phrasings in the
   binary: `You've hit your usage limit for <model>`, `You're out of credits.`, `Your workspace is
   out of credits.`, `Usage limit reached. You've reached your usage limit.`, `Try again later.`,
   `Try again at <time>`.
3. **A fresh, never logged in `CODEX_HOME`** makes `codex exec` print
   `ERROR: unexpected status 401 Unauthorized: Missing bearer or basic authentication in header`
   (plus `ERROR: Reconnecting... n/5`) and exit 1. `codex login status` prints `Not logged in`,
   exit 1. A logged-in home has `auth.json` (mode 0600) with `auth_mode`, `tokens.{access_token,
   account_id, id_token, refresh_token}`, `last_refresh`.
4. `codex exec -p` is `--profile`, not "print". `codex exec --json` prints JSONL events; Riker does
   not use it and reads the `-o FILE` answer instead.
5. `codex login --with-access-token` exists (stdin) but is a non-goal: the browser flow is the
   only path that yields a refresh token.

## Decisions

1. **Codex accounts live in the same vault (`vibemon-accounts`) as Claude accounts.** They carry
   no secret, but every consumer (`rank`, `list`, `usage`, prefs order, fleet benches, the panel)
   already walks one `Vault`; a second registry would have to be merged into each of them. The
   Claude Code keychain item is never touched by the Codex path; rules 1 to 3 in `CLAUDE.md` are
   unaffected by construction.
2. **The vault key of a Codex account is `codex:<lowercased email>`.** The field stays `UUID`
   (`json:"uuid"`) because that name is the vault key in every code path and on disk; renaming it
   is out of scope. The `codex:` prefix lets `accountOrder` tell the kind of a key whose account
   has left the vault, which keeps the fail-closed rule for policies intact (decision 8).
3. **The home directory is derived, not stored:** `codexHome(email)` is
   `~/.vibemon/codex/<lowercased email>`; `VIBEMON_CODEX_HOMES` overrides the parent for tests,
   mirroring `VIBEMON_STATE`. The existing `~/.codex` login is registered by a symlink at that
   path (`vibemon codex add --home ~/.codex` creates it); nothing is moved.
4. **Usage gauges map by window length.** A window with `limit_window_seconds >= 7 days` is
   `Weekly`, anything shorter is `Session`. The task text says primary is weekly and secondary is
   session; that holds for the prolite account only because it has no session window (fact 1). A
   Pro account is expected to report a 5 h primary and a weekly secondary, the shape the additional
   limit already shows. Severity is `critical` when `limit_reached` is true or the percent is
   100 or more, otherwise `normal`; the panel already colours by percent.
5. **Additional per-model limits go to a new `Usage.Extra []Gauge` (`json:"extra,omitempty"`),
   one gauge per entry, label `limit_name`, percent and reset from the worse of its two windows.**
   `spentGauge` ignores `Extra`; `usage` and `pick` print it. Backwards compatible in
   `fleet.json`.
6. **The monitor polls Codex accounts on every tick (3 min), like the active Claude account.**
   The parked cadence exists because each parked Claude poll may burn a token refresh; a Codex
   poll is a plain GET, and these numbers drive Riker's rotation. Failures go through the same
   `penalise` backoff, a 429 honours `Retry-After`.
7. **`pick` defaults to `--kind all`, `exec` defaults to `--kind claude`.** The bridge reads one
   `pick --json` and groups by `kind`; an `exec` needs one kind. `all` runs `rank` once per kind
   and concatenates, Claude first.
8. **Account order is partitioned by kind.** `accountOrder(v, dir, kind)` keeps the configured
   entries (project list, else global order) whose key is of the requested kind. If the list names
   at least one key of that kind, only those run (gone keys drop out, fail closed as today). If it
   names none of that kind, every stored account of that kind runs in email order: a policy written
   before Codex existed must not silently exclude every Codex account. Per-project Codex lists thus
   work through the existing settings window for free.
9. **`vibemon codex add` has three modes in one command.** Bare `vibemon codex add` logs a new
   account in (temporary home, renamed to the email the endpoint reports). `vibemon codex add
   <email>` ensures that account: if `~/.vibemon/codex/<email>/auth.json` exists and the endpoint
   answers 200, it registers without a browser round trip (MaPa's manual WIT login); otherwise it
   runs `codex login` in that home, then verifies the reported email matches. `vibemon codex add
   --home <dir>` registers an existing foreign home (`~/.codex`) by symlink. On a name collision in
   the bare mode the temporary home is deleted and the command says to use `codex add <email>`;
   nothing existing is overwritten.
10. **`vibemon remove <email>` forgets the vault entry only; the home stays.** Deleting the home
    would log the account out, and Claude's `remove` does not log out either. The message names
    the directory to delete by hand. When both a Claude and a Codex account share an email,
    `remove --kind codex <email>` disambiguates; without `--kind` an ambiguous email is an error
    that prints both forms.
11. **A Codex headless attempt reruns the whole command.** No `--session-id`, `--resume` or
    `--model` surgery (`buildArgs` is Claude-only), no session lock, no `outNoSession` path. The
    worktree lock stays: one turn per working directory is the existing contract.
12. **Child environment.** `CODEX_HOME=<home>` is set and any inherited `CODEX_HOME` and
    `OPENAI_API_KEY` are dropped, by the same reasoning as `ANTHROPIC_API_KEY` for Claude: an API
    key would bill usage-based instead of the plan and the run would not appear in the account's
    numbers.
13. **Naming.** `childEnv(token)` becomes `claudeChildEnv(token)`; `codexChildEnv(home)` is its
    twin; `(*Account).childEnv()` dispatches on kind. `findByEmail` gains a leading `kind`
    argument. `authGet` keeps its name and delegates its status handling to `doJSON`. No other
    renames.

## On-disk layout

```
~/.vibemon/codex/<email>/                CODEX_HOME of one ChatGPT login (created by vibemon codex add)
    auth.json                            written and refreshed by codex; vibemon reads, never writes
    config.toml -> ~/.codex/config.toml  symlink created at login time when the source exists
    sessions/, *.sqlite, ...             codex's own state, ignored
~/.vibemon/codex/max.partenfelder@aso.nexus -> ~/.codex   the existing login, registered by symlink
~/.vibemon/codex/.login-<pid>/           temporary home during a bare `codex add`, renamed on success
```

Vault entry (keychain item `vibemon-accounts`, JSON map by key), new fields in bold:

```json
"codex:max.partenfelder@wilde-it.com": {
  "uuid": "codex:max.partenfelder@wilde-it.com",
  "kind": "codex",
  "email": "max.partenfelder@wilde-it.com",
  "label": "max.partenfelder@wilde-it.com",
  "plan": "pro",
  "needsReauth": false,
  "capturedAt": "2026-09-11T12:40:00+02:00"
}
```

Existing Claude entries are byte-identical: `kind` is `omitempty` and empty reads as `claude`.
`oauth` is present on Claude entries only. `fleet.json` and `prefs.json` reference Codex accounts
by the same key; `Usage.Extra` is the only new shape there.

## Code changes by file

### `accounts.go`

```go
const (
	kindClaude = "claude"
	kindCodex  = "codex"
)

type Account struct {
	// ... existing fields unchanged ...
	// Kind is empty on every entry stored before Codex support and reads as claude, so the vault
	// on disk did not change.
	Kind string `json:"kind,omitempty"`
}

func (a *Account) kind() string        // "" -> kindClaude
```

- `planLabel()`: first line `if a.kind() == kindCodex { return "ChatGPT · " + a.Plan }` (Plan is
  the raw `plan_type`: `prolite`, `pro`, `plus`, `team`, ...).
- `(v Vault) sorted()`: order by `kind()` then `Email`, so Claude rows come first and Codex rows
  form a group in every list.
- `findByEmail(v Vault, kind, email string) (*Account, error)`: `kind == ""` matches any kind but
  must be unique; the ambiguity error reads `"%q is both a Claude and a Codex account: say --kind
  claude or --kind codex"`. Also matches when `email` equals the vault key exactly. Callers:
  `cmdSwitch`, `cmdToken`, `addHeadlessToken` pass `kindClaude`; `cmdRemove` passes its `--kind`
  (default `""`); `rank` passes `o.Kind`.
- `usageFor(v, a, isActive)`: first statement `if a.kind() == kindCodex { return codexUsageFor(a) }`.
- `switchTo`: refuse a Codex key with `"%s is a ChatGPT account; Claude Code cannot log into it"`
  (the existing headless-only refusal sits right there).
- `forget`: unchanged (works on any key).
- `capture()`: the loop that folds a same-email token stub into the captured login must skip
  entries whose `kind()` is `kindCodex`. Today it deletes every same-email entry, which would
  silently drop a Codex account whose email matches the Claude login.

### `api.go`

Split `authGet` so the status classification is shared:

```go
// doJSON sends req and decodes a 200 body into into. 401/403 are errNeedsReauth, 429 is a
// rateLimitError carrying Retry-After, anything else non-200 is a plain error with the body head.
func doJSON(req *http.Request, into any) error
func authGet(url, token string, into any) error   // builds the Anthropic request, calls doJSON
```

`Usage` gets `Extra []Gauge \`json:"extra,omitempty"\`` with the comment: per-model limits that
never gate picking; shown, not acted on.

### `codex.go` (new)

```go
const codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

func codexHomesDir() string                 // $VIBEMON_CODEX_HOMES, else ~/.vibemon/codex
func codexHome(email string) string         // codexHomesDir()/<strings.ToLower(email)>
func codexKey(email string) string          // "codex:" + strings.ToLower(email)
func codexLoggedIn(home string) bool        // auth.json exists in home

type codexAuth struct {
	Tokens struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
	} `json:"tokens"`
}

// readCodexAuth reads auth.json. A missing file or empty token is errNeedsReauth: the account
// exists but has no login.
func readCodexAuth(home string) (codexAuth, error)

type codexWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAfterSeconds  int64   `json:"reset_after_seconds"`
	ResetAt            int64   `json:"reset_at"` // unix seconds
}

type codexRateLimit struct {
	Allowed      bool         `json:"allowed"`
	LimitReached bool         `json:"limit_reached"`
	Primary      *codexWindow `json:"primary_window"`
	Secondary    *codexWindow `json:"secondary_window"`
}

type codexUsageResponse struct {
	Email     string         `json:"email"`
	PlanType  string         `json:"plan_type"`
	RateLimit codexRateLimit `json:"rate_limit"`
	Additional []struct {
		LimitName string         `json:"limit_name"`
		RateLimit codexRateLimit `json:"rate_limit"`
	} `json:"additional_rate_limits"`
}

// normalize maps windows by length, not slot (see plan decision 4).
func (r *codexUsageResponse) normalize(now time.Time) Usage

// fetchCodexUsage reads the home's auth.json and asks the wham endpoint. It never refreshes:
// codex owns that token and rotates it inside the same home.
func fetchCodexUsage(home string) (codexUsageResponse, error)

// codexUsageFor is usageFor for the codex kind: fetch, keep Plan current, set or clear
// NeedsReauth with the same rule as Claude (only errNeedsReauth sets it, only success clears it).
func codexUsageFor(a *Account) (Usage, error)

// codexChildEnv hands the child one home. CODEX_HOME from the caller's shell and OPENAI_API_KEY
// go: an API key outranks the ChatGPT login and bills the API instead of the plan.
func codexChildEnv(home string) []string

// codexLogin runs `codex login` in the foreground with stdio inherited; the browser flow needs it.
func codexLogin(home string) error

// registerCodex identifies the login in home through the usage endpoint and files it in the vault
// under codexKey(email). A home outside codexHomesDir() is linked in as codexHome(email); a home
// already at that path is used as is. Returns the account and the usage just fetched.
func registerCodex(v Vault, home string) (*Account, Usage, error)
```

Request headers: `Authorization: Bearer <access_token>`, `ChatGPT-Account-Id: <account_id>`,
`Accept: application/json`, `User-Agent: vibemon`. Missing `account_id` is an error, not a
request without the header.

`normalize` in detail:

```
u := Usage{Session: {Label "Session", Severity "normal"}, Weekly: {Label "Weekly", Severity "normal"}, FetchedAt: now}
for each window w in [Primary, Secondary] that is non-nil:
    g := gauge(w, r.RateLimit.LimitReached, now)
    if w.LimitWindowSeconds >= 7*24*3600 { u.Weekly = g (label Weekly) } else { u.Session = g (label Session) }
for each a in r.Additional:
    pick the window with the higher UsedPercent among a.RateLimit.{Primary,Secondary}; skip when both nil
    u.Extra = append(u.Extra, gauge(that window, a.RateLimit.LimitReached, now) with Label a.LimitName)

gauge(w, reached, now):
    Percent  = w.UsedPercent
    Severity = "critical" if reached || w.UsedPercent >= 100, else "normal"
    ResetsAt = time.Unix(w.ResetAt, 0) when w.ResetAt > 0,
               else now.Add(w.ResetAfterSeconds * time.Second) when w.ResetAfterSeconds > 0,
               else nil
```

Two windows of the same class (both short or both weekly) are not expected; if it happens the
later one wins and the case is covered by a test only for the documented shapes.

`registerCodex` flow:

1. `readCodexAuth(home)`; error out with `"<home> has no login: run vibemon codex add"` on
   `errNeedsReauth`.
2. `fetchCodexUsage(home)`; a 401/403 is `"<home>'s login is dead: run vibemon codex add <email>"`
   when the email is known, else the bare `codex add` hint. A 429 is returned as is (try later).
3. `email := r.Email` (must contain `@`), `key := codexKey(email)`, `want := codexHome(email)`.
4. If `cleanPath(home) != cleanPath(want)`: if `want` exists and is not a symlink to `home`,
   error `"%s already exists; remove it or use vibemon codex add %s"`; else `os.MkdirAll(parent)`,
   `os.Symlink(home, want)`.
5. Upsert `v[key] = &Account{UUID: key, Kind: kindCodex, Email: email, Label: email, Plan:
   r.PlanType, CapturedAt: now}` keeping an existing entry's `CapturedAt`; `NeedsReauth = false`.
6. `saveVault(v)`; return with `r.normalize(now)` so the caller can print the numbers and cache them.

### `limits.go`

- `reReset`: prefix becomes `(?:resets?\s+(?:at\s+)?|try again (?:at|on)\s+)`. The rest of the
  pattern already parses `Sep 15th, 2026 7:59 PM` (layout `Jan 2 2006 3:04 PM` after ordinal and
  punctuation stripping). A trailing period after `PM` is not captured.
- `reLimit`: append `|out of credits`. (`usage limit` is already there; `Usage limit reached.`
  matches case-insensitively.)
- `reAuth`: append `|401 unauthorized|missing bearer|not logged in`.
- Comment on `classify` gains one sentence: Codex prints its limit on stderr with exit 1 and names
  the reset as `try again at <date>`.
- `Try again later.` without a time stays `outLimit` with the one-hour fallback bench; the
  `Reconnecting... n/5` lines are codex's own retries and match nothing.

### `prefs.go`

```go
// accountOrder is the priority list exec uses for dir and kind: the configured entries of that
// kind (project's, else global), else every stored account of that kind alphabetically. A list
// that names accounts of a kind must never widen to "anyone" of that kind behind the user's back;
// a list that names none of that kind was written before that kind existed and excludes nothing.
func (p prefs) accountOrder(v Vault, dir, kind string) (keys []string, project *projectPolicy)
```

Kind of a configured key: `strings.HasPrefix(k, "codex:")` is codex, anything else claude. Helper
`keyKind(k string) string` in `codex.go` next to `codexKey`. `migrateAccountKey` unchanged.

### `fleet.go`

- `pickOptions.Kind string` (comment: `kindClaude` or `kindCodex`; `rank` handles exactly one).
- `rank`: `keys, project := p.accountOrder(v, o.Dir, o.Kind)`; the forced `--account` path uses
  `findByEmail(v, o.Kind, o.Only)`; the runnable switch gains the kind split:

```go
case a.kind() == kindCodex && !codexLoggedIn(codexHome(a.Email)):
	c.Reason = "not logged in (vibemon codex add " + a.Email + ")"
case a.kind() == kindClaude && a.HeadlessToken == "":
	c.Reason = "no headless token (vibemon add-token)"
```

`spentGauge`, `earliestReturn`, benches, turns: unchanged.

### `exec.go`

- `wrapperFlags["kind"] = true`; `fs.StringVar(&o.Kind, "kind", "", "claude (default for exec) or codex")`.
  `cmdExec` resolves `""` to `kindClaude`; `cmdPick` resolves `""` to `"all"`. Any other value is
  an error naming the three accepted ones.
- Default command when none or only flags are given: `claude` for `kindClaude`, `codex` for
  `kindCodex`.
- `isHeadless(kind string, cmd []string) bool`: Claude as today (`-p`/`--print`); Codex when
  `slices.Contains(cmd[1:], "exec")` (`codex exec`, `codex exec resume`, `codex exec review`).
  `codex exec -p` is a profile and must not be read as print.
- `hasPositionalPrompt(kind string, cmd []string) bool`: Claude as today with
  `claudeValueFlags`; Codex walks `cmd[1:]`, skips the `exec`/`resume`/`fork`/`review`
  subcommand words and the flags in `codexValueFlags`, and reports any other positional:

```go
var codexValueFlags = map[string]bool{
	"-m": true, "--model": true, "-c": true, "--config": true, "-s": true, "--sandbox": true,
	"-p": true, "--profile": true, "-o": true, "--output-last-message": true, "-C": true, "--cd": true,
	"-i": true, "--image": true, "--add-dir": true, "--output-schema": true, "--enable": true,
	"--disable": true, "--local-provider": true, "--thread-source": true, "--color": true,
}
```

  Same "incomplete on purpose" caveat as the Claude table: a missed value flag means one extra
  stdin read, never a hang, because stdin is only read when it is not a character device.
- `claudeChildEnv(token string) []string` (renamed from `childEnv`), `codexChildEnv(home)` in
  `codex.go`, and

```go
// childEnv is what a child process gets to run as this account: a headless token for Claude, a
// home directory for Codex. Nothing else about the account leaves the process.
func (a *Account) childEnv() []string
```

- `runInteractive`: `cmd.Env = c.Account.childEnv()`; the `--model` append only for Claude.
- `runHeadless`:
  - stdin: `if !hasPositionalPrompt(kind, o.Command)` as today.
  - session: for Codex `sid` stays `""` and the session lock is skipped
    (`if sid != "" { acquire }`); `--session` with `--kind codex` is an error in `parseExecArgs`.
  - `model`: for Codex always `""` (`--model` is an error with `--kind codex`; codex takes `-m`
    inside its own argv).
  - `argv := o.Command; if kind == kindClaude { argv = buildArgs(o.Command, sid, resume, model) }`.
  - `cmd.Env = c.Account.childEnv()`.
  - outcome handling unchanged; `outModelCap` cannot arise for Codex because `reReached` only
    yields it for a Claude model name; `outNoSession` cannot arise because nothing resumes.
  - the `outAuth` bench reason is kind-aware: `token rejected: replace with vibemon add-token`
    for Claude, `login rejected: vibemon codex add <email>` for Codex (MaPa reads these on the
    bridge).
  - `emit` and `reportNoHeadroom` gain `"kind": kind` in the JSON; the existing fields keep their
    names (`session` is `""` for Codex).
- `execUsage()`: add the `--kind` line and one paragraph:
  `With --kind codex, runs codex under the Codex account with the most headroom; "codex exec" is
  the headless form and reruns the whole command under the next account on a limit.`

### `main.go`

- `case "codex":` dispatches `os.Args[2]`: `add` to `cmdCodexAdd(os.Args[3:])`; anything else
  prints `usage: vibemon codex add [<email> | --home <dir>]`.
- `cmdCodexAdd(args []string) error` implements decision 9:

```
parse: at most one positional (email) or --home <dir>, not both
lockVault; loadVault
--home <dir>:      a, u, err := registerCodex(v, dir)
<email>:           home := codexHome(email); if !codexLoggedIn(home) || probe(home) is errNeedsReauth:
                       os.MkdirAll(home, 0o700); linkConfig(home); codexLogin(home)
                   a, u, err := registerCodex(v, home); if !EqualFold(a.Email, email): error
                       "logged in as %s, not %s; registered under the real email"  (still registered)
bare:              tmp := codexHomesDir()/.login-<pid>; MkdirAll 0o700; linkConfig(tmp); codexLogin(tmp)
                   r := fetchCodexUsage(tmp); want := codexHome(r.Email)
                   if want exists: RemoveAll(tmp); error "%s is already registered; run vibemon codex add %s to log it in again"
                   os.Rename(tmp, want); a, u, err := registerCodex(v, want)
print "added <email> (ChatGPT <plan>)  session NN% weekly NN%"; cacheUsage({a.UUID: u})
```

  `linkConfig(home)`: `os.Symlink(~/.codex/config.toml, home/config.toml)` when the source exists
  and the target does not; errors are printed as a note, not fatal. The bare mode fetches usage
  twice (once to learn the email for the rename, once inside `registerCodex`); two GETs on a
  one-time command are not worth a second code path. The probe in the `<email>`
  branch is one `fetchCodexUsage`; a 429 there is fatal ("try again in ...") rather than a login.
- `cmdRemove(args []string)`: parses `--kind` (default `""`), then `findByEmail(v, kind, email)`.
  For a Codex account the note reads
  `login kept at <home>; delete that directory to log the account out`.
- `cmdList`: per row, Codex accounts print `plan` from `planLabel()` and the flags
  `[needs re-auth]` or `[not logged in]` (from `codexLoggedIn`); the `*` marker never applies.
  `sorted()` already groups. A blank line separates the two groups when both exist.
- `cmdUsage`: the loop is unchanged except that `usageFor` dispatches; after the line, append
  `  <Extra.Label> NN%` for each extra gauge. The `saveVault(v)` at the end persists
  `NeedsReauth`/`Plan` updates for Codex too.
- `cmdPick`: `kinds := []string{o.Kind}` or `{kindClaude, kindCodex}` for `all`; run `rank` per
  kind and append. The JSON row gets `Kind string \`json:"kind"\`` as the first field; the text
  form prints `codex` rows under a `codex:` header line when `all`.
- `usageNote(u)`: append extras like `cmdUsage`.
- `usageText()`: add

```
  vibemon codex add [<email> | --home <dir>]
                           log a ChatGPT account in (browser) and track it, or register an existing CODEX_HOME
  vibemon remove [--kind claude|codex] <email>
  vibemon pick [--kind claude|codex|all] ...
  vibemon exec --kind codex [flags] [--] codex exec ...
```

### `gui.go`

- `panelAccount.Kind string \`json:"kind"\`` set from `a.kind()`.
- `pollOnce` loop: for Codex accounts `pa.HasLogin = pa.HasToken = codexLoggedIn(codexHome(a.Email))`,
  the not-logged-in error text is `"not logged in: vibemon codex add <email>"`, and the poll
  condition is `case isActive || includeParked || a.kind() == kindCodex:` (decision 6). `usageFor`
  dispatches; `penalise`, `previous[]`, `cacheUsage` unchanged.
- `autoRotate`: both candidate loops `continue` on `a.Kind == kindCodex`.
- `buildMenu`: the switch checkboxes and the "Preferred account" radios skip Codex accounts.
- `confirmRemove`: message for Codex: `"Forget %s?\n\nvibemon stops tracking it. The login stays
  in %s; delete that directory to log the account out."`
- `trayLabel`: unchanged (only the active Claude account).

### `frontend/index.html`

- Account list renders two groups: `<h2>Claude</h2>` rows as today, then `<h2>Codex</h2>` rows
  when any `kind === 'codex'` exists. When there are no Codex accounts the heading stays
  `Accounts` so the current look is unchanged.
- A Codex row has no active dot semantics (`dot` stays hollow), is not clickable for switching
  (skip binding `panel:switch` on `.acct[data-kind=codex]`, `cursor: default`), shows the same
  `nums` (`session% · weekly%`), the same `resets ...` sub line and turns, `benched` and `needs
  login` badges as a Claude row. The `×` (forget) stays. The `sub` for a Codex account without a
  login reads `not logged in`.
- The header gauges keep showing the active Claude account; a Codex account has no "active".

### `frontend/settings.html`

- Accounts table gets a `kind` column (`claude`/`codex`); `login` and `token` read `yes`/`no` from
  the same booleans (both equal `logged in` for Codex).
- `renderOrderList` appends ` <span class="dim">(codex)</span>` after a Codex email, and the
  `(no token)` note only for Claude.
- Hint text under "Accounts" gains one sentence: `ChatGPT accounts are added with vibemon codex
  add and run under vibemon exec --kind codex.`

## CLI surface after the change

```
vibemon codex add                       browser login into a new home, named by the reported email
vibemon codex add <email>               ensure that account is logged in and tracked
vibemon codex add --home ~/.codex       register the existing login by symlink
vibemon remove [--kind codex] <email>   forget (login kept)
vibemon list                            Claude rows, blank line, Codex rows (ChatGPT · <plan>)
vibemon usage                           both kinds; Codex rows append additional limits
vibemon pick [--kind claude|codex|all] [--json]    default all; every JSON row carries "kind"
vibemon exec --kind codex [--workdir d] [--account e] [--exclude a,b] [--ceiling n] [--wait] [--json] -- codex exec ...
```

Riker's form once the follow-up lands:

```sh
riker-run start codex-<slug> --task ... --cwd "$WT" -- \
  vibemon exec --kind codex --workdir "$WT" -- \
  codex exec -m "$(route codex model)" -c model_reasoning_effort="$(route codex effort)" \
  --sandbox read-only --skip-git-repo-check -o "$TASK_DIR/codex-review.md" "<prompt>"
```

Exit codes are the existing ones: 0 done, 1 failed, 2 no Codex account has headroom (earliest
return printed, `--wait` sleeps), 3 never for Codex.

## `pick --json` contract (Riker hand-off, documentation only)

```json
{
  "runnable": [{"kind": "claude", "email": "...", "usage": {...}},
               {"kind": "codex",  "email": "...", "usage": {"session": {...}, "weekly": {...}, "extra": [...], "fetchedAt": "..."}}],
  "skipped":  [{"kind": "codex", "email": "...", "reason": "Weekly at 100%", "until": "2026-09-15T19:59:30+02:00"}],
  "project":  "/Users/mapa/github.com/mxcd/vibemon"
}
```

`kind` is new on every row; `email`, `usage`, `reason`, `until`, `project` keep their names and
types. Rows are Claude first, then Codex, each in priority order. The bridge groups by `kind`.

## Tests

All assert-based `testing`, fixtures as string constants in the test file (anonymised emails and
ids; no account of MaPa's in the repo).

### `codex_test.go` (new)

`TestNormalizeCodexUsage`, table-driven over four fixtures:

1. `codexFixtureWeeklySpent`: the live shape, primary 604800 s at 100 with `reset_at
   1789495170`, `secondary_window: null`, `limit_reached: true`, `additional_rate_limits: []`.
   Expect `Weekly.Percent == 100`, `Weekly.Severity == "critical"`, `Weekly.ResetsAt` equal to
   `time.Unix(1789495170, 0)`, `Session.Percent == 0`, `Session.ResetsAt == nil`, `len(Extra) == 0`.
2. `codexFixtureProBothWindows`: primary 18000 s at 42 (`reset_at 1789150000`), secondary
   604800 s at 17 (`reset_at 1789640000`), `limit_reached: false`. Expect `Session.Percent == 42`,
   `Weekly.Percent == 17`, both `normal`, both reset times set.
3. `codexFixtureAdditional`: fixture 1 plus one additional limit `GPT-5.3-Codex-Spark` with
   primary 18000 s at 60 and secondary 604800 s at 12. Expect gauges as in fixture 1 and
   `Extra == [{Label: "GPT-5.3-Codex-Spark", Percent: 60, ResetsAt: primary's reset}]`.
4. `codexFixtureResetAfterOnly`: primary 604800 s at 5 with `reset_at: 0`,
   `reset_after_seconds: 3600`. Expect `Weekly.ResetsAt` within a second of `now + 1h`.

`TestFetchCodexUsageClassifiesStatus`: `httptest.Server` behind a `codexUsageURL` var (make it a
`var` like `tokenURL`), auth.json written into a temp home; 401 and 403 are `errNeedsReauth`, 429
is `*rateLimitError`, 200 decodes and the request carried both headers. A home without auth.json
is `errNeedsReauth` without any request.

`TestCodexChildEnv`: an environment with `CODEX_HOME=/elsewhere`, `OPENAI_API_KEY=sk-x`,
`ANTHROPIC_API_KEY=keep` yields exactly one `CODEX_HOME=<home>`, no `OPENAI_API_KEY`, and leaves
unrelated variables alone.

`TestCodexKeyAndHome`: `codexKey("Max@X.io") == "codex:max@x.io"`, `codexHome` honours
`VIBEMON_CODEX_HOMES`, `keyKind("codex:x") == kindCodex`, `keyKind("uuid") == kindClaude`.

### `limits_test.go`

Add to `TestClassify`:

- `{"codex usage limit", "", "ERROR: You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at Sep 15th, 2026 7:59 PM.", 1, outLimit, ""}`
- `{"codex out of credits", "", "ERROR: You're out of credits.", 1, outLimit, ""}`
- `{"codex not logged in", "", "ERROR: unexpected status 401 Unauthorized: Missing bearer or basic authentication in header, url: https://api.openai.com/v1/responses", 1, outAuth, ""}`
- `{"codex prose about limits", "The usage limit is documented in the README.", "", 0, outOK, ""}`

Add to `TestParseReset` (now 08.09.2026 10:00 Berlin):
`{"... or try again at Sep 15th, 2026 7:59 PM.", time.Date(2026, 9, 15, 19, 59, 0, 0, berlin)}`
and `{"try again at 11:59 PM", time.Date(2026, 9, 8, 23, 59, 0, 0, berlin)}`.

`TestBenchForCodexLimit`: `benchFor(<the real line>, now)` equals 15.09.2026 20:01 local (reset
plus two minutes), not `now + 1h`.

### `fleet_test.go`

`testVault()` gains two Codex accounts, `"codex:x@x.io"` and `"codex:y@x.io"` (`Kind: kindCodex`).
Existing assertions keep passing because every `accountOrder` and `rank` call there passes
`kindClaude` (4 Claude accounts).

`TestRankPartitionsByKind`:

- `VIBEMON_CODEX_HOMES` set to a temp dir with `x@x.io/auth.json` present and no `y@x.io` dir.
- `rank(kindClaude, ...)` never lists a Codex account, runnable or skipped.
- `rank(kindCodex, ...)` with an empty policy: runnable `[x]`, skipped `[y]` with reason
  containing `not logged in`.
- With `prefs{Order: ["c", "b", "a"]}` (Claude keys only): `rank(kindCodex)` still yields `[x]`
  (decision 8, second sentence). With `Order: ["codex:gone"]`: `rank(kindCodex)` yields nothing
  (fail closed).
- A bench on `codex:x@x.io` and cached `Usage{Weekly: 100, ResetsAt future}` each drop `x` out.

### `exec_test.go`

The fake script logs `home=$CODEX_HOME okey=${OPENAI_API_KEY:-unset}` in addition to the token and
`api=`; `setupFake` also writes it as `codex` and exports `OPENAI_API_KEY=sk-must-not-leak`, sets
`VIBEMON_CODEX_HOMES` to a temp dir and creates `x@x.io/auth.json` and `y@x.io/auth.json`.

`TestExecCodexRerunsUnderNextAccountOnLimit`:

```
lines: `1||ERROR: You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at 11:59 PM.`
       `0|{"type":"turn.completed"}|`
run:   --kind codex --workdir <dir> --json -- <dir>/codex exec --skip-git-repo-check -o out.md "review this"
assert: exit 0; JSON has "kind":"codex", "account":"y@x.io", two attempts with outcomes limit, ok
        attempt 0: home=<homes>/x@x.io, attempt 1: home=<homes>/y@x.io, both okey=unset
        args are exactly `exec --skip-git-repo-check -o out.md review this` on both (no --session-id, --resume, --model)
        fleet: codex:x@x.io benched with reason containing "usage limit", turnsLastHour 1 each
```

`TestParseExecArgsStopsAtClaudeFlags` and `cmdExec` call `hasPositionalPrompt` and `isHeadless`
with the new leading `kind` argument (`kindClaude`); the existing `childEnv` call sites become
`c.Account.childEnv()`.

`TestExecCodexRefusesClaudeOnlyFlags`: `parseExecArgs({"--kind","codex","--model","x"})` and
`{"--kind","codex","--session","s"}` return errors; `{"--kind","codex"}` yields `Command ==
["codex"]`; `isHeadless(kindCodex, ["codex","exec","-p","prof","hi"])` is true and
`isHeadless(kindCodex, ["codex","-p","prof"])` is false; `hasPositionalPrompt(kindCodex,
["codex","exec","-o","f","hi"])` is true and without `"hi"` false.

### `accounts_test.go`

`TestFindByEmailAcrossKinds`: a vault with `a@x.io` as both kinds: `findByEmail(v, "", "a@x.io")`
errors, `findByEmail(v, kindCodex, "a@x.io")` returns the Codex entry, and the exact key
`"codex:a@x.io"` resolves with `kind == ""`.

`TestVaultRoundTripKeepsClaudeEntriesUnchanged`: marshal a Claude-only vault and assert the JSON
contains no `"kind"`.

## Gates

```sh
go build ./... && go vet ./... && go test ./...
just check                      # test + gofmt -l . + vet
```

`gofmt -l .` must print nothing. No new module in `go.mod`.

Manual smoke (not a gate, record the result in the PR):
`vibemon codex add --home ~/.codex` shows `max.partenfelder@aso.nexus (ChatGPT prolite) weekly
100%`, and `vibemon pick --kind codex` lists it under skipped with `Weekly at 100%` and the
15.09.2026 return.

## Risks

1. **The `try again at` time zone.** The message prints a local clock time without a zone;
   `parseReset` uses `time.Local`, which is right on MaPa's machine. A wrong zone shifts the bench
   by hours, never days; the reset from the usage endpoint (`reset_at`) corrects the picture on the
   next poll.
2. **Pro-plan window shape is inferred, not observed.** Decision 4 maps by window length, which
   is right for both known shapes. If a Pro account reports two windows shorter than seven days,
   both land in `Session` and the later one wins; `usage` output would make that visible at once.
3. **`codex login` writing through the `config.toml` symlink.** Trust-level prompts rewrite
   `config.toml`; through the symlink that edits `~/.codex/config.toml`, which is the intended
   sharing but also means one account's trust answer applies to all. Acceptable; documented.
4. **The wham endpoint is not a public API.** Field names could change under us; the normaliser
   tolerates missing windows (nil pointers) and an empty `additional_rate_limits`. A 5xx or a
   shape change shows as a fetch error with backoff, never as a re-auth prompt.
5. **Per-model Codex limits bench the whole account.** `You've hit your usage limit for <model>`
   is `outLimit`; another model on the same account might still run. Reviews use one model, so
   the cost is a bench that is too wide for a few hours. Ceiling marked `ponytail:` in the
   classifier comment; upgrade path is a Codex `outModelCap` driven by `additional_rate_limits`.
6. **`isHeadless` for Codex keys on the word `exec` anywhere in the argv.** A prompt that is the
   single word `exec` would be misread; real prompts are sentences.
7. **Two accounts sharing an email across kinds** (`max.partenfelder@wilde-it.com` may be both).
   `findByEmail` errors on ambiguity and `remove --kind` resolves it; `rank` always has a kind.
   `switch`, `token`, `add-token` are Claude-only by construction.
8. **The vault keychain item now holds non-secret Codex rows.** Harmless, but `just reset-vault`
   forgets them too; the homes survive and `codex add <email>` re-registers without a login.

## Implementation order

One commit per step; each leaves `just check` green.

1. `feat(accounts): account kind, codex vault keys and kind-aware lookup`
   `accounts.go` (Kind, kind(), constants, sorted, planLabel, findByEmail with kind, switchTo
   refusal), `codex.go` with only `codexKey`, `keyKind`, `codexHomesDir`, `codexHome`,
   `codexLoggedIn`; `prefs.accountOrder(v, dir, kind)`; `pickOptions.Kind` and `rank` split;
   `testVault()` extension; `TestRankPartitionsByKind`, `TestFindByEmailAcrossKinds`,
   `TestVaultRoundTripKeepsClaudeEntriesUnchanged`, `TestCodexKeyAndHome`.
2. `feat(codex): usage from the ChatGPT wham endpoint`
   `api.go` `doJSON` split and `Usage.Extra`; `codex.go` auth reader, response types,
   `normalize`, `fetchCodexUsage`, `codexUsageFor`; `usageFor` dispatch;
   `TestNormalizeCodexUsage`, `TestFetchCodexUsageClassifiesStatus`.
3. `feat(limits): codex limit, credit and login phrasing with try-again reset times`
   `limits.go` regex changes; `TestClassify` cases, `TestParseReset` cases,
   `TestBenchForCodexLimit`.
4. `feat(exec): --kind codex runs codex under a per-account CODEX_HOME`
   `exec.go` (`--kind`, defaults, `isHeadless`, `hasPositionalPrompt`, `codexValueFlags`,
   `claudeChildEnv` rename, `Account.childEnv`, headless loop branches, emit kind, usage text),
   `codexChildEnv`; `cmdPick` kinds and `kind` row field; `TestExecCodexRerunsUnderNextAccountOnLimit`,
   `TestExecCodexRefusesClaudeOnlyFlags`, `TestCodexChildEnv`.
5. `feat(cli): vibemon codex add, remove --kind, both kinds in list and usage`
   `codexLogin`, `registerCodex`, `linkConfig`, `cmdCodexAdd`, `cmdRemove` flag, `cmdList`,
   `cmdUsage`, `usageNote` extras, `usageText`.
6. `feat(gui): codex accounts as their own group in panel and settings`
   `gui.go` (Kind, poll branch, autoRotate and menu skips, confirmRemove text),
   `frontend/index.html`, `frontend/settings.html`. Build with `just run` and eyeball once.
7. `docs: codex accounts in CLAUDE.md and README`
   `CLAUDE.md`: layout row `codex.go | Codex homes, wham usage, codex login wrapper, child env`,
   a "Codex accounts" rule block (one CODEX_HOME per login under `~/.vibemon/codex/<email>`;
   vibemon reads `auth.json` and never writes or refreshes it; a 401 is re-auth, a 429 is back
   off; `remove` keeps the home; the Codex path never calls `keychain.go`), and the unverified
   list gains: a real `vibemon codex add` login, a real limit under `exec --kind codex`, the
   `try again at` zone. `README.md`: a "Codex (ChatGPT) accounts" section with the four commands
   and the Riker form, and `pick --json`'s `kind` field.

## Self-review

Findings from the one pass over this plan, all incorporated above:

- The task's literal "primary is weekly, secondary is session" would misreport a Pro account;
  mapping by window length (decision 4) is backed by the live response's additional limit shape.
- `codex exec -p` means profile; `isHeadless` had to become kind-aware or every Codex profile run
  would have been treated as headless and every prompt-less run as interactive.
- `hasPositionalPrompt` with the Claude flag table would misread `-o FILE` as a prompt and skip
  reading a piped prompt; the Codex table fixes it.
- `findByEmail` without a kind would attach a Claude setup-token to a Codex entry when the
  emails coincide; the kind argument closes that.
- `accountOrder` unchanged would have made `pick --kind codex` return nothing for anyone with a
  configured global order (every Riker machine); the partition rule in decision 8 keeps fail
  closed for named keys and open for unnamed kinds.
- The exec test must use a clock-time reset (`try again at 11:59 PM`), not the dated real line,
  or the bench assertion expires on 15.09.2026; the dated form is covered in `TestParseReset`
  with a fixed `now`.
- `remove` must not delete the home; the Claude `remove` does not log out either.
- `capture()` deletes every vault entry with the captured login's email; a Codex entry with the
  same email would vanish on the next Claude capture. Guarded by kind.
- The regex changes were run against the real limit line and the real 401 line with a throwaway
  test on this branch (classify gives `outLimit` and `outAuth`, `parseReset` gives 15.09.2026
  19:59 Berlin, the `resets 9:40am (Europe/Rome)` form still parses, `reReached` does not misfire).
