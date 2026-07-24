# vibemon

A macOS menu bar monitor for Claude Code usage limits — and a switcher for juggling several Claude
accounts.

Claude Code enforces a 5-hour session limit and a 7-day weekly limit, but the only way to see where
you stand is to type `/usage` inside a running session. vibemon puts those numbers in the menu bar,
watches every account you own — not just the one you are signed into — and swaps the active account
in a click.

<p align="center">
  <img src="docs/panel.png" alt="The vibemon panel: session, weekly and per-model gauges over a list of accounts" width="360">
</p>

## What it does

- **Live gauges** for the 5-hour session window, the 7-day weekly window, and the per-model weekly
  limit when one is in play, each with its reset countdown.
- **Every account at once.** Parked accounts keep reporting usage in the background, so you know
  which one has room before you switch to it.
- **One-click switching.** The next Claude Code start picks up the new account.
- **Auto-rotation** (optional) moves you off an account that is nearly spent, and comes back to your
  preferred account once it recovers.
- **Re-auth warnings** when an account's refresh token dies.

### Menu bar

The label beside the icon has three densities, set from the tray menu:

<p align="center">
  <img src="docs/menubar.png" alt="Menu bar label in condensed, extended and extra extended density" width="560">
</p>

## Install

Requires macOS, Go 1.26+, and [just](https://github.com/casey/just). Claude Code must already be
installed and logged in.

```sh
git clone git@github.com:mxcd/vibemon.git
cd vibemon
just install      # builds and copies to ~/.local/bin
vibemon           # run the menu bar app
```

To start it at login:

```sh
just autostart    # installs a LaunchAgent; logs land in ~/Library/Logs/vibemon.log
just no-autostart # undo
```

## Getting your accounts in

vibemon never performs a login itself — it captures whatever Claude Code is already signed into.

```sh
claude auth login       # sign in as the account you want to add
vibemon capture         # or press Capture in the panel
```

Repeat per account. Then switch whenever you like:

```sh
vibemon list                    # * marks the active account
vibemon switch you@example.com
vibemon usage                   # usage for every stored account
vibemon remove you@example.com  # forget an account
```

**Restart Claude Code after a switch.** Running sessions hold their token in memory and keep using
the old account until they exit; `vibemon switch` tells you how many are running. To carry on the
same conversation afterwards, `claude -c` continues where you left off.

## Auto-rotation

Off by default — enable *Auto-switch when exhausted* in the tray menu. When the active account
crosses 95% on either window, vibemon moves to the account with the most headroom. Mark one account
as **Preferred** and vibemon will favour it whenever it has room, and return to it after rotating
away. Switching accounts by hand clears that intent until you rotate again.

Two honest limits: rotation only affects sessions started *after* it, so it cannot rescue a session
that is already blocked; and the 95% trigger is a constant in `gui.go` if you want to run closer to
the edge.

## How it works

Usage comes from `GET https://api.anthropic.com/api/oauth/usage`, the same endpoint `/usage` renders
from, authenticated with the OAuth token Claude Code already holds. Account identity comes from
`/api/oauth/profile`.

Accounts live in your login keychain under `vibemon-accounts`. Switching rewrites the `claudeAiOauth`
key inside Claude Code's own keychain item (`Claude Code-credentials`) and patches `oauthAccount` and
`userID` in `~/.claude.json`.

Three properties this is careful about, because getting them wrong is silent and expensive:

- **MCP tokens are preserved.** That keychain item also holds every MCP server token you have
  authorised. Only the `claudeAiOauth` key is ever replaced; everything else passes through
  untouched, and a test asserts it.
- **Every keychain write is verified.** `security(1)` reports success even when it stored nothing
  useful, so each write is read back and compared before it counts.
- **The active account's token is never refreshed by vibemon.** Claude Code owns it; refreshing it
  from outside risks invalidating the session you are sitting in. Only parked accounts get refreshed.

Nothing leaves your machine except the two Anthropic API calls above. Preferences live in
`~/Library/Application Support/vibemon/prefs.json` and contain no secrets.

## Development

```sh
just check   # test + gofmt + vet
just run     # build and launch
just icon    # render the tray glyph and open it
```

The panel is a single `frontend/index.html` — markup, CSS and JS in one file, embedded with
`go:embed`. No npm, no build step. See `CLAUDE.md` for the rules around the credential paths.

The screenshots above are rendered from the real panel markup with sample data, so no account of
mine ends up in the repo.

## Caveats

- macOS only. Credential storage differs on Windows and Linux.
- Built against Claude Code 2.1.219. The usage endpoint and keychain layout are not public API and
  could change under you.
- Wails v3 is still alpha.

## License

MIT
