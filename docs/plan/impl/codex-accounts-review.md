# Review: Codex (ChatGPT) accounts beside Claude accounts

Task `aso/vibemon/codex-accounts`, branch `task/codex-accounts` (`a27e0ed..205b640`, 10 commits on
`main`), reviewed 11.09.2026 against `docs/plan/impl/codex-accounts.md`.
Reviewer: Fable 5.1. Codex round 1: gpt-6-astra high, ran to completion (no quota limit), verdict
REJECT with 3 MAJOR and 2 MINOR; all five were re-verified by hand below, one reclassified.
Findings are also in `/Users/mapa/.riker/run/aso/vibemon/codex-accounts/findings.json` for inline
comments. Screenshots: `docs/plan/impl/screens/codex-accounts/`.

## Verdict reasoning

The implementation is complete against the plan: every section (account kind, wham usage and its
normaliser, limit classifier, `exec --kind codex` with `CODEX_HOME`, `pick --json` with `kind`,
CLI, panel and settings groups, docs) is present, the four deviations are documented in the plan,
and all gates are green (`go build`, `go vet`, `gofmt -l` empty, `go test -count=1` passes, `just
check` recipe reproduced). The tests the task asked for exist and actually exercise the change:
four recorded wham fixtures, header and status classification against an `httptest` server, the
real Codex limit and 401 lines through `classify` and `parseReset`, kind-partitioned ranking with
the fail-closed case, and the headless rerun asserting `CODEX_HOME` per attempt and no
`OPENAI_API_KEY` on the child.

It is rejected for one reason: `vibemon codex add` can move a directory that CLAUDE.md says is
never moved. `codexHome(email)` joins the typed argument onto `~/.vibemon/codex` without
validation, so `vibemon codex add ../../.codex` resolves to `~/.codex` (verified with
`filepath.Join`), and the mismatch branch then renames that home to the reported email's canonical
path whenever that path does not exist yet. On this machine the aso home exists and the command
errors out, but on any other machine, or for a third account, it relocates the interactive login.
The same branch renames an existing per-account home when the browser signs into a different
account than the one asked for, which the plan said to reject. Both close with one guard (validate
the email, rename only the temporary `.login-<pid>` home) and one test. Nothing else blocks.

## Findings

- **MAJOR** `main.go:208` - the typed email is a path segment without validation; `../../.codex`
  resolves to `~/.codex` and the rename at line 234 can move MaPa's interactive login, which
  CLAUDE.md forbids. Fix: reject an email without `@` or containing `/`, `..` or a path separator
  before it reaches `codexHome`, in both `cmdCodexAdd` and `registerCodex` (the endpoint's email
  is also a path segment).
- **MAJOR** `main.go:234` - a home the user named (`codex add <email>`) is renamed when the login
  turns out to be a different account, so the wrong account silently takes over a home and its
  sessions. The plan said to error on mismatch. Fix: rename only when `home` is the temporary
  `.login-<pid>` directory; otherwise return the plan's "logged in as %s, not %s" error and leave
  both homes as they are. Add a test for the mismatch path.
- **MINOR** `accounts.go:308` - the exact-key shortcut runs before the ambiguity scan, so with a
  Claude token stub keyed by its lowercased email and a Codex account for the same email, `remove
  a@x.io` resolves the Claude entry silently instead of erroring. Fix: run the email scan first
  and use the exact key only as a fallback, plus one case in `TestFindByEmailAcrossKinds`.
  (Codex rated this MAJOR; it needs a token-only Claude stub and the effect is a reversible
  forget, so MINOR.)
- **MINOR** `main.go:197` - the vault flock is held across the interactive `codex login`. The
  monitor's `pollOnce` takes `m.mu` and then `lockVault()`, so the panel freezes for the whole
  browser round trip. Fix: probe and log in before `lockVault()`, take the lock only around
  `loadVault` and `registerCodex`.
- **MINOR** `codex.go:284` - `appendCodexOrder` runs on every registration, so re-running `codex
  add <email>` after a re-auth puts an account the user unticked in Settings back into
  `codexOrder`. Fix: append only when `v[key]` was nil.
- **MINOR** `main.go:218` - a failed or aborted `codex login` leaves `.login-<pid>` behind, and
  the name-collision branch leaves a second logged-in `auth.json` in it. Fix: `os.RemoveAll` the
  temporary home on every error path, as the plan specified.
- **MINOR** `codex.go:195` - the access token is a 5-day JWT that only codex refreshes. An account
  idle for five days answers 401 on the poll, is flagged `NeedsReauth`, and `codex add <email>`
  then runs a browser login even though a plain `codex exec` would have refreshed it. No code
  change required now, but add it to the CLAUDE.md unverified list so the first "needs login" on
  a parked account is read correctly.
- **DESIGN** `frontend/index.html:202` - a Codex row's sub line reads `resets · 4d 7h` because the
  session gauge has no reset and the two countdowns are joined unconditionally; the stray dot
  reads as a broken field. Fix: collect the countdowns that have a `resetsAt` and join those.
- **DESIGN** `frontend/index.html:211` - Codex rows keep the hollow active dot and the same
  `0% · 100%` numbers as Claude rows although neither "active" nor a session window exists for
  them, so a spent Codex account looks half healthy. Fix: `visibility: hidden` on the dot for
  `.acct.codex`, and render a dash for a gauge with `percent === 0` and no `resetsAt`.
- **DESIGN** `accounts.go:81` - `planLabel` yields `ChatGPT · ` with a trailing separator when
  `Plan` is empty (visible in the settings table). Fix: return `ChatGPT` alone when `Plan == ""`.

## Verified fine

- Rules 1 to 3 hold by construction: the Codex path never imports or calls `keychain.go`, and
  `switchTo` refuses a Codex key. Rule 4: `doJSON` maps 401/403 to `errNeedsReauth`, 429 to
  `rateLimitError` with `Retry-After`, everything else to a plain error; `codexUsageFor` sets
  `NeedsReauth` only on `errNeedsReauth` and clears it only on success.
- Vault compatibility: `Kind` is `omitempty`, `TestVaultRoundTripKeepsClaudeEntriesUnchanged`
  asserts no `"kind"` in a Claude-only vault; `capture()` skips Codex entries with the same email.
- `accountOrder` partitions by kind, falls back to the global list of that kind, fails closed on
  named-but-gone keys (`TestRankPartitionsByKind`); prefs gain `codexOrder`, the GUI reloads
  orders from disk each tick so CLI appends are not overwritten by a tray toggle.
- `exec --kind codex`: `--model` and `--session` refused, no `buildArgs` surgery, no session lock,
  whole-command rerun, `kind` in the JSON; `codexChildEnv` drops inherited `CODEX_HOME` and
  `OPENAI_API_KEY` and keeps `ANTHROPIC_API_KEY` handling unchanged for Claude.
- `pick --json`: `kind` on every row, existing field names untouched, Claude rows first.
- Classifier: real limit line and real 401 line covered, `try again at` parses with and without
  a date, `Reconnecting... n/5` matches nothing, prose about limits on a successful turn stays
  `outOK`.
- Panel and settings at 390px and 1440px (static harness feeding the Wails `state` event, four
  screenshots): two labelled groups appear only when a Codex account exists, Codex rows are not
  clickable, the forget control stays, both order lists render per kind, project lists mark
  `(codex)`. The settings table wraps at 390px, but the window's minimum width is 520px so that
  viewport cannot occur. The impeccable detector's findings (10px footer buttons, phosphor glow,
  scanline stripes) all predate this task and belong to the CRT theme.
- No em dashes in added code or docs (the one in the diff is a moved pre-existing string).
  CLAUDE.md layout row, "Codex accounts" rule block and unverified list, and the README section
  are present as planned. No new module in `go.mod`.
- Impeccable ran degraded (single context, detector inline) because the reviewer role writes only
  the review file and screenshots; the design findings above are the reviewer's own.

VERDICT: REJECT
