package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// proxyTo starts the auth proxy in front of upstream with read as its token source, so no test
// touches the real keychain.
func proxyTo(t *testing.T, upstream http.HandlerFunc, read func() (proxyCreds, error)) string {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	u, err := url.Parse(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	p := httptest.NewServer(newAuthProxy(u, read))
	t.Cleanup(p.Close)
	return p.URL
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

func TestProxyInjectsActiveToken(t *testing.T) {
	logs := captureLog(t)
	var got *http.Request
	var body string
	base := proxyTo(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got, body = r, string(b)
	}, func() (proxyCreds, error) { return proxyCreds{"tok-active", "a@x.io"}, nil })

	req, _ := http.NewRequest("POST", base+"/v1/messages?beta=true", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer tok-memoized")
	req.Header.Set("X-Api-Key", "sk-ant-api-x")
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if a := got.Header.Get("Authorization"); a != "Bearer tok-active" {
		t.Errorf("Authorization upstream = %q, want the active token", a)
	}
	if got.Header.Get("X-Api-Key") != "" {
		t.Error("x-api-key must be dropped: it outranks the bearer")
	}
	if b := got.Header.Get("Anthropic-Beta"); b != "oauth-2025-04-20" {
		t.Errorf("anthropic-beta = %q, want it as sent", b)
	}
	if got.URL.Path != "/v1/messages" || got.URL.RawQuery != "beta=true" || body != `{"model":"m"}` {
		t.Errorf("path, query or body changed: %s?%s %s", got.URL.Path, got.URL.RawQuery, body)
	}
	if !strings.Contains(logs.String(), "POST /v1/messages 200 as a@x.io") {
		t.Errorf("log line missing: %q", logs.String())
	}
	if strings.Contains(logs.String(), "tok-") {
		t.Error("a token reached the log")
	}
}

func TestProxyStreamsIncrementally(t *testing.T) {
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	base := proxyTo(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, "data: two\n\n")
	}, func() (proxyCreds, error) { return proxyCreds{token: "t"}, nil })

	resp, err := http.Get(base + "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
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
	rest, _ := io.ReadAll(br)
	if string(rest) != "\ndata: two\n\n" {
		t.Errorf("rest of stream = %q", rest)
	}
}

func TestProxy401RereadsKeychain(t *testing.T) {
	reads, calls := 0, 0
	var seen []string
	base := proxyTo(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		seen = append(seen, r.Header.Get("Authorization"))
		if calls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
		}
	}, func() (proxyCreds, error) {
		reads++
		return proxyCreds{token: fmt.Sprintf("tok-%d", reads)}, nil
	})

	for range 3 {
		resp, err := http.Get(base + "/v1/messages")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if reads != 2 {
		t.Errorf("keychain reads = %d, want 2: one at start, one after the 401, then the cache", reads)
	}
	want := []string{"Bearer tok-1", "Bearer tok-2", "Bearer tok-2"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Errorf("tokens upstream = %v, want %v", seen, want)
	}
}

func TestProxyFallsBackToClientCredentials(t *testing.T) {
	logs := captureLog(t)
	var auth string
	base := proxyTo(t, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
	}, func() (proxyCreds, error) { return proxyCreds{}, errors.New("keychain locked") })

	req, _ := http.NewRequest("POST", base+"/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer tok-own")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || auth != "Bearer tok-own" {
		t.Errorf("status %d, Authorization upstream %q: a failed read must not fail the request", resp.StatusCode, auth)
	}
	if !strings.Contains(logs.String(), "client's own credentials (keychain locked)") {
		t.Errorf("the fallback was not logged: %q", logs.String())
	}
}

func TestProxyRefusesForeignHost(t *testing.T) {
	called := false
	base := proxyTo(t, func(http.ResponseWriter, *http.Request) { called = true },
		func() (proxyCreds, error) { return proxyCreds{token: "t"}, nil })
	req, _ := http.NewRequest("GET", base+"/v1/models", nil)
	req.Host = "evil.example:6661"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || called {
		t.Errorf("a DNS-rebound Host must never reach upstream with the token (status %d)", resp.StatusCode)
	}
}
