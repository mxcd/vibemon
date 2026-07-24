# vibemon

macOS menu bar app that shows Claude Code usage limits and switches between Claude accounts.
Go + Wails v3, single binary, plain HTML/CSS panel — no npm, no frontend build step.

## Read this before touching credentials

Three rules, each learned the hard way. Breaking any of them silently damages the user's setup.

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

Related: shell out to `/usr/bin/security` rather than calling `SecItem*` via cgo. Claude Code
created the keychain item through that same binary, so its ACL already trusts it and reads/writes
never prompt. A native call from our own binary is a different app and may raise a prompt.

## Layout

| File | Holds |
|---|---|
| `keychain.go` | `security(1)` wrapper — verified writes |
| `accounts.go` | vault, Claude Code state, switch/capture/forget, plan labels |
| `api.go` | `/api/oauth/usage`, `/api/oauth/profile`, token refresh |
| `gui.go` | systray, panel window, poll loop, prefs |
| `icon.go` | the CRT tray glyph, drawn in code |
| `main.go` | CLI subcommands and the GUI entrypoint |
| `frontend/index.html` | the whole panel: markup, CSS and JS in one file |

State lives in three places: Claude Code's keychain item (its own credentials), `vibemon-accounts`
(our vault of stored accounts), and `~/Library/Application Support/vibemon/prefs.json` (display
preference only — never secrets).

## Conventions

- Comments explain *why*, never restate the code. Deliberate shortcuts are marked `ponytail:` with
  the ceiling they hit and the upgrade path.
- `Usage` is the normalised UI-facing shape; parse `limits[]` from the API in preference to the flat
  `five_hour`/`seven_day` windows, because it carries severity and per-model scoping.
- Anything touching `~/.claude.json` re-reads immediately before writing (live sessions rewrite it
  constantly) and replaces it atomically via a temp file + rename.
- The panel talks to Go over Wails events only (`panel:*` in, `state` out). No bindings.

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
Keep those green.

Two things remain unverified in the real world and should be treated as such until confirmed:
a live account switch followed by `/mcp` reconnecting, and a parked account surviving past its
~12h token expiry.
