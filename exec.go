package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Exit codes of `vibemon exec`. A child's own failure is always reported as 1 so that a driver can
// tell "the turn failed" from these two.
const (
	exitNoHeadroom = 2 // every account is benched or spent; the earliest return is printed
	exitTooLong    = 3 // context exhausted: the caller must start over with a compact brief
)

// Test seams: the vault lives in the keychain and the retry pause is 30 s.
var (
	vaultLoader = loadVault
	emptyRetry  = 30 * time.Second
	maxAttempts = 20
)

type execOptions struct {
	pickOptions
	Session  string
	Wait     bool
	JSON     bool
	Fallback []string
	Command  []string
}

// wrapperFlags are ours (value: whether the flag takes an argument). Parsing stops at the first
// argument that is not one of them, so the child's own flags (-p above all) pass through and `--`
// is optional.
var wrapperFlags = map[string]bool{
	"session": true, "workdir": true, "model": true, "account": true, "exclude": true,
	"ceiling": true, "fallback": true, "kind": true, "wait": false, "json": false,
}

func splitWrapperArgs(args []string) (ours, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return ours, args[i+1:]
		}
		name, _, inline := strings.Cut(strings.TrimLeft(a, "-"), "=")
		takesValue, known := wrapperFlags[name]
		if !strings.HasPrefix(a, "-") || !known {
			return ours, args[i:]
		}
		ours = append(ours, a)
		if takesValue && !inline && i+1 < len(args) {
			i++
			ours = append(ours, args[i])
		}
	}
	return ours, nil
}

func parseExecArgs(args []string) (execOptions, error) {
	var o execOptions
	var exclude, fallback string
	ours, rest := splitWrapperArgs(args)
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.Kind, "kind", "", "account kind: claude or codex (pick also takes all)")
	fs.StringVar(&o.Session, "session", "", "session id to create or resume")
	fs.StringVar(&o.Dir, "workdir", "", "run in this directory (default: current)")
	fs.StringVar(&o.Model, "model", "", "model to run; per-model caps fall back down the list")
	fs.StringVar(&o.Only, "account", "", "force this account (email)")
	fs.StringVar(&exclude, "exclude", "", "comma-separated emails to skip")
	fs.Float64Var(&o.Ceiling, "ceiling", 90, "usage percent at which an account counts as spent")
	fs.StringVar(&fallback, "fallback", strings.Join(modelOrder, ","), "model fallback order")
	fs.BoolVar(&o.Wait, "wait", false, "sleep until an account comes back instead of exiting 2")
	fs.BoolVar(&o.JSON, "json", false, "print {account, model, attempts, session, result}")
	if err := fs.Parse(ours); err != nil {
		return o, err
	}
	if exclude != "" {
		o.Exclude = strings.Split(exclude, ",")
	}
	o.Fallback = strings.Split(strings.ToLower(fallback), ",")
	o.Command = rest
	switch o.Kind {
	case "", kindClaude, kindCodex, kindAll:
	default:
		return o, fmt.Errorf("unknown --kind %q: claude, codex or all", o.Kind)
	}
	if o.Kind == kindCodex {
		// codex takes its model as -m inside its own argv and keeps rollouts per CODEX_HOME, so
		// neither flag has anything to act on here; silently ignoring them would mislead.
		if o.Model != "" {
			return o, fmt.Errorf("--model does not apply to --kind codex: pass -m to codex itself")
		}
		if o.Session != "" {
			return o, fmt.Errorf("--session does not apply to --kind codex: a review is one turn, nothing resumes")
		}
	}
	if len(o.Command) == 0 || strings.HasPrefix(o.Command[0], "-") {
		o.Command = append([]string{defaultCommand(o.Kind)}, o.Command...)
	}
	if o.Dir == "" {
		o.Dir, _ = os.Getwd()
	}
	return o, nil
}

// defaultCommand is what exec runs when the caller gave only flags.
func defaultCommand(kind string) string {
	if kind == kindCodex {
		return "codex"
	}
	return "claude"
}

func execUsage() string {
	return `usage: vibemon exec [flags] [--] [claude args...]

  --kind claude|codex account kind to run under (claude)
  --session <uuid>   session id to create (or resume, on a limit); default: fresh
  --workdir <dir>    run there; also picks the project policy (default: cwd)
  --model <name>     model to run; a per-model cap falls back down --fallback
  --account <email>  force one account
  --exclude a,b      skip these accounts
  --ceiling <pct>    treat an account as spent at this usage percent (90)
  --fallback f,o,s,h model fallback order (fable,opus,sonnet,haiku)
  --wait             sleep until an account comes back instead of exiting 2
  --json             print {account, model, attempts, session, result}

Without a command, runs an interactive "claude" under the project's first available account.
With "-p", buffers the output, resumes the session under the next account on a limit, and
exits 0 done, 1 failed, 2 no account has headroom, 3 context exhausted.

With --kind codex, runs codex under the ChatGPT account with the most headroom, with that
account's CODEX_HOME set. "codex exec" is the headless form; on a limit the whole command
reruns under the next account, because a review is one turn and nothing resumes.`
}

type attempt struct {
	Account string `json:"account"`
	Model   string `json:"model,omitempty"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
}

// cmdExec runs one command under the best account for the working directory. Interactive runs are
// a straight pass-through; headless (-p) runs get the limit-and-resume loop.
func cmdExec(args []string) int {
	o, err := parseExecArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, execUsage())
		return 1
	}
	v, err := vaultLoader()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if o.Kind == kindAll {
		fmt.Fprintln(os.Stderr, "error: exec runs one kind: --kind claude or --kind codex")
		return 1
	}
	if o.Kind == "" {
		o.Kind = kindClaude
	}
	p := loadPrefs()
	if isHeadless(o.Kind, o.Command) {
		return runHeadless(v, p, o)
	}
	return runInteractive(v, p, o)
}

// isHeadless tells a turn that can be retried elsewhere from an interactive session. For codex that
// is the "exec" subcommand; its -p is --profile, not --print.
//
// ponytail: a Codex prompt that is the single word "exec" would be misread as the subcommand.
// Upgrade path: walk the argv properly instead of asking whether the word is in it.
func isHeadless(kind string, cmd []string) bool {
	if kind == kindCodex {
		return len(cmd) > 1 && slices.Contains(cmd[1:], "exec")
	}
	return slices.Contains(cmd, "-p") || slices.Contains(cmd, "--print")
}

// claudeValueFlags are the CLI's flags that take an argument, so a positional prompt can be told
// apart from a flag value. Incomplete on purpose: an unknown value flag only means a prompt-less
// invocation reads stdin one time too few, never a hang.
var claudeValueFlags = map[string]bool{
	"--model": true, "--session-id": true, "--resume": true, "-r": true, "--output-format": true,
	"--input-format": true, "--settings": true, "--mcp-config": true, "--allowedTools": true,
	"--disallowedTools": true, "--permission-mode": true, "--append-system-prompt": true,
	"--append-system-prompt-file": true, "--system-prompt": true, "--json-schema": true,
	"--agents": true, "--max-turns": true, "--add-dir": true, "--fallback-model": true,
	"--permission-prompt-tool": true, "--max-budget-usd": true, "--effort": true, "--betas": true,
}

// codexValueFlags is the same table for codex. -o and -p above all: reading "-o out.md" as a prompt
// would skip the piped one.
var codexValueFlags = map[string]bool{
	"-m": true, "--model": true, "-c": true, "--config": true, "-s": true, "--sandbox": true,
	"-p": true, "--profile": true, "-o": true, "--output-last-message": true, "-C": true, "--cd": true,
	"-i": true, "--image": true, "--add-dir": true, "--output-schema": true, "--enable": true,
	"--disable": true, "--local-provider": true, "--thread-source": true, "--color": true,
}

// codexSubcommands are the words between "codex" and its prompt; they are not the prompt.
var codexSubcommands = map[string]bool{"exec": true, "resume": true, "fork": true, "review": true}

func hasPositionalPrompt(kind string, cmd []string) bool {
	valueFlags, skip := claudeValueFlags, map[string]bool(nil)
	if kind == kindCodex {
		valueFlags, skip = codexValueFlags, codexSubcommands
	}
	for i := 1; i < len(cmd); i++ {
		a := cmd[i]
		if a == "--" {
			return i+1 < len(cmd)
		}
		if strings.HasPrefix(a, "-") {
			if valueFlags[a] {
				i++
			}
			continue
		}
		if skip[a] {
			continue
		}
		return true
	}
	return false
}

// runChild starts cmd and relays SIGINT/SIGTERM to it, so killing the wrapper never leaves an
// orphaned claude holding a worktree whose lock just evaporated. Reports whether the wrapper itself
// was told to stop.
func runChild(cmd *exec.Cmd) (err error, interrupted bool) {
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)
	if err := cmd.Start(); err != nil {
		return err, false
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case sig := <-sigs:
			interrupted = true
			_ = cmd.Process.Signal(sig)
		case err := <-done:
			return err, interrupted
		}
	}
}

func runInteractive(v Vault, p prefs, o execOptions) int {
	st := readFleet()
	runnable, skipped, project := rank(v, p, &st, o.pickOptions)
	if len(runnable) == 0 {
		return reportNoHeadroom(skipped, o, "", nil, "")
	}
	c := runnable[0]
	fmt.Fprintf(os.Stderr, "vibemon: %s%s\n", c.Account.Email, projectNote(project))
	argv := o.Command
	if c.Account.kind() == kindClaude && o.Model != "" && flagValue(argv, "--model") == "" {
		argv = append(slices.Clone(argv), "--model", o.Model)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = o.Dir
	cmd.Env = c.Account.childEnv()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err, _ := runChild(cmd)
	recordTurn(c.Account, o.Model, "interactive", nil, o.Dir)
	return exitCodeOf(err)
}

func runHeadless(v Vault, p prefs, o execOptions) int {
	// A prompt on stdin is consumed once and replayed to every attempt. Only read it when the
	// command carries no prompt of its own: draining an inherited pipe would eat a driver loop's
	// remaining lines, or block forever on one that never closes. Read before taking any lock.
	var stdin []byte
	if !hasPositionalPrompt(o.Kind, o.Command) {
		if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice == 0 {
			stdin, _ = io.ReadAll(os.Stdin)
		}
	}

	release, err := acquireLock(worktreeLockName(o.Dir))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer release()

	// A Codex turn owns no session: rollouts live inside the account's own CODEX_HOME, so a retry
	// under the next account reruns the whole command rather than resuming anything.
	var sid string
	var resume bool
	if o.Kind != kindCodex {
		sid, resume = sessionFromArgs(o.Command)
		if o.Session != "" {
			sid = o.Session
		}
		if sid == "" {
			sid = newUUID()
		}
		release2, err := acquireLock(sessionLockName(sid))
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		defer release2()
	}

	model := o.Model
	if model == "" && o.Kind != kindCodex {
		model = flagValue(o.Command, "--model")
	}
	var attempts []attempt
	emptyRetries, newSessions := 0, 0
	var lastOut, lastErr string
	// Benches this process decided on, applied even when the ledger write fails: otherwise the
	// same rejected account would be picked again on the very next iteration.
	localBench := map[string]benchEntry{}
	for len(attempts) < maxAttempts {
		st := readFleet()
		for k, b := range localBench {
			if cur, ok := st.Bench[k]; !ok || b.Until.After(cur.Until) {
				st.Bench[k] = b
			}
		}
		runnable, skipped, project := rank(v, p, &st, pickOptions{Dir: o.Dir, Kind: o.Kind, Model: model, Only: o.Only, Exclude: o.Exclude, Ceiling: o.Ceiling})
		if len(runnable) == 0 {
			if until, ok := earliestReturn(skipped); ok && o.Wait && until.After(time.Now()) {
				fmt.Fprintf(os.Stderr, "vibemon: no account has headroom, sleeping until %s\n", until.Local().Format("02.01.2006 15:04:05"))
				time.Sleep(time.Until(until) + time.Second)
				continue
			}
			return reportNoHeadroom(skipped, o, sid, attempts, lastOut)
		}
		c := runnable[0]
		argv := o.Command
		if c.Account.kind() == kindClaude {
			argv = buildArgs(o.Command, sid, resume, model)
		}
		fmt.Fprintf(os.Stderr, "vibemon: attempt %d as %s%s%s\n", len(attempts)+1, c.Account.Email,
			modelNote(model), projectNote(project))

		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = o.Dir
		cmd.Env = c.Account.childEnv()
		if len(stdin) > 0 {
			cmd.Stdin = bytes.NewReader(stdin) // nil means /dev/null: the CLI waits 3 s on an empty pipe
		}
		var out, errb bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = io.MultiWriter(&errb, os.Stderr)
		runErr, interrupted := runChild(cmd)
		lastOut, lastErr = out.String(), errb.String()
		if interrupted {
			os.Stdout.WriteString(lastOut)
			return 130
		}
		code := exitCodeOf(runErr)
		kind, detail := classify(lastOut, lastErr, code)
		if errors.Is(runErr, exec.ErrNotFound) {
			kind, detail = outFailed, runErr.Error()
		}
		attempts = append(attempts, attempt{Account: c.Account.Email, Model: model, Outcome: kind.String(), Detail: truncate(detail, 200)})
		// Whatever happened, the id may have a transcript now; resuming is right, and a resume of
		// an id that never got one comes back as outNoSession and starts over below.
		resume = true

		var reset *time.Time
		bench := func(until time.Time, reason string) {
			reset = &until
			localBench[c.Account.UUID] = benchEntry{Until: until, Reason: truncate(reason, 90)}
			benchAccount(c.Account.UUID, until, reason)
		}
		switch kind {
		case outLimit:
			bench(benchFor(detail, time.Now()), detail)
		case outModelCap:
			if next := nextModel(detail, o.Fallback); next == "" {
				bench(benchFor(detail, time.Now()), "model cap: "+detail)
			} else {
				fmt.Fprintf(os.Stderr, "vibemon: %s capped, falling back to %s\n", detail, next)
				model = next
			}
		case outAuth:
			reason := "token rejected: replace with vibemon add-token"
			if c.Account.kind() == kindCodex {
				reason = "login rejected: vibemon codex add " + c.Account.Email
			}
			bench(time.Now().Add(24*time.Hour), reason)
		case outNoSession:
			newSessions++
			if newSessions > 2 {
				kind = outFailed
			}
			sid, resume = newUUID(), false
		case outEmpty:
			emptyRetries++
			if emptyRetries > 5 {
				kind = outFailed
			} else {
				time.Sleep(emptyRetry)
			}
		}
		recordTurn(c.Account, model, kind.String(), reset, o.Dir)

		switch kind {
		case outOK:
			emit(o, c.Account.Email, model, sid, attempts, lastOut)
			return 0
		case outTooLong:
			emit(o, c.Account.Email, model, sid, attempts, lastOut)
			return exitTooLong
		case outFailed:
			emit(o, c.Account.Email, model, sid, attempts, lastOut)
			return 1
		}
	}
	fmt.Fprintf(os.Stderr, "vibemon: giving up after %d attempts\n", len(attempts))
	os.Stdout.WriteString(lastOut)
	return 1
}

func emit(o execOptions, email, model, sid string, attempts []attempt, out string) {
	if !o.JSON {
		os.Stdout.WriteString(out)
		return
	}
	result := json.RawMessage(strings.TrimSpace(out))
	if !json.Valid(result) {
		result, _ = json.Marshal(out)
	}
	enc := json.NewEncoder(os.Stdout)
	_ = enc.Encode(map[string]any{
		"kind": o.Kind, "account": email, "model": model, "session": sid,
		"attempts": attempts, "result": result,
	})
}

func reportNoHeadroom(skipped []candidate, o execOptions, sid string, attempts []attempt, lastOut string) int {
	fmt.Fprintln(os.Stderr, "vibemon: no account has headroom")
	if len(skipped) == 0 {
		fmt.Fprintln(os.Stderr, "  the account policy for this directory names no stored account (check Settings)")
	}
	for _, c := range skipped {
		fmt.Fprintf(os.Stderr, "  %-34s %s%s\n", c.Account.Email, c.Reason, untilNote(c.Until))
	}
	until, ok := earliestReturn(skipped)
	if ok {
		fmt.Fprintf(os.Stderr, "earliest return: %s\n", until.Local().Format("02.01.2006 15:04"))
	}
	if o.JSON {
		out := map[string]any{"kind": o.Kind, "error": "no account has headroom", "session": sid, "attempts": attempts}
		if ok {
			out["earliestReturn"] = until
		}
		if lastOut != "" {
			out["result"] = lastOut
		}
		_ = json.NewEncoder(os.Stdout).Encode(out)
	} else if lastOut != "" {
		os.Stdout.WriteString(lastOut)
	}
	return exitNoHeadroom
}

// benchFor turns a limit message into a bench deadline: the named reset plus two minutes of slack
// for clock skew, or an hour when nothing parses.
func benchFor(msg string, now time.Time) time.Time {
	if t, ok := parseReset(msg, now, time.Local); ok {
		return t.Add(2 * time.Minute)
	}
	return now.Add(time.Hour)
}

// benchAccount extends an account's bench; a shorter deadline never cuts a longer one that another
// runner already recorded.
func benchAccount(key string, until time.Time, reason string) {
	_, err := updateFleet(func(st *fleetState) {
		if cur, ok := st.Bench[key]; ok && cur.Until.After(until) {
			return
		}
		st.Bench[key] = benchEntry{Until: until, Reason: truncate(reason, 90)}
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "vibemon: could not record bench:", err)
	}
}

func recordTurn(a *Account, model, outcome string, reset *time.Time, dir string) {
	_, err := updateFleet(func(st *fleetState) {
		st.Turns = append(st.Turns, turn{Account: a.UUID, Email: a.Email, Model: model, At: time.Now(),
			Outcome: outcome, Reset: reset, Project: dir})
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "vibemon: could not record turn:", err)
	}
}

// childEnv is what a child process gets in order to run as this account: a headless token for
// Claude, a home directory for Codex. Nothing else about the account leaves the process.
func (a *Account) childEnv() []string {
	if a.kind() == kindCodex {
		return codexChildEnv(codexHome(a.Email))
	}
	return claudeChildEnv(a.HeadlessToken)
}

// claudeChildEnv hands the child exactly one credential. ANTHROPIC_API_KEY silently outranks the
// OAuth token and would bill the API instead of the subscription; ANTHROPIC_AUTH_TOKEN would route
// to a gateway. Both go.
func claudeChildEnv(token string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		switch strings.SplitN(kv, "=", 2)[0] {
		case "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN":
			continue
		}
		env = append(env, kv)
	}
	return append(env, "CLAUDE_CODE_OAUTH_TOKEN="+token)
}

// buildArgs owns the session and model flags on the child: the user's copies are stripped and ours
// appended, so a retry can turn --session-id into --resume and swap the model.
//
// ponytail: a resume resends the original prompt rather than a "continue" message. The transcript
// carries the finished work, so the model picks up where it stopped; the price is a repeated prompt
// in the context. Upgrade path: detect the prompt positional and swap it for a continue message.
func buildArgs(cmd []string, sid string, resume bool, model string) []string {
	out := stripFlags(cmd, "--session-id", "--resume", "--model")
	if resume {
		out = append(out, "--resume", sid)
	} else {
		out = append(out, "--session-id", sid)
	}
	if model != "" {
		out = append(out, "--model", model)
	}
	return out
}

func stripFlags(cmd []string, names ...string) []string {
	out := make([]string, 0, len(cmd))
	for i := 0; i < len(cmd); i++ {
		name, _, inline := strings.Cut(cmd[i], "=")
		if slices.Contains(names, name) {
			if !inline {
				i++
			}
			continue
		}
		out = append(out, cmd[i])
	}
	return out
}

func flagValue(cmd []string, name string) string {
	for i, a := range cmd {
		if a == name && i+1 < len(cmd) {
			return cmd[i+1]
		}
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v
		}
	}
	return ""
}

// sessionFromArgs honours a session the caller already chose, resuming it if they said --resume.
func sessionFromArgs(cmd []string) (sid string, resume bool) {
	if v := flagValue(cmd, "--resume"); v != "" && !strings.HasPrefix(v, "-") {
		return v, true
	}
	return flagValue(cmd, "--session-id"), false
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if code := ee.ExitCode(); code >= 0 {
			return code
		}
		return 128 // killed by a signal
	}
	return 1
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func projectNote(p *projectPolicy) string {
	if p == nil {
		return ""
	}
	return " (project " + p.Path + ")"
}

func modelNote(model string) string {
	if model == "" {
		return ""
	}
	return " with " + model
}

func untilNote(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return " until " + t.Local().Format("15:04")
}
