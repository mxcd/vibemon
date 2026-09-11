# Plan: headless multi-account runs with automatic switching

> **Status 08.09.2026: implemented** (`exec.go`, `fleet.go`, `limits.go`, `prefs.go`, settings
> window), with three departures from the design below, all forced by one finding: a
> `claude setup-token` token is inference-only, so it can neither poll usage nor identify itself.
>
> 1. Accounts carry a separate headless token (`vibemon add-token`), stored in the vault. `exec`
>    runs only on those; it never hands a login's access token to a child and never refreshes.
> 2. Account choice is an ordered per-project policy (settings page), not a headroom sort. Order
>    is priority; usage numbers (from the monitor's polling of the captured login, via `fleet.json`)
>    and benches only skip entries. `--prefer` became `--account`.
> 3. `exec` without `-p` is an interactive pass-through, so `vibemon exec` in a project directory
>    is the per-project account switch. `token`/`pick` exist; fleet mode does not.
>
> `apiKeyHelper` was tested as an alternative broker pattern and rejected: Claude Code sends its
> output as an API key and Anthropic answers 401 for an OAuth token.

Written 08.09.2026 after the Paloma One overnight build (asolabs/paloma-one,
`scripts/agents/run.sh`), where up to 15 headless `claude -p` agents ran for 27 hours across five
accounts. The rotation logic lived in a bash script and learned every fact the hard way; vibemon
already owns the two things that script lacked: real usage numbers per account and refreshable
tokens. This plan moves the runner logic into vibemon.

## What exists today

- `accounts.go`: a vault of captured accounts with `OAuth{AccessToken, RefreshToken, ExpiresAt}`,
  `switchTo` for the interactive CLI (keychain blob + `~/.claude.json`), `usageFor` that refreshes
  parked accounts' tokens and polls `/api/oauth/usage` (session, weekly and per-model windows with
  reset times), `penalise`/`benched` backoff on failed polls.
- `gui.go`: auto-switch of the interactive account at 95 % with 80 % headroom on the candidate.
- CLI: `capture`, `list`, `remove`, `switch`, `usage`.

## What the bash runner had to invent, and what it got wrong first

1. A token pool in `.env` (`CLAUDE_ACCOUNT_TOKEN_n`, long-lived oauth tokens without the
   `user:profile` scope, so usage could not be polled) and health learned from failures.
2. Limit detection from the CLI's JSON (`is_error`, `result: "You've hit your session limit ·
   resets 9:40am (Europe/Rome)"`), parsing the reset time out of prose. A stale log tail was
   mistaken for a limit once and marked every account exhausted with a bogus reset.
3. Resume of the same `--session-id` under another account (works: transcripts are local, auth is
   separate; memory survives the switch).
4. Per-model caps ("You've reached your Fable limit") are not account limits: fall back to the
   next model, keep the account.
5. "No conversation found with session ID" means the id was recorded before a transcript existed:
   start over with the original prompt, do not retry the resume.
6. Empty CLI output with no limit named is a transient launch failure: retry the same account.
7. Load spreading: with lowest-number-first, one account carried 66 turns while another carried one;
   spreading by recent turns per account kept all five alive longer.
8. Locks: one turn per session id (a second `--resume` on the same id corrupts nothing but wastes a
   turn) and one turn per worktree (a reviewer and an implementer building and running Playwright in
   the same tree produced half-reviewed states).
9. A Haiku probe passing does not mean an Opus turn will pass; the limit message names the window.
10. Runners must survive the orchestrator: `nohup`, state on disk, resume by name.

## Design

### 1. `vibemon exec`: one turn of a headless command under the best account

```
vibemon exec [--session <uuid>] [--workdir <dir>] [--model fable|opus|sonnet|haiku]
             [--prefer <email>] [--exclude <email>...] [--json] -- claude -p ... 
```

- Picks the account with the most headroom from *usage numbers* (`usageFor` on every vault
  account, cached for 60 s): lowest of the session and weekly percentages, both under a configurable
  ceiling (default 90 %), respecting per-model windows when `--model` is given and the account has a
  per-model limit in `limits[]`. Ties break by fewest turns in the last hour (kept in
  `~/Library/Application Support/vibemon/turns.json`), then by `--prefer`.
- Runs the command with `CLAUDE_CODE_OAUTH_TOKEN=<fresh access token>` (refresh a parked account's
  token first when `expiresSoon`; never refresh the interactive account's token, rule 3 in CLAUDE.md;
  for the interactive account read the keychain token as `usageFor` does).
- Watches the child's stdout and stderr:
  - a limit in the JSON result or on stderr (`session limit`, `usage limit`, `weekly limit`,
    `resets <time> (<zone>)`) benches the account until the named reset (parsed with the zone;
    default `+1h`), marks the turn `retry`, and reruns the same command with `--resume <session>`
    under the next account. If the first attempt produced no output tokens the original prompt is
    resent instead of a "continue" message.
  - a per-model cap (`reached your <Model> limit`) reruns on the same account with the next model
    in `fable > opus > sonnet > haiku` (or the caller's `--fallback` list).
  - `No conversation found with session ID` starts a new session id and resends the prompt.
  - empty output with nothing named: retry the same account after 30 s, five times, then fail.
  - `Prompt is too long`: terminal; exit code 3 so the caller starts a fresh session with a
    compact brief (the runner must not loop on it).
- Exit codes: 0 done, 1 command failed, 2 no account has headroom (prints the earliest reset),
  3 context exhausted. `--json` prints `{account, model, attempts, session, result}`.
- Locks: `--session` acquires `<state>/locks/<uuid>`; `--workdir` acquires
  `<state>/locks/wt-<sha1(abs path)>`. A second `exec` on the same lock waits and reports
  `queued behind <pid>`; a dead pid's lock is reclaimed.

### 2. `vibemon pick` and `vibemon token`

- `vibemon pick [--model m] [--json]` prints the account `exec` would choose, with its headroom
  and reset times. Scripts that must run the CLI themselves use it.
- `vibemon token <email>` prints a fresh access token for scripts (refreshing a parked account if
  needed). Never prints the interactive account's token unless `--allow-active`.

### 3. `vibemon daemon` state and the panel

- `exec` records every attempt (account, model, outcome, reset) in `turns.json`; the panel's
  account list gains a "headless turns (1h)" column and a "benched until" badge fed by the same
  file, so a human sees what the fleet is doing to each account.
- Auto-rotation of the interactive account must never pick an account the fleet is draining:
  `exec` turns count toward the candidate's headroom (the 80 % rule already exists; feed it the
  per-account turn rate as a second input).

### 4. Reset-time parsing

One function, tested: `parseReset("resets 9:40am (Europe/Rome)")`, `"resets 2:40pm"`, `"resets
Sep 14th, 2026 12:22 AM"` (Codex style, keep for the future), zone default from the account's
usage response (`resets_at` is authoritative when present: prefer the API's `resets_at` over the
prose whenever the usage endpoint answers).

### 5. Fleet mode (later)

`vibemon fleet <manifest.yaml>`: a list of named agents (prompt file, model, workdir, session
persistence path), each run through `exec` in a loop with resume-on-limit and a per-agent status
file, plus a `vibemon fleet status` table. This is `run.sh` + `shard.sh` as a subcommand; do it
only once `exec` has run a real workload for a week.

## Tests

- `exec` against a fake `claude` script that emits the limit JSON, the model-cap message, the
  missing-transcript error, empty output, and a normal result; assert account choice, benching,
  resume args, model fallback, exit codes.
- `pick` on a synthetic vault with usage fixtures (session 95 %, weekly 50 %, per-model windows).
- Lock contention: two `exec` on one session id, the second waits; dead pid reclaimed.
- `parseReset` table.
- No test ever writes the real keychain; the vault path and the state dir are injectable.

## Order of work

1. `parseReset` + limit classifier (pure, tested) - half a day.
2. `pick` + `token` (reuse `usageFor`, `refreshToken`) - half a day.
3. `exec` with resume, benching, model fallback, locks, `turns.json` - one to two days.
4. Panel column and badge - half a day.
5. Fleet mode - later, after real use.

## Non-goals

Orchestration of review loops, git worktrees or merges (that stays with the project's scripts);
Codex account handling (its CLI reports limits differently and has one account).
