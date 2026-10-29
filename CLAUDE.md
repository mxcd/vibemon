# vibemon

macOS menu bar app that shows Claude Code usage limits and switches between Claude accounts; on
Linux the same binary runs headless (CLI, proxies, monitor, dashboard; no menu bar).
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

## Linux: files instead of the keychain

Both secrets go through `secretStore` (`secrets.go`): the keychain on macOS, `fileSecrets` on Linux.
What Claude Code does there was read off its 2.1.312 linux-x64 build, not guessed: the store is
file-only, `<config dir>/.credentials.json` with the config dir `CLAUDE_CONFIG_DIR` or `~/.claude`,
the same JSON as the keychain value (`claudeAiOauth`, `mcpOAuth`, ...), written 0600 through a temp
file and a rename, and dropped from its cache when the file's mtime changes.

- **Rules 1 and 3 hold unchanged**: same blob, same read-modify-write of `claudeAiOauth` only.
- **Rule 2 becomes**: write a temp file in the same directory, sync, rename, read back and compare.
  `.credentials.json` keeps its mode (0600 when new), the vault is always 0600. No single-line rule:
  that was `security -w` printing a multi-line secret as hex, and `switchTo`'s restore must be able
  to put back the exact bytes Claude Code wrote, pretty-printed or not.
- **A symlinked `.credentials.json` is not supported**: the rename replaces the link with a regular
  file.
- **Missing means empty, nothing else does**: only `fs.ErrNotExist` reads as "no vault yet".
- **`CLAUDE_CONFIG_DIR` moves both files** (`.credentials.json` and `.claude.json`), so Linux follows
  it for both; macOS ignores it, where it would also rename the keychain item.
- The vault is `stateDir()/vault.json` (`~/.config/vibemon`, created 0700). Nothing but vault.lock
  guards the files: every read-modify-write of either secret already runs under it (so does the
  Linux `just reset-vault`, through flock(1)), and the rename keeps readers whole. Claude Code's own
  lock (`<config dir>/.storage-write.lock`, proper-lockfile) is not taken, so a token refresh
  landing inside a switch's read-to-write window can still lose one of the two writes, as on macOS.
- A bare `vibemon` is `headless_linux.go`: `monitor.run()` without Wails, notices to the journal.
  Wails needs cgo and GTK on Linux, so `gui.go` and `icon.go` are `//go:build darwin`.
- **Tests never resolve a real path**: `TestMain` (`sandbox_test.go`) points HOME,
  `CLAUDE_CONFIG_DIR`, `XDG_CONFIG_HOME`, `VIBEMON_STATE` and `VIBEMON_CODEX_HOMES` into a temp dir
  and puts files in place of the macOS keychain; a test that writes a path calls `sandbox(t)`. A
  resolver that reads a new variable adds it to `sandboxEnv` and itself to
  `TestEveryPathResolvesInsideTheSandbox`.

## Codex accounts

ChatGPT logins are a second account kind and share none of the Claude machinery.

- **One `CODEX_HOME` per login**, at `~/.vibemon/codex/<email>` (`VIBEMON_CODEX_HOMES` overrides the
  parent). The directory is the whole identity; `vibemon codex add <email>` adopts one that is
  already logged in and only runs `codex login` when there is none.
- **Only the Codex proxy writes `auth.json`, and only like this** (`refreshCodexAuth`). A pooled
  home codex is not running in is never refreshed otherwise, so the proxy refreshes the homes it
  sends with:
  - only when the access token is expired or within 5 minutes of its JWT `exp`, or after an
    upstream 401 for that account;
  - under an exclusive flock on `.vibemon-refresh.lock` in that home, re-reading `auth.json` inside
    the lock first: when the token changed meanwhile (codex refreshed), that one is used and nothing
    is refreshed;
  - exactly as codex 0.163.0 does it (`login/src/auth/manager.rs`): `POST
    https://auth.openai.com/oauth/token`, JSON `{client_id, grant_type: refresh_token,
    refresh_token}`, client id `app_EMoamEEZ73f0CkXaXp7hrann`;
  - written atomically (temp file in the same directory, 0600, rename) as a read-modify-write that
    keeps every field it does not own exactly as it was and sets only the tokens the endpoint
    returned and `last_refresh`;
  - a 400, 401 or 403 from the refresh is re-auth (rule 4): that account is benched for a day and the
    proxy moves on.

  The usage poll and `exec` still never refresh: the poll runs every tick for every account, and
  codex itself neither locks nor writes atomically, so every refresh is a rotation a running codex
  could race. The email and plan always come from the usage endpoint, never from what the user
  typed.
- **`~/.codex` is MaPa's interactive login and is never moved, symlinked, registered or
  refreshed.** It is not a pooled account: the proxy never sends with it, and `refreshCodexAuth`
  refuses it outright. Only `config.toml` is shared into a newly created home, by symlink, which is
  why the proxy's `openai_base_url` belongs in a profile (`~/.codex/vibemon.config.toml`), never at
  the top of that file.
- Rule 4 applies unchanged: a 401 or 403 from `wham/usage` is re-auth, a 429 is back off.
- `remove` forgets the vault entry and keeps the home; deleting the directory is what logs the
  account out, and the message says so.
- The Codex path never touches `keychain.go`'s Claude Code item, so rules 1 to 3 hold by
  construction. The vault (`vibemon-accounts`) does hold the Codex rows, which carry no secret.

## Layout

| File | Holds |
|---|---|
| `secrets.go` | `secretStore`, and `fileSecrets`, the Linux store: atomic, mode-keeping, verified writes |
| `keychain_darwin.go` | `security(1)` wrapper, verified writes: the macOS store |
| `secrets_linux.go` | Linux paths: `.credentials.json`, `.claude.json` (both after `CLAUDE_CONFIG_DIR`), the vault file |
| `accounts.go` | vault, account kinds, Claude Code state, switch/capture/forget, plan labels |
| `api.go` | `/api/oauth/usage`, `/api/oauth/profile`, token refresh |
| `codex.go` | Codex homes, `auth.json` read and refresh, wham usage, the `codex login` wrapper, child env |
| `monitor.go` | the poll loop, auto-rotation, backoff and the state every view renders |
| `gui.go` | macOS only: systray, panel and settings windows around a monitor |
| `headless_linux.go` | a bare `vibemon` on Linux: proxies, monitor and dashboard in the foreground |
| `prefs.go` | prefs.json: display, auto-switch, exec order and per-project account policies |
| `exec.go` | `vibemon exec`: interactive pass-through or the headless limit-and-resume loop |
| `fleet.go` | `fleet.json` ledger (turns, benches, usage cache), flock-based locks, account ranking |
| `history.go` | `history.jsonl`: one usage sample per account per poll, feeds the dashboard charts |
| `web.go` | the dashboard on `127.0.0.1:6660`: columns, rundown charts as server-side SVG, htmx swaps |
| `proxy.go` | the auth proxy on `127.0.0.1:6661`: every request to Anthropic leaves with the active account's token (read-only, 5 s cache, dropped on a 401) |
| `codexproxy.go` | the Codex auth proxy on `127.0.0.1:6662`: codex's model traffic leaves with a pooled ChatGPT account (exec's Codex order, sticky until a limit or a dead login), a limit benches and sends the request again on the next account, WebSockets get 426 |
| `limits.go` | limit message classifier and reset-time parser |
| `icon.go` | macOS only: the CRT tray glyph, drawn in code |
| `main.go` | CLI subcommands and the app entrypoint |
| `frontend/index.html` | the whole panel: markup, CSS and JS in one file |
| `frontend/settings.html` | the settings window: tokens, default order, per-project account lists |
| `web/index.html` | the dashboard template (`page` and `view`), CSS included; `web/htmx.min.js` is vendored |

State lives in four places: Claude Code's keychain item (its own credentials), `vibemon-accounts`
(our vault of stored accounts, including headless tokens), and under
`~/Library/Application Support/vibemon/` (`~/.config/vibemon/` on Linux, where `vault.json` sits
too; `VIBEMON_STATE` overrides it): `prefs.json` (display,
auto-switch, exec policies, the dashboard's column selection, never secrets), `fleet.json` (turns,
benches, usage cache, written under a flock by every exec and by the monitor) and `history.jsonl`
(usage samples, appended by every poll, compacted by size to 14 days).

The dashboard is loopback only and not on port 6666: every browser refuses that port (IRC
blocklist). `VIBEMON_WEB` overrides the address. It is read-only apart from the column and chart
selection, which POSTs to `/view`; POSTs require htmx's `HX-Request` header and every request a
loopback `Host`, which is what keeps a cross-site page or a DNS-rebound one out.

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
just web-preview # render the dashboard with seeded data and open it in the browser
just install    # build to ~/.local/bin
just autostart  # LaunchAgent for login start; on Linux a systemd --user unit (Restart=always)
```

`install`, `autostart`, `no-autostart` and `reset-vault` have a `[linux]` twin; `icon` and
`web-preview` are `[macos]` only. Check a Linux change from the Mac with
`GOOS=linux CGO_ENABLED=0 go vet ./...` and run the tests in a container (`secrets_linux_test.go`
only runs there):

```sh
docker run --rm -v "$PWD":/src -w /src golang:1.26 go test -race ./...
```

## Testing

Tests are assert-based `testing`, no frameworks. The ones that matter guard the destructive paths:
`TestSwapOAuthPreservesEverythingElse` (MCP tokens survive a switch),
`TestFileSecretsSwitchKeepsMCPOAuth` (the same through the whole Linux file path, on any OS),
`TestPatchClaudeJSONTouchesOnlyIdentity` (the 500KB config keeps its 178 project entries),
`TestRefreshCodexAuthWritesLikeCodex` (a refresh keeps every `auth.json` field it does not own) and
`TestRefreshCodexAuthUsesWhatChangedUnderTheLock` (no second rotation of a refresh token).
Keep those green. `exec_test.go` drives the headless loop against a fake `claude` script; it asserts
the resume-under-next-account path and that `ANTHROPIC_API_KEY` never reaches a child.

Unverified in the real world, treat as such until confirmed: a real `vibemon codex add` (both of
MaPa's homes were adopted from a manual `codex login`, and the browser flow inside `codexLogin` has
never run); a real limit under `exec --kind codex` (the classifier's Codex patterns come from one
recorded runner line); the time zone of codex's `try again at <clock time>`, which is read as local;
what a Codex account idle for more than five days does on the next poll (the access token is a
5-day JWT that only codex refreshes, so the poll may answer 401 and raise `NeedsReauth` although a
plain `codex exec` in that home would have refreshed it, and `codex add <email>` would then run a
browser login that was not needed); a live account switch followed by
`/mcp` reconnecting; a parked account surviving past its ~12h token expiry; `exec` against a real
limit (the classifier's patterns come from the Paloma One runner logs, the fake script in the tests
replays them); interactive `vibemon exec` behaviour of features that need a full-scope login;
the auth proxy against the real API (a Claude Code session on `ANTHROPIC_BASE_URL`, SSE through it,
a switch reaching that session, and whether the session's own refresh of its startup account writes
that account back over a switch: read from the code, not observed, it should not, because in the
2.1.312 bundle the refresh save, the function that logs `OAuth refresh CAS save failed` and takes
`postedRefreshToken`, writes only while the stored `claudeAiOauth.refreshToken` is empty or still
the one it posted, and otherwise keeps what is stored);
and the Codex proxy against the real API. Its routing was checked
with codex 0.163.0 against local stand-ins only (`openai_base_url` sends `/models` and `/responses`
to the proxy, `chatgpt_base_url` traffic stays on the own login, a 426 moves codex to HTTP for the
session); unverified are a real 429 body and the retry on the next account, a refresh at
auth.openai.com, and whether upstream accepts a conversation's encrypted reasoning items under an
account other than the one that produced them, which every switch mid-session asks of it.
Linux has only run in a `golang` container (tests, and an offline smoke of the CLI round trip and a
headless start): unverified are a real `claude auth login` plus `capture` on a Linux box, the systemd
unit under linger, and when a session off the proxy notices a switched `.credentials.json` (the
bundle drops its cached token on an mtime change, but when a running session checks is not known,
so the restart advice stands).
