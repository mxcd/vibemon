package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeClaude is a shell stand-in for the CLI: each run pops one "exit|stdout|stderr" line from the
// script file and appends its token and argv to a log, so a test can assert what exec did.
const fakeClaude = `#!/bin/sh
echo "token=$CLAUDE_CODE_OAUTH_TOKEN api=${ANTHROPIC_API_KEY:-unset} args=$*" >> "$FAKE_LOG"
line=$(head -n 1 "$FAKE_SCRIPT")
tail -n +2 "$FAKE_SCRIPT" > "$FAKE_SCRIPT.tmp" && mv "$FAKE_SCRIPT.tmp" "$FAKE_SCRIPT"
code=$(printf '%s' "$line" | cut -d'|' -f1)
printf '%s' "$line" | cut -d'|' -f2
printf '%s' "$line" | cut -d'|' -f3 >&2
exit "${code:-0}"
`

type fakeRun struct {
	dir, log, script string
}

func setupFake(t *testing.T, lines ...string) fakeRun {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("VIBEMON_STATE", filepath.Join(dir, "state"))
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api-must-not-leak")
	f := fakeRun{dir: dir, log: filepath.Join(dir, "log"), script: filepath.Join(dir, "script")}
	t.Setenv("FAKE_LOG", f.log)
	t.Setenv("FAKE_SCRIPT", f.script)
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(fakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.script, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	vaultLoader = func() (Vault, error) { return testVault(), nil }
	emptyRetry = 0
	t.Cleanup(func() { vaultLoader = loadVault; emptyRetry = 30 * time.Second })
	return f
}

func (f fakeRun) attempts(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(f.log)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

// runExec captures what exec prints on stdout.
func runExec(t *testing.T, args ...string) (code int, stdout string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- b.String()
	}()
	code = cmdExec(args)
	w.Close()
	os.Stdout = old
	return code, <-done
}

func TestExecResumesUnderNextAccountOnLimit(t *testing.T) {
	f := setupFake(t,
		`1|{"is_error":true,"result":"You've hit your session limit · resets 9:40am (Europe/Rome)"}|`,
		`0|{"result":"done"}|`,
	)
	code, out := runExec(t, "--workdir", f.dir, "--session", "11111111-1111-4111-8111-111111111111",
		"--", filepath.Join(f.dir, "claude"), "-p", "do the thing")
	if code != 0 || !strings.Contains(out, `"done"`) {
		t.Fatalf("want exit 0 with the final output, got %d %q", code, out)
	}
	at := f.attempts(t)
	if len(at) != 2 {
		t.Fatalf("want 2 attempts, got %v", at)
	}
	if !strings.Contains(at[0], "token=sk-ant-oat01-a") || !strings.Contains(at[0], "--session-id 11111111") {
		t.Errorf("first attempt must start the session under the first account: %s", at[0])
	}
	if !strings.Contains(at[1], "token=sk-ant-oat01-b") || !strings.Contains(at[1], "--resume 11111111") || !strings.Contains(at[1], "do the thing") {
		t.Errorf("second attempt must resume under the next account with the prompt: %s", at[1])
	}
	if strings.Contains(at[0], "api=sk-ant") {
		t.Error("ANTHROPIC_API_KEY leaked into the child; it would outrank the OAuth token")
	}
	st := readFleet()
	b, ok := st.benched("a")
	if !ok || !strings.Contains(b.Reason, "session limit") {
		t.Errorf("account a must be benched with the limit message, got %+v", b)
	}
	if st.turnsLastHour("a") != 1 || st.turnsLastHour("b") != 1 {
		t.Errorf("one turn per account expected, got %+v", st.Turns)
	}
}

func TestExecFallsBackAModelOnCap(t *testing.T) {
	f := setupFake(t,
		`1|{"is_error":true,"result":"You've reached your Fable limit · resets 3pm"}|`,
		`0|ok|`,
	)
	code, _ := runExec(t, "--workdir", f.dir, "--model", "fable", "--", filepath.Join(f.dir, "claude"), "-p", "x")
	if code != 0 {
		t.Fatalf("want exit 0, got %d", code)
	}
	at := f.attempts(t)
	if len(at) != 2 || !strings.Contains(at[1], "token=sk-ant-oat01-a") || !strings.Contains(at[1], "--model opus") || !strings.Contains(at[1], "--resume") {
		t.Fatalf("second attempt must stay on the account, resume, and drop to opus: %v", at)
	}
	if strings.Contains(at[1], "--model fable") {
		t.Error("the caller's --model must be replaced, not duplicated")
	}
}

func TestExecStartsOverWhenTheTranscriptNeverExisted(t *testing.T) {
	f := setupFake(t,
		`1|You've hit your session limit · resets 9:40am|`,
		`1||No conversation found with session ID 1111`,
		`0|fresh|`,
	)
	code, _ := runExec(t, "--workdir", f.dir, "--", filepath.Join(f.dir, "claude"), "-p", "x")
	if code != 0 {
		t.Fatalf("want exit 0, got %d", code)
	}
	at := f.attempts(t)
	if len(at) != 3 || !strings.Contains(at[1], "--resume") || !strings.Contains(at[2], "--session-id") {
		t.Fatalf("resume must give way to a fresh session: %v", at)
	}
	first := strings.SplitAfter(at[0], "--session-id ")[1]
	if strings.Contains(at[2], first) {
		t.Error("the fresh session must use a new id")
	}
}

func TestExecRetriesEmptyOutputAndPropagatesExitCodes(t *testing.T) {
	f := setupFake(t, `0||`, `0|there|`)
	if code, out := runExec(t, "--workdir", f.dir, "--", filepath.Join(f.dir, "claude"), "-p", "x"); code != 0 || strings.TrimSpace(out) != "there" {
		t.Fatalf("empty then ok: want 0 %q, got %d %q", "there", code, out)
	}
	if at := f.attempts(t); len(at) != 2 || !strings.Contains(at[1], "token=sk-ant-oat01-a") {
		t.Fatalf("empty output must retry the same account: %v", at)
	}

	f = setupFake(t, `1|{"is_error":true,"result":"Prompt is too long"}|`)
	if code, _ := runExec(t, "--workdir", f.dir, "--", filepath.Join(f.dir, "claude"), "-p", "x"); code != exitTooLong {
		t.Fatalf("context exhaustion must exit %d, got %d", exitTooLong, code)
	}

	// A child's exit 2 must not masquerade as "no headroom".
	f = setupFake(t, `2|nope|boom`)
	if code, _ := runExec(t, "--workdir", f.dir, "--", filepath.Join(f.dir, "claude"), "-p", "x"); code != 1 {
		t.Fatalf("a plain failure must exit 1, got %d", code)
	}
	if at := f.attempts(t); len(at) != 1 {
		t.Fatalf("a failure with stderr must not be retried as empty output: %v", at)
	}
}

func TestExecReportsWhenEveryAccountIsBenched(t *testing.T) {
	f := setupFake(t)
	_, err := updateFleet(func(st *fleetState) {
		for _, k := range []string{"a", "b", "c"} {
			st.Bench[k] = benchEntry{Until: time.Now().Add(time.Hour), Reason: "limit"}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	code, out := runExec(t, "--workdir", f.dir, "--json", "--", filepath.Join(f.dir, "claude"), "-p", "x")
	if code != exitNoHeadroom || !strings.Contains(out, "no account has headroom") {
		t.Fatalf("want exit %d with a JSON error, got %d %q", exitNoHeadroom, code, out)
	}
	if _, err := os.Stat(f.log); err == nil {
		t.Error("nothing should have run")
	}
}

// The README form: claude's own flags right after exec's, no "--" needed.
func TestParseExecArgsStopsAtClaudeFlags(t *testing.T) {
	o, err := parseExecArgs([]string{"--model", "opus", "--json", "-p", "run the tests", "--output-format", "json"})
	if err != nil {
		t.Fatal(err)
	}
	if o.Model != "opus" || !o.JSON {
		t.Errorf("wrapper flags not parsed: %+v", o)
	}
	if strings.Join(o.Command, " ") != "claude -p run the tests --output-format json" {
		t.Errorf("child args mangled: %v", o.Command)
	}
	if !hasPositionalPrompt(o.Command) {
		t.Error("prompt positional not detected")
	}
	if hasPositionalPrompt([]string{"claude", "-p", "--model", "opus", "--output-format", "json"}) {
		t.Error("flag values mistaken for a prompt")
	}
}

func TestBuildArgsOwnsSessionAndModelFlags(t *testing.T) {
	got := buildArgs([]string{"claude", "-p", "hi", "--session-id", "old", "--model=fable"}, "new", true, "opus")
	want := "claude -p hi --resume new --model opus"
	if strings.Join(got, " ") != want {
		t.Errorf("want %q, got %q", want, strings.Join(got, " "))
	}
	sid, resume := sessionFromArgs([]string{"claude", "--resume", "abc", "-p", "x"})
	if sid != "abc" || !resume {
		t.Errorf("caller's --resume must be honoured, got %q %v", sid, resume)
	}
}
