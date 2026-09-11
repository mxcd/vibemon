# Review: Codex (ChatGPT) accounts beside Claude accounts

Task `aso/vibemon/codex-accounts`, branch `task/codex-accounts` (`a27e0ed..205b640`, 10 commits on
`main`), reviewed 11.09.2026 against `docs/plan/impl/codex-accounts.md`.
Reviewer: Fable 5.1. Codex round 1: gpt-6-astra high, ran to completion (no quota limit), verdict
REJECT with 3 MAJOR and 2 MINOR; all five were re-verified by hand below, one reclassified.
Round 2 (re-review of fix commit `16dc5cc`, 11.09.2026 13:15): Codex round 2 ran to completion,
two MINOR, see "Round 2" at the end. Final verdict APPROVE with three open MINORs for a
follow-up commit.
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

Round 1 rejected it for one reason: `vibemon codex add` could move a directory CLAUDE.md says is
never moved (typed email used as a path segment, plus a rename of a user-named home on a mismatched
login). Commit `16dc5cc` closes both with `codexEmailOK` and `placeCodexHome`, each with a test that
pins the concrete `../../.codex` traversal, and answers every other round 1 finding. Round 2
verified each resolution in code and in the re-shot screens, reran the gates (all green, 19
Codex-related tests pass) and found three MINORs, none a correctness or security defect, each a
one-line fix. Approved; the three are listed under "Round 2" for the follow-up.

## Findings

Resolution filled in by the implementer on 11.09.2026, commit `fix(codex): ...` below; every
finding is addressed in code or docs.

- **MAJOR** `main.go:208` - the typed email is a path segment without validation; `../../.codex`
  resolves to `~/.codex` and the rename at line 234 can move MaPa's interactive login, which
  CLAUDE.md forbids. Fix: reject an email without `@` or containing `/`, `..` or a path separator
  before it reaches `codexHome`, in both `cmdCodexAdd` and `registerCodex` (the endpoint's email
  is also a path segment).
  **Resolution: fixed.** `codexEmailOK` (`codex.go`) rejects an address without `@` and one holding
  `/`, `\`, `..`, leading or trailing space, or anything `filepath.Base` shortens. It runs on the
  typed argument in `cmdCodexAdd`, on the reported address in `registerCodex`, and inside
  `placeCodexHome`. `TestCodexEmailOK` covers both lists and asserts the concrete
  `../../.codex` traversal is what the guard stands in front of.
- **MAJOR** `main.go:234` - a home the user named (`codex add <email>`) is renamed when the login
  turns out to be a different account, so the wrong account silently takes over a home and its
  sessions. The plan said to error on mismatch. Fix: rename only when `home` is the temporary
  `.login-<pid>` directory; otherwise return the plan's "logged in as %s, not %s" error and leave
  both homes as they are. Add a test for the mismatch path.
  **Resolution: fixed.** The placement decision moved into `placeCodexHome(home, temporary,
  reported, asked)` in `codex.go`: a home the user named is never renamed, the mismatch returns
  `"%s is logged in as %s, not %s; nothing was moved"`, and only a `.login-<pid>` home moves, never
  over an existing one. `TestPlaceCodexHome` covers match, mismatch (and that the named home is
  still on disk afterwards), move, collision and a traversing address.
- **MINOR** `accounts.go:308` - the exact-key shortcut runs before the ambiguity scan, so with a
  Claude token stub keyed by its lowercased email and a Codex account for the same email, `remove
  a@x.io` resolves the Claude entry silently instead of erroring. Fix: run the email scan first
  and use the exact key only as a fallback, plus one case in `TestFindByEmailAcrossKinds`.
  (Codex rated this MAJOR; it needs a token-only Claude stub and the effect is a reversible
  forget, so MINOR.)
  **Resolution: fixed.** `findByEmail` scans addresses first and falls back to the exact vault key
  only when no address matched, so the stub case now errors and `codex:a@x.io` still resolves.
  `TestFindByEmailAcrossKinds` gained the stub vault.
- **MINOR** `main.go:197` - the vault flock is held across the interactive `codex login`. The
  monitor's `pollOnce` takes `m.mu` and then `lockVault()`, so the panel freezes for the whole
  browser round trip. Fix: probe and log in before `lockVault()`, take the lock only around
  `loadVault` and `registerCodex`.
  **Resolution: fixed.** `cmdCodexAdd` probes, logs in and places the home with no lock held;
  `lockVault()` is taken immediately before `loadVault` and covers only that and `registerCodex`.
- **MINOR** `codex.go:284` - `appendCodexOrder` runs on every registration, so re-running `codex
  add <email>` after a re-auth puts an account the user unticked in Settings back into
  `codexOrder`. Fix: append only when `v[key]` was nil.
  **Resolution: fixed.** `registerCodex` remembers whether the key was already in the vault and
  appends to the order only on a first registration.
- **MINOR** `main.go:218` - a failed or aborted `codex login` leaves `.login-<pid>` behind, and
  the name-collision branch leaves a second logged-in `auth.json` in it. Fix: `os.RemoveAll` the
  temporary home on every error path, as the plan specified.
  **Resolution: fixed.** One `defer` removes the temporary home on every path that does not file
  it, including the collision, and says so on stderr when it is discarding a login that did work,
  so the user knows to run the command again rather than hunt for the directory.
- **MINOR** `codex.go:195` - the access token is a 5-day JWT that only codex refreshes. An account
  idle for five days answers 401 on the poll, is flagged `NeedsReauth`, and `codex add <email>`
  then runs a browser login even though a plain `codex exec` would have refreshed it. No code
  change required now, but add it to the CLAUDE.md unverified list so the first "needs login" on
  a parked account is read correctly.
  **Resolution: documented.** Added to the unverified list in CLAUDE.md, naming the 5-day JWT, the
  401 it may produce on a parked account, and that a plain `codex exec` in that home refreshes it.
- **DESIGN** `frontend/index.html:202` - a Codex row's sub line reads `resets · 4d 7h` because the
  session gauge has no reset and the two countdowns are joined unconditionally; the stray dot
  reads as a broken field. Fix: collect the countdowns that have a `resetsAt` and join those.
  **Resolution: fixed.** The row builds one list of the windows the account actually has and joins
  only the countdowns among them, so a Codex row reads `resets 4d 7h`.
- **DESIGN** `frontend/index.html:211` - Codex rows keep the hollow active dot and the same
  `0% · 100%` numbers as Claude rows although neither "active" nor a session window exists for
  them, so a spent Codex account looks half healthy. Fix: `visibility: hidden` on the dot for
  `.acct.codex`, and render a dash for a gauge with `percent === 0` and no `resetsAt`.
  **Resolution: fixed**, with one change to the suggestion: the dot is `visibility: hidden` for
  `.acct.codex` as asked, but a window the account does not have is left out of the numbers rather
  than drawn as a dash, so a spent Codex row reads `100%` instead of `- · 100%`. A row whose
  windows are all genuinely at zero still reads `0%`.
- **DESIGN** `accounts.go:81` - `planLabel` yields `ChatGPT · ` with a trailing separator when
  `Plan` is empty (visible in the settings table). Fix: return `ChatGPT` alone when `Plan == ""`.
  **Resolution: fixed.** `planLabel` returns `ChatGPT` when the plan is unknown.

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

## Round 2 (re-review of `16dc5cc`)

Every round 1 resolution verified, in code and by rerunning the gates and re-shooting the screens:

- Traversal (MAJOR): `codexEmailOK` runs on the typed argument, the reported address and inside
  `placeCodexHome`; `TestCodexEmailOK` asserts the guard list and that `codexHome("../../.codex")`
  still resolves to the parent's `.codex`, so the test fails the day the guard becomes moot.
  Verified fixed.
- Rename of a named home (MAJOR): `placeCodexHome(home, temporary, reported, asked)` returns the
  plan's error on mismatch and moves only a `.login-<pid>` home, never over an existing one;
  `TestPlaceCodexHome` covers match, mismatch with the named home still on disk, move, collision
  and a traversing address. `registerCodex` keeps its own mismatch check as a second fence.
  Verified fixed.
- `findByEmail` order (MINOR): address scan first, exact key as fallback; the stub case is in
  `TestFindByEmailAcrossKinds` and `codex:a@x.io` still resolves by key. Verified fixed.
- Vault lock across login (MINOR): `lockVault()` now sits directly before `loadVault` and covers
  only that and `registerCodex`. Verified fixed.
- Order append on re-registration (MINOR): `registerCodex` appends only when the key was not in
  the vault. Verified fixed in code; not covered by a test (see round 2 finding 2).
- Temporary home cleanup (MINOR): one `defer` removes a temporary home on every path that does not
  file it and says so on stderr when it discards a working login. Verified fixed for every path
  the process lives through (see round 2 finding 1 for the one it does not).
- 5-day JWT (MINOR): added to the CLAUDE.md unverified list with the mechanism and the self-heal
  path. Verified documented.
- Stray separator, hollow dot, `ChatGPT · ` (DESIGN): re-shot at 390px and 1440px; a spent Codex
  row now reads `100%` with `resets 4d 7h`, the dot is hidden, the plan reads `ChatGPT`. Verified
  fixed.

Gates: `go build`, `go vet`, `gofmt -l` empty, `go test -count=1 ./...` passes.

Codex round 2 (gpt-6-astra high, exit 0, verdict REJECT on two MINORs): both confirmed as stated,
neither blocks. Open findings after round 2, all MINOR, for one follow-up commit:

- **MINOR** `frontend/index.html:201` - the "leave out a window the account does not have" rule
  keys on `percent > 0 || resetsAt`, so a Claude row whose 5 h window is idle and carries no reset
  time now reads `12%` alone while its neighbours read `34% · 61%`; the single number is ambiguous
  (session or weekly?) and the Claude group loses its uniform shape. Fix: apply the omission only
  for `a.kind === 'codex'`; Claude always has both windows.
- **MINOR** `main.go:211` - Ctrl-C during the browser login kills vibemon and the child
  together, and a signal does not run the cleanup defer, so `.login-<pid>` stays behind (without a
  login, since the flow never finished). Fix: sweep stale `.login-*` directories at the start of a
  bare `codex add`, which is smaller than a signal handler and also catches a crash. (Codex r2.)
- **MINOR** `codex.go:319` - the first-registration-only append to `codexOrder` has no test.
  `registerCodex` writes the vault through the keychain, which is why none exists; if a test is
  wanted, lift the `known` decision into a small pure helper and test that. Accepted without a
  test for now. (Codex r2.)

VERDICT: APPROVE
