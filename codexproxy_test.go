package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// pooled logs each email in under a temp homes dir, with a token tagged by its email that is good
// for a month, and returns them as a pool in that order.
func pooled(t *testing.T, emails ...string) []codexAccount {
	t.Helper()
	dir := t.TempDir()
	var pool []codexAccount
	for _, e := range emails {
		home := filepath.Join(dir, e)
		writeCodexAuth(t, home, fakeJWT(e, time.Now().Add(30*24*time.Hour)))
		raw, _ := os.ReadFile(filepath.Join(home, "auth.json"))
		raw = []byte(strings.Replace(string(raw), `"acct-1"`, `"acct-`+e+`"`, 1))
		if err := os.WriteFile(filepath.Join(home, "auth.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		pool = append(pool, codexAccount{Key: codexKey(e), Email: e, Home: home})
	}
	return pool
}

// tagOf names the fake token a request carries.
func tagOf(r *http.Request) string {
	parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
	if len(parts) != 3 {
		return "?"
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct{ Tag string }
	_ = json.Unmarshal(raw, &c)
	return c.Tag
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// codexProxyTo starts the Codex proxy in front of upstream. Benches go to a throwaway fleet.json;
// pool and now replace the vault, the real homes and the clock.
func codexProxyTo(t *testing.T, upstream http.HandlerFunc, pool func() ([]codexAccount, error), now func() time.Time) string {
	t.Helper()
	sandbox(t)
	t.Setenv("VIBEMON_CODEX_PROXY_ADDR", "")
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	u, err := url.Parse(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	p := httptest.NewServer(newCodexProxy(u, pool, now))
	t.Cleanup(p.Close)
	return p.URL
}

func poolOf(pool []codexAccount) func() ([]codexAccount, error) {
	return func() ([]codexAccount, error) { return pool, nil }
}

func postCodex(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestCodexProxyInjectsPooledAccount(t *testing.T) {
	logs := captureLog(t)
	var got *http.Request
	var body string
	base := codexProxyTo(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got, body = r, string(b)
	}, poolOf(pooled(t, "a@x.io")), time.Now)

	req, _ := http.NewRequest("POST", base+"/backend-api/codex/responses?x=1", strings.NewReader("\x28\xb5\x2f\xfdzstd"))
	req.Header.Set("Authorization", "Bearer own-token")
	req.Header.Set("ChatGPT-Account-Id", "own-account")
	req.Header.Set("Originator", "codex_cli_rs")
	req.Header.Set("Content-Encoding", "zstd")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if tag := tagOf(got); tag != "a@x.io" {
		t.Errorf("token upstream is %q's, want the pooled account's", tag)
	}
	if id := got.Header.Get("ChatGPT-Account-Id"); id != "acct-a@x.io" {
		t.Errorf("ChatGPT-Account-Id upstream = %q", id)
	}
	if got.URL.Path != "/backend-api/codex/responses" || got.URL.RawQuery != "x=1" || body != "\x28\xb5\x2f\xfdzstd" ||
		got.Header.Get("Originator") != "codex_cli_rs" || got.Header.Get("Content-Encoding") != "zstd" {
		t.Errorf("path, query, body or a header changed: %s?%s %q", got.URL.Path, got.URL.RawQuery, body)
	}
	if !strings.Contains(logs.String(), "POST /backend-api/codex/responses 200 as a@x.io") {
		t.Errorf("log line missing: %q", logs.String())
	}
	if strings.Contains(logs.String(), "eyJ") || strings.Contains(logs.String(), "own-token") {
		t.Error("a token reached the log")
	}
}

func TestCodexProxyStreamsIncrementally(t *testing.T) {
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	base := codexProxyTo(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, "data: two\n\n")
	}, poolOf(pooled(t, "a@x.io")), time.Now)

	resp, err := http.Post(base+"/backend-api/codex/responses", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	first := make(chan string, 1)
	br := bufio.NewReader(resp.Body)
	go func() {
		line, _ := br.ReadString('\n')
		first <- line
	}()
	select {
	case line := <-first:
		if line != "data: one\n" {
			t.Fatalf("first line = %q", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the first event was held back until the stream ended: the proxy buffers")
	}
	unblock()
	if rest, _ := io.ReadAll(br); string(rest) != "\ndata: two\n\n" {
		t.Errorf("rest of stream = %q", rest)
	}
}

// Sticky: a better-placed account does not take over a running session (its prompt cache lives on
// the account it started on); a bench recorded elsewhere, by exec say, does move it.
func TestCodexProxyStaysOnItsAccountUntilItFails(t *testing.T) {
	accounts := pooled(t, "a@x.io", "b@x.io")
	var mu sync.Mutex
	pool := accounts
	var seen []string
	clk := &clock{t: time.Now()}
	base := codexProxyTo(t, func(w http.ResponseWriter, r *http.Request) { seen = append(seen, tagOf(r)) },
		func() ([]codexAccount, error) { mu.Lock(); defer mu.Unlock(); return pool, nil }, clk.now)

	postCodex(t, base+"/backend-api/codex/responses", "{}")
	mu.Lock()
	pool = []codexAccount{accounts[1], accounts[0]} // b moved to the top of the order
	mu.Unlock()
	clk.add(time.Minute)
	postCodex(t, base+"/backend-api/codex/responses", "{}")
	mu.Lock()
	pool = accounts[1:] // a benched by someone else
	mu.Unlock()
	clk.add(time.Minute)
	postCodex(t, base+"/backend-api/codex/responses", "{}")

	if want := "a@x.io,a@x.io,b@x.io"; strings.Join(seen, ",") != want {
		t.Errorf("accounts upstream = %v, want %s", seen, want)
	}
}

// The point of the proxy: a limit on one account is invisible to codex. The same request goes out
// again, byte for byte, on the next account, and the bench lands where exec and pick see it.
func TestCodexProxyLimitBenchesAndRetriesOnTheNextAccount(t *testing.T) {
	logs := captureLog(t)
	reset := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	var bodies []string
	base := codexProxyTo(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, tagOf(r)+":"+string(b))
		if tagOf(r) == "a@x.io" {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprintf(w, `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"pro","resets_at":%d}}`, reset.Unix())
			return
		}
		io.WriteString(w, "ok from "+tagOf(r))
	}, poolOf(pooled(t, "a@x.io", "b@x.io")), time.Now)

	status, body := postCodex(t, base+"/backend-api/codex/responses", `{"input":"same"}`)
	if status != 200 || body != "ok from b@x.io" {
		t.Fatalf("codex saw %d %q, want b's answer", status, body)
	}
	postCodex(t, base+"/backend-api/codex/responses", `{"input":"next"}`)
	want := []string{`a@x.io:{"input":"same"}`, `b@x.io:{"input":"same"}`, `b@x.io:{"input":"next"}`}
	if strings.Join(bodies, " ") != strings.Join(want, " ") {
		t.Errorf("upstream saw %v, want %v", bodies, want)
	}
	b, ok := readFleet().Bench[codexKey("a@x.io")]
	if !ok || !b.Until.Equal(reset.Add(2*time.Minute)) {
		t.Errorf("bench = %+v, want until the reset plus slack", b)
	}
	if !strings.Contains(logs.String(), "200 as b@x.io (a@x.io: The usage limit has been reached, benched until") ||
		!strings.Contains(logs.String(), "sent again") {
		t.Errorf("log does not tell the story: %q", logs.String())
	}
}

// Once bytes have gone to codex nothing is sent again: a stream that dies after its 200 is codex's.
// Neither is a body above the cap, which was streamed and is gone; the limit still benches.
func TestCodexProxyDoesNotRetryWhatCannotBeSentAgain(t *testing.T) {
	t.Run("stream already started", func(t *testing.T) {
		calls := map[string]int{}
		base := codexProxyTo(t, func(w http.ResponseWriter, r *http.Request) {
			calls[tagOf(r)]++
			io.WriteString(w, "data: partial\n\n")
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}, poolOf(pooled(t, "a@x.io", "b@x.io")), time.Now)
		resp, err := http.Post(base+"/backend-api/codex/responses", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.HasPrefix(string(got), "data: partial") || calls["a@x.io"] != 1 || calls["b@x.io"] != 0 {
			t.Errorf("status %d %q, calls %v", resp.StatusCode, got, calls)
		}
	})
	t.Run("body above the cap", func(t *testing.T) {
		logs := captureLog(t)
		old := codexRetryBodyMax
		codexRetryBodyMax = 4
		t.Cleanup(func() { codexRetryBodyMax = old })
		calls := map[string]int{}
		var body string
		base := codexProxyTo(t, func(w http.ResponseWriter, r *http.Request) {
			calls[tagOf(r)]++
			b, _ := io.ReadAll(r.Body)
			body = string(b)
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"type":"usage_limit_reached"}}`)
		}, poolOf(pooled(t, "a@x.io", "b@x.io")), time.Now)
		status, got := postCodex(t, base+"/backend-api/codex/responses", "longer than four")
		if status != 429 || !strings.Contains(got, "usage_limit_reached") || body != "longer than four" || calls["b@x.io"] != 0 {
			t.Errorf("status %d %q, upstream body %q, calls %v", status, got, body, calls)
		}
		if _, ok := readFleet().Bench[codexKey("a@x.io")]; !ok {
			t.Error("the limit must bench the account even when the request cannot be sent again")
		}
		if !strings.Contains(logs.String(), "body too large to send again") {
			t.Errorf("log: %q", logs.String())
		}
	})
}

func TestCodexLimit(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	reset := now.Add(5 * time.Hour)
	cases := []struct {
		name       string
		status     int
		retryAfter string
		body       string
		limited    bool
		until      time.Time
	}{
		{"usage limit names its reset", 429, "", fmt.Sprintf(`{"error":{"type":"usage_limit_reached","resets_at":%d}}`, reset.Unix()), true, reset.Add(2 * time.Minute)},
		{"plan without codex", 429, "", `{"error":{"type":"usage_not_included"}}`, true, now.Add(time.Hour)},
		{"retry-after when no reset", 429, "600", `{"error":{"code":"insufficient_quota"}}`, true, now.Add(10 * time.Minute)},
		{"unreadable body still a limit", 429, "", `<html>`, true, now.Add(time.Hour)},
		{"throttle is codex's to back off from", 429, "5", `{"error":{"code":"rate_limit_exceeded"}}`, false, time.Time{}},
		{"not a 429", 401, "", `{"error":{"type":"usage_limit_reached"}}`, false, time.Time{}},
	}
	for _, tc := range cases {
		resp := &http.Response{StatusCode: tc.status, Header: http.Header{}}
		if tc.retryAfter != "" {
			resp.Header.Set("Retry-After", tc.retryAfter)
		}
		if until, _, limited := codexLimit(resp, []byte(tc.body), now); limited != tc.limited || !until.Equal(tc.until) {
			t.Errorf("%s: limited %v until %v, want %v until %v", tc.name, limited, until, tc.limited, tc.until)
		}
	}
}

// codex refreshes five minutes before exp, and so does the proxy: the request after the clock
// crosses that line goes out with a token refreshed into the home, not the one about to die.
func TestCodexProxyRefreshesNearExpiry(t *testing.T) {
	clk := &clock{t: time.Now()}
	pool := pooled(t, "a@x.io")
	writeCodexAuth(t, pool[0].Home, fakeJWT("old", clk.now().Add(10*time.Minute)))
	calls := tokenServer(t, http.StatusOK, `{"access_token":"`+fakeJWT("new", clk.now().Add(10*24*time.Hour))+`","refresh_token":"rt-2"}`)
	var seen []string
	base := codexProxyTo(t, func(w http.ResponseWriter, r *http.Request) { seen = append(seen, tagOf(r)) }, poolOf(pool), clk.now)

	postCodex(t, base+"/backend-api/codex/responses", "{}")
	clk.add(6 * time.Minute)
	postCodex(t, base+"/backend-api/codex/responses", "{}")

	if strings.Join(seen, ",") != "old,new" || *calls != 1 {
		t.Errorf("tokens upstream %v, refreshes %d; want old then new after one refresh", seen, *calls)
	}
	if a, _ := readCodexAuth(pool[0].Home); a.Tokens.RefreshToken != "rt-2" {
		t.Error("the refreshed tokens were not written back to the home")
	}
}

func TestCodexProxy401RefreshesAndSendsAgain(t *testing.T) {
	pool := pooled(t, "a@x.io")
	writeCodexAuth(t, pool[0].Home, fakeJWT("old", time.Now().Add(24*time.Hour)))
	calls := tokenServer(t, http.StatusOK, `{"access_token":"`+fakeJWT("new", time.Now().Add(10*24*time.Hour))+`"}`)
	var seen []string
	base := codexProxyTo(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, tagOf(r))
		if tagOf(r) == "old" {
			w.WriteHeader(http.StatusUnauthorized)
		}
	}, poolOf(pool), time.Now)

	if status, _ := postCodex(t, base+"/backend-api/codex/responses", "{}"); status != 200 {
		t.Errorf("status %d, want the retry's 200", status)
	}
	if strings.Join(seen, ",") != "old,new" || *calls != 1 {
		t.Errorf("tokens upstream %v, refreshes %d", seen, *calls)
	}
}

// A refresh the auth server rejects is a dead login (rule 4): bench it for a day, as exec does, and
// carry on with the next account.
func TestCodexProxyDeadLoginBenchesAndMovesOn(t *testing.T) {
	logs := captureLog(t)
	tokenServer(t, http.StatusUnauthorized, `{"error":"refresh_token_expired"}`)
	base := codexProxyTo(t, func(w http.ResponseWriter, r *http.Request) {
		if tagOf(r) == "a@x.io" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, "ok from "+tagOf(r))
	}, poolOf(pooled(t, "a@x.io", "b@x.io")), time.Now)

	if status, body := postCodex(t, base+"/backend-api/codex/responses", "{}"); status != 200 || body != "ok from b@x.io" {
		t.Errorf("codex saw %d %q", status, body)
	}
	b, ok := readFleet().Bench[codexKey("a@x.io")]
	if !ok || !strings.Contains(b.Reason, "login rejected") || time.Until(b.Until) < 23*time.Hour {
		t.Errorf("bench = %+v, want a day for a rejected login", b)
	}
	if !strings.Contains(logs.String(), "a@x.io needs a new login") {
		t.Errorf("log: %q", logs.String())
	}
}

func TestCodexProxyGuards(t *testing.T) {
	called := false
	upstream := func(http.ResponseWriter, *http.Request) { called = true }
	base := codexProxyTo(t, upstream, poolOf(pooled(t, "a@x.io")), time.Now)

	req, _ := http.NewRequest("GET", base+"/backend-api/codex/models", nil)
	req.Host = "evil.example:6662"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign Host: status %d, want 403", resp.StatusCode)
	}

	// A WebSocket would pin the session to one account; 426 is what makes codex use HTTP instead.
	req, _ = http.NewRequest("GET", base+"/backend-api/codex/responses", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Errorf("WebSocket upgrade: status %d, want 426", resp.StatusCode)
	}

	empty := codexProxyTo(t, upstream, poolOf(nil), time.Now)
	status, body := postCodex(t, empty+"/backend-api/codex/responses", "{}")
	if status != http.StatusServiceUnavailable || !strings.Contains(body, "no pooled Codex account") {
		t.Errorf("empty pool: %d %q", status, body)
	}
	if called {
		t.Error("a refused request reached upstream")
	}
}

// The pool is exec's ranking: its order, benches and logins, with spent accounts marked rather
// than dropped.
func TestCodexPoolFollowsExecRanking(t *testing.T) {
	sandbox(t)
	codexHomes(t, "x@x.io", "y@x.io") // z is in the vault but never logged in
	v := testVault()
	v["codex:z@x.io"] = &Account{UUID: "codex:z@x.io", Email: "z@x.io", Kind: kindCodex}
	vaultLoader = func() (Vault, error) { return v, nil }
	t.Cleanup(func() { vaultLoader = loadVault })
	if err := savePrefs(prefs{CodexOrder: []string{"codex:y@x.io", "codex:z@x.io", "codex:x@x.io"}}); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	cacheUsage(map[string]Usage{"codex:y@x.io": {Session: Gauge{Percent: 95, ResetsAt: &later}, FetchedAt: time.Now()}})

	got := func() string {
		pool, err := codexPool()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, a := range pool {
			out = append(out, fmt.Sprintf("%s spent=%v", a.Email, a.Spent))
		}
		return strings.Join(out, ", ")
	}
	if g := got(); g != "y@x.io spent=true, x@x.io spent=false" {
		t.Errorf("pool = %s", g)
	}
	benchAccount("codex:x@x.io", later, "limit")
	if g := got(); g != "y@x.io spent=true" {
		t.Errorf("after the bench: %s", g)
	}
}
