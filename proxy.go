package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync"
	"time"
)

// The auth proxy: Claude Code sessions started with ANTHROPIC_BASE_URL=http://127.0.0.1:6661 send
// every API request through here, and each one leaves with the active account's token. Claude Code
// memoizes its token in-process and re-reads the keychain only on its own refresh, a 401 or a
// credentials-file change, so without the proxy a switch never reaches a session already running.

const defaultProxyAddr = "127.0.0.1:6661"

func proxyAddr() string {
	if a := os.Getenv("VIBEMON_PROXY_ADDR"); a != "" {
		return a
	}
	return defaultProxyAddr
}

var anthropicAPI = &url.URL{Scheme: "https", Host: "api.anthropic.com"}

// A request burst (a turn fans out into several calls) shares one keychain read; a switch reaches
// the next request after at most this long.
const proxyTokenTTL = 5 * time.Second

type proxyCreds struct {
	token string
	email string // for the log line only
}

// activeCreds reads the token Claude Code is logged in with. Read-only (rule 3): an expired token
// goes out as it is, upstream answers 401, the session refreshes it and writes the keychain, and the
// cache drops it on that same 401.
func activeCreds() (proxyCreds, error) {
	o, err := ccOAuth()
	if err != nil {
		return proxyCreds{}, err
	}
	// A switch patches ~/.claude.json alongside the keychain, so its email names the token's account.
	_, raw := readClaudeJSONIdentity()
	var id struct {
		Email string `json:"emailAddress"`
	}
	_ = json.Unmarshal(raw, &id)
	return proxyCreds{token: o.AccessToken, email: id.Email}, nil
}

type tokenCache struct {
	read func() (proxyCreds, error)
	mu   sync.Mutex
	c    proxyCreds
	at   time.Time
}

// get holds the lock across the read on purpose: a burst waits for one security(1) call instead of
// starting one each.
func (t *tokenCache) get() (proxyCreds, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.c.token != "" && time.Since(t.at) < proxyTokenTTL {
		return t.c, nil
	}
	c, err := t.read()
	if err != nil {
		return proxyCreds{}, err
	}
	t.c, t.at = c, time.Now()
	return c, nil
}

func (t *tokenCache) invalidate() {
	t.mu.Lock()
	t.c = proxyCreds{}
	t.mu.Unlock()
}

// proxyAuth travels with the outbound request so the response side knows what it carried.
type proxyAuth struct {
	proxyCreds
	err error // the keychain read failed and the client's own credentials went out
}

type proxyAuthKey struct{}

// newAuthProxy forwards everything to upstream unchanged except the credentials: Authorization
// becomes the active account's token and x-api-key goes, because it would outrank the bearer.
// anthropic-beta stays as sent; Claude Code adds the oauth beta itself.
func newAuthProxy(upstream *url.URL, read func() (proxyCreds, error)) http.Handler {
	cache := &tokenCache{read: read}
	rp := &httputil.ReverseProxy{
		FlushInterval: -1, // SSE: each event goes out the moment it arrives
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			var a proxyAuth
			a.proxyCreds, a.err = cache.get()
			if a.err == nil {
				pr.Out.Header.Set("Authorization", "Bearer "+a.token)
				pr.Out.Header.Del("X-Api-Key")
			}
			pr.Out = pr.Out.WithContext(context.WithValue(pr.Out.Context(), proxyAuthKey{}, a))
		},
		ModifyResponse: func(resp *http.Response) error {
			a, _ := resp.Request.Context().Value(proxyAuthKey{}).(proxyAuth)
			if resp.StatusCode == http.StatusUnauthorized && a.err == nil {
				cache.invalidate()
			}
			who := "as " + a.email
			switch {
			case a.err != nil:
				who = fmt.Sprintf("with the client's own credentials (%v)", a.err)
			case a.email == "":
				who = "as the active account"
			}
			log.Printf("vibemon proxy: %s %s %d %s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, who)
			return nil
		},
	}
	// Any page in the browser can reach a loopback port, and this one spends a subscription: a Host
	// that is not ours is DNS rebinding.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host, proxyAddr()) {
			http.Error(w, "unexpected Host", http.StatusForbidden)
			return
		}
		rp.ServeHTTP(w, r)
	})
}

// viaProxy reports whether an ANTHROPIC_BASE_URL routes through this proxy.
func viaProxy(base string) bool {
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	_, port, _ := net.SplitHostPort(proxyAddr())
	return u.Port() == port && loopbackHost(u.Host, proxyAddr())
}

// serveProxy runs the auth proxy until its listener fails.
func serveProxy() error {
	ln, err := net.Listen("tcp", proxyAddr())
	if err != nil {
		return fmt.Errorf("auth proxy not started: %w", err)
	}
	log.Printf("vibemon: auth proxy on http://%s", ln.Addr())
	if err := http.Serve(ln, newAuthProxy(anthropicAPI, activeCreds)); err != nil {
		return fmt.Errorf("auth proxy stopped: %w", err)
	}
	return nil
}
