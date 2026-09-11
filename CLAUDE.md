# vibemon

macOS menu bar app that shows Claude Code usage limits and switches between Claude accounts.
Go + Wails v3, single binary, plain HTML/CSS panel — no npm, no frontend build step.

## Read this before touching credentials

Four rules, each learned the hard way. Breaking any of them silently damages the user's setup.

**1. Never overwrite the Claude Code keychain blob wholesale.**
`service="Claude Code-credentials"` holds `claudeAiOauth` *and* `mcpOAuth` — every MCP server token
the user has authorised. Only ever read-modify-write the `claudeAiOauth` key and pass everything
else through as the raw bytes it arrived as (`swapOAuth` in `accounts.go`). Losing `mcpOAuth`
silently breaks every connector, and the user only finds out the next time one fails.

**2. Every keychain write must be read back and verified.**
`/usr/bin/security` exits 0 even when it stored nothing useful. Its stdin prompt truncates secrets
at 128 bytes without complaint — it once stored a 1562-byte vault as 128 bytes and reported success.
`keychainWrite` writes, reads back, and compares. Do not "simplify" that away.

**3. Never refresh the active account's token.**
Claude Code owns those tokens and keeps them fresh. If Anthropic rotates refresh tokens, refreshing
from here invalidates the running session's token and logs the user out. `usageFor` reads the active
account's token straight from the keychain and only ever refreshes *parked* accounts.

**4. Classify HTTP failures before acting on them.**
`/api/oauth/usage` rate limits per account. A 429 means "ask later"; only 400/401/403 mean the grant
is dead. Treating every 4xx as re-auth once told the user to log in again on a healthy account. Every
failed fetch benches that account (`monitor.penalise`) instead of retrying on the next tick, and a
transient error never clears an existing `NeedsReauth` flag.

**5. A `claude setup-token` token is inference-only.**
`/api/oauth/usage` and `/api/oauth/profile` answer 403 `oauth_scope_insufficient` for it, and
`apiKeyHelper` rejects it outright (401). It works only as `CLAUDE_CODE_OAUTH_TOKEN` on a child
process, which is what `exec` does. Never write one into the Claude Code keychain blob (`switchTo`
refuses), and never expect to poll usage with it: usage comes from the captured login of the same
email. `exec` never refreshes tokens either; several runners refreshing one parked refresh token
concurrently would race the rotation, so usage numbers for picking come from the monitor via
`fleet.json`.

Related: shell out to `/usr/bin/security` rather than calling `SecItem*` via cgo. Claude Code
created the keychain item through that same binary, so its ACL already trusts it and reads/writes
never prompt. A native call from our own binary is a different app and may raise a prompt.

## Codex accounts

ChatGPT logins are a second account kind and share none of the Claude machinery.

- **One `CODEX_HOME` per login**, at `~/.vibemon/codex/<email>` (`VIBEMON_CODEX_HOMES` overrides the
  parent). The directory is the whole identity; `vibemon codex add <email>` adopts one that is
  already logged in and only runs `codex login` when there is none.
- **vibemon reads `auth.json` and never writes it.** codex owns those tokens and refreshes them in
  its own home; a second refresher would race the rotation. The email and plan always come from the
  usage endpoint, never from what the user typed.
- **`~/.codex` is MaPa's interactive login and is never moved, symlinked or registered.** Only
  `config.toml` is shared into a newly created home, by symlink.
- Rule 4 applies unchanged: a 401 or 403 from `wham/usage` is re-auth, a 429 is back off.
- `remove` forgets the vault entry and keeps the home; deleting the directory is what logs the
  account out, and the message says so.
- The Codex path never touches `keychain.go`'s Claude Code item, so rules 1 to 3 hold by
  construction. The vault (`vibemon-accounts`) does hold the Codex rows, which carry no secret.

## Layout

| File | Holds |
|---|---|
| `keychain.go` | `security(1)` wrapper — verified writes |
| `accounts.go` | vault, account kinds, Claude Code state, switch/capture/forget, plan labels |
| `api.go` | `/api/oauth/usage`, `/api/oauth/profile`, token refresh |
| `codex.go` | Codex homes, wham usage, the `codex login` wrapper, child env |
| `gui.go` | systray, panel and settings windows, poll loop |
| `prefs.go` | prefs.json: display, auto-switch, exec order and per-project account policies |
| `exec.go` | `vibemon exec`: interactive pass-through or the headless limit-and-resume loop |
| `fleet.go` | `fleet.json` ledger (turns, benches, usage cache), flock-based locks, account ranking |
| `limits.go` | limit message classifier and reset-time parser |
| `icon.go` | the CRT tray glyph, drawn in code |
| `main.go` | CLI subcommands and the GUI entrypoint |
| `frontend/index.html` | the whole panel: markup, CSS and JS in one file |
| `frontend/settings.html` | the settings window: tokens, default order, per-project account lists |

State lives in four places: Claude Code's keychain item (its own credentials), `vibemon-accounts`
(our vault of stored accounts, including headless tokens), and under
`~/Library/Application Support/vibemon/` (`VIBEMON_STATE` overrides it): `prefs.json` (display,
auto-switch, exec policies, never secrets) and `fleet.json` (turns, benches, usage cache, written
under a flock by every exec and by the monitor).

## Conventions

- Comments explain *why*, never restate the code. Deliberate shortcuts are marked `ponytail:` with
  the ceiling they hit and the upgrade path.
- `Usage` is the normalised UI-facing shape; parse `limits[]` from the API in preference to the flat
  `five_hour`/`seven_day` windows, because it carries severity and per-model scoping.
- Anything touching `~/.claude.json` re-reads immediately before writing (live sessions rewrite it
  constantly) and replaces it atomically via a temp file + rename.
- The panel talks to Go over Wails events only (`panel:*` and `settings:*` in, `state` out). No bindings.
- `exec` classifies a child's outcome from its output alone (`classify` in `limits.go`); it never
  carries a verdict from one attempt into the next. A stale signal once benched five healthy accounts.

## Commands

```sh
just check      # test + gofmt + vet
just run        # build and launch the menu bar app
just icon       # render the tray glyph and open it — the only way to judge icon.go
just install    # build to ~/.local/bin
just autostart  # LaunchAgent for login start
```

## Testing

Tests are assert-based `testing`, no frameworks. The ones that matter guard the destructive paths:
`TestSwapOAuthPreservesEverythingElse` (MCP tokens survive a switch) and
`TestPatchClaudeJSONTouchesOnlyIdentity` (the 500KB config keeps its 178 project entries).
Keep those green. `exec_test.go` drives the headless loop against a fake `claude` script; it asserts
the resume-under-next-account path and that `ANTHROPIC_API_KEY` never reaches a child.

Unverified in the real world, treat as such until confirmed: a real `vibemon codex add` (both of
MaPa's homes were adopted from a manual `codex login`, and the browser flow inside `codexLogin` has
never run); a real limit under `exec --kind codex` (the classifier's Codex patterns come from one
recorded runner line); the time zone of codex's `try again at <clock time>`, which is read as local;
a live account switch followed by
`/mcp` reconnecting; a parked account surviving past its ~12h token expiry; `exec` against a real
limit (the classifier's patterns come from the Paloma One runner logs, the fake script in the tests
replays them); and interactive `vibemon exec` behaviour of features that need a full-scope login.
