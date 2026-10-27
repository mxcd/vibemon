package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync"
	"time"
)

// The Codex auth proxy: codex started with openai_base_url=http://127.0.0.1:6662/backend-api/codex
// sends its model traffic (/models, /responses) through here, and each request leaves with a pooled
// ChatGPT account's credentials instead of the user's own. The account is sticky until a limit or a
// dead login moves it to the next one in exec's Codex order, and the request that hit the limit is
// sent again on that next account before codex sees anything.

const defaultCodexProxyAddr = "127.0.0.1:6662"

func codexProxyAddr() string {
	if a := os.Getenv("VIBEMON_CODEX_PROXY_ADDR"); a != "" {
		return a
	}
	return defaultCodexProxyAddr
}

var chatgptAPI = &url.URL{Scheme: "https", Host: "chatgpt.com"}

// A request body up to this size is kept for a retry on the next account. codex sends its context
// zstd-compressed, so even a long session stays far below it. A var for tests.
var codexRetryBodyMax int64 = 32 << 20

var errNoCodexAccount = errors.New("no pooled Codex account is usable (vibemon pick --kind codex)")

// codexAccount is one pooled login the proxy may send with.
type codexAccount struct {
	Key, Email, Home string
	// Spent: over exec's ceiling. The proxy does not start on such an account while another has
	// headroom, but it does not leave one for it either: only a failure moves the sticky choice.
	Spent bool
}

// codexPool lists the accounts the proxy may use, in exec's Codex order: rank drops benched and
// logged-out accounts, and a second pass without a ceiling tells the spent ones apart.
func codexPool() ([]codexAccount, error) {
	v, err := vaultLoader()
	if err != nil {
		return nil, err
	}
	p, st := loadPrefs(), readFleet()
	// The proxy serves every project at once, so only the global Codex order applies; "/" is a
	// directory no project policy names.
	o := pickOptions{Dir: "/", Kind: kindCodex, Ceiling: defaultCeiling}
	fresh, _, _ := rank(v, p, &st, o)
	o.Ceiling = math.Inf(1)
	usable, _, _ := rank(v, p, &st, o)
	headroom := map[string]bool{}
	for _, c := range fresh {
		headroom[c.Account.UUID] = true
	}
	pool := make([]codexAccount, 0, len(usable))
	for _, c := range usable {
		a := c.Account
		pool = append(pool, codexAccount{Key: a.UUID, Email: a.Email, Home: codexHome(a.Email), Spent: !headroom[a.UUID]})
	}
	return pool, nil
}

type codexCreds struct {
	codexAccount
	token, accountID string
}

type codexProxy struct {
	pool func() ([]codexAccount, error)
	now  func() time.Time
	next http.RoundTripper

	mu      sync.Mutex
	current codexCreds // the sticky account, with its token as read at
	at      time.Time
	benched map[string]time.Time // benches this process decided on, kept even if the ledger write fails
}

// newCodexProxy forwards to upstream with the path unchanged and swaps Authorization and
// ChatGPT-Account-Id for the pooled account's. pool and now are the seams tests replace.
func newCodexProxy(upstream *url.URL, pool func() ([]codexAccount, error), now func() time.Time) http.Handler {
	p := &codexProxy{pool: pool, now: now, next: http.DefaultTransport, benched: map[string]time.Time{}}
	rp := &httputil.ReverseProxy{
		FlushInterval: -1, // SSE: each event goes out the moment it arrives
		Rewrite:       func(pr *httputil.ProxyRequest) { pr.SetURL(upstream) },
		Transport:     p,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			status := http.StatusBadGateway
			if errors.Is(err, errNoCodexAccount) {
				status = http.StatusServiceUnavailable
			}
			log.Printf("vibemon codex proxy: %s %s %d (%v)", r.Method, r.URL.Path, status, err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "vibemon: " + err.Error()}})
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Any page in the browser can reach a loopback port, and this one spends a subscription: a
		// Host that is not ours is DNS rebinding.
		if !loopbackHost(r.Host, codexProxyAddr()) {
			http.Error(w, "unexpected Host", http.StatusForbidden)
			return
		}
		// A WebSocket stays on the account it opened with and carries a limit inside its frames,
		// where no switch can reach it. 426 is the answer codex takes as "use HTTP" for the rest of
		// the session (core/src/client.rs), and over HTTP every turn is a request of its own.
		if r.Header.Get("Upgrade") != "" {
			log.Printf("vibemon codex proxy: %s %s %d (WebSocket refused, codex falls back to HTTP)", r.Method, r.URL.Path, http.StatusUpgradeRequired)
			http.Error(w, "use HTTP", http.StatusUpgradeRequired)
			return
		}
		rp.ServeHTTP(w, r)
	})
}

// RoundTrip sends one request with the sticky account and, when the answer is a usage limit or a
// rejected token, once more with whatever replaces it. Every retry decision is made on the status
// line, before the response goes back to the ReverseProxy, so nothing has reached codex yet; a
// stream that fails after its 200 is codex's to handle.
func (p *codexProxy) RoundTrip(req *http.Request) (*http.Response, error) {
	body, replay, err := keepBody(req)
	if err != nil {
		return nil, err
	}
	c, err := p.creds(codexCreds{})
	if err != nil {
		return nil, err
	}
	resp, err := p.send(req, body, replay, c)
	if err != nil {
		return nil, err
	}
	note := ""
	var next codexCreds
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		until, reason, limited := codexLimit(resp, peekBody(resp), p.now())
		if !limited {
			break // a throttle: codex backs off and asks again by itself (rule 4)
		}
		p.bench(c.codexAccount, until, reason)
		note = fmt.Sprintf("%s: %s, benched until %s", c.Email, reason, until.Local().Format("02.01. 15:04"))
		next, err = p.creds(codexCreds{})
	case http.StatusUnauthorized:
		note = c.Email + ": token rejected"
		next, err = p.creds(c)
	}
	switch {
	case note == "":
	case !replay:
		note += "; body too large to send again"
	case err != nil:
		note += "; " + err.Error()
	default:
		retry, err := p.send(req, body, replay, next)
		if err != nil {
			note += "; sending again failed: " + err.Error()
			break
		}
		resp.Body.Close()
		resp, c = retry, next
		note += "; sent again"
	}
	if note != "" {
		note = " (" + note + ")"
	}
	log.Printf("vibemon codex proxy: %s %s %d as %s%s", req.Method, req.URL.Path, resp.StatusCode, c.Email, note)
	return resp, nil
}

func (p *codexProxy) send(req *http.Request, body []byte, replay bool, c codexCreds) (*http.Response, error) {
	out := req.Clone(req.Context())
	if replay {
		out.Body, out.ContentLength, out.TransferEncoding = nil, int64(len(body)), nil
		if body != nil {
			out.Body = io.NopCloser(bytes.NewReader(body))
		}
	}
	out.Header.Set("Authorization", "Bearer "+c.token)
	out.Header.Set("ChatGPT-Account-Id", c.accountID)
	return p.next.RoundTrip(out)
}

// creds returns the account to send with. rejected, when set, is what upstream has just answered
// 401 to: that account is refreshed (or picks up what codex refreshed meanwhile) before it is used
// again. The lock is held across the reads and a refresh on purpose: a burst waits for one keychain
// read and one refresh instead of starting one each.
func (p *codexProxy) creds(rejected codexCreds) (codexCreds, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if rejected.token == "" && p.current.token != "" && now.Sub(p.at) < proxyTokenTTL && !codexTokenStale(p.current.token, now) {
		return p.current, nil
	}
	pool, err := p.pool()
	if err != nil {
		return codexCreds{}, err
	}
	for _, a := range p.order(pool, now) {
		stale := ""
		if a.Key == rejected.Key {
			stale = rejected.token
		}
		c, err := p.load(a, stale, now)
		if errors.Is(err, errNeedsReauth) {
			log.Printf("vibemon codex proxy: %s needs a new login: %v", a.Email, err)
			p.benchLocked(a, now.Add(24*time.Hour), "login rejected: vibemon codex add "+a.Email)
			continue
		}
		if err != nil {
			return codexCreds{}, err
		}
		p.current, p.at = c, now
		return c, nil
	}
	return codexCreds{}, errNoCodexAccount
}

// order puts the sticky account first, then the rest with headroom, then the spent ones, which still
// beat refusing the request outright.
func (p *codexProxy) order(pool []codexAccount, now time.Time) []codexAccount {
	var first, rest, spent []codexAccount
	for _, a := range pool {
		switch until, ok := p.benched[a.Key]; {
		case ok && now.Before(until):
		case a.Key == p.current.Key:
			first = append(first, a)
		case a.Spent:
			spent = append(spent, a)
		default:
			rest = append(rest, a)
		}
	}
	return append(append(first, rest...), spent...)
}

// load reads an account's token and refreshes it when it is about to expire or is the one upstream
// just rejected.
func (p *codexProxy) load(a codexAccount, rejected string, now time.Time) (codexCreds, error) {
	auth, err := readCodexAuth(a.Home)
	if err == nil && (auth.Tokens.AccessToken == rejected || codexTokenStale(auth.Tokens.AccessToken, now)) {
		auth, err = refreshCodexAuth(a.Home, auth.Tokens.AccessToken, now)
	}
	if err != nil {
		return codexCreds{}, err
	}
	return codexCreds{codexAccount: a, token: auth.Tokens.AccessToken, accountID: auth.Tokens.AccountID}, nil
}

func (p *codexProxy) bench(a codexAccount, until time.Time, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.benchLocked(a, until, reason)
}

// benchLocked records a bench the same way exec does, so pick, exec and the panel see it too, and
// drops the sticky choice if it was this account.
func (p *codexProxy) benchLocked(a codexAccount, until time.Time, reason string) {
	p.benched[a.Key] = until
	benchAccount(a.Key, until, reason)
	if p.current.Key == a.Key {
		p.current = codexCreds{}
	}
}

// codexLimit reads a 429 the way codex does (codex-api/src/api_bridge.rs, codex 0.163.0): an error
// of type usage_limit_reached names its reset as resets_at in unix seconds. Any 429 is a limit
// except a plain throttle, which codex backs off from by itself and which says nothing about the
// account's headroom. The bench runs to the reset plus exec's two minutes of slack, else to
// Retry-After, else for exec's hour.
func codexLimit(resp *http.Response, body []byte, now time.Time) (until time.Time, reason string, ok bool) {
	if resp.StatusCode != http.StatusTooManyRequests {
		return time.Time{}, "", false
	}
	var e struct {
		Error struct {
			Type     string `json:"type"`
			Code     string `json:"code"`
			Message  string `json:"message"`
			ResetsAt int64  `json:"resets_at"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	if e.Error.Type == "rate_limit_exceeded" || e.Error.Code == "rate_limit_exceeded" {
		return time.Time{}, "", false
	}
	reason = "usage limit"
	for _, s := range []string{e.Error.Message, e.Error.Type, e.Error.Code} {
		if s != "" {
			reason = s
			break
		}
	}
	switch ra := retryAfter(resp); {
	case e.Error.ResetsAt > 0:
		until = time.Unix(e.Error.ResetsAt, 0).Add(2 * time.Minute)
	case ra > 0:
		until = now.Add(ra)
	default:
		until = now.Add(time.Hour)
	}
	return until, reason, true
}

// keepBody reads a request body into memory so it can be sent a second time. Above
// codexRetryBodyMax it puts back a reader that streams the rest instead, and replay is false.
func keepBody(req *http.Request) (body []byte, replay bool, err error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, true, nil
	}
	body, err = io.ReadAll(io.LimitReader(req.Body, codexRetryBodyMax+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(body)) > codexRetryBodyMax {
		req.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(body), req.Body), req.Body}
		return nil, false, nil
	}
	req.Body.Close()
	return body, true, nil
}

// peekBody reads the start of a response body for classification and leaves the whole body in
// place for codex, which shows the error's own message.
func peekBody(resp *http.Response) []byte {
	head, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}
	return head
}

// serveCodexProxy runs the Codex auth proxy until its listener fails.
func serveCodexProxy() error {
	ln, err := net.Listen("tcp", codexProxyAddr())
	if err != nil {
		return fmt.Errorf("codex auth proxy not started: %w", err)
	}
	log.Printf("vibemon: codex auth proxy on http://%s", ln.Addr())
	if err := http.Serve(ln, newCodexProxy(chatgptAPI, codexPool, time.Now)); err != nil {
		return fmt.Errorf("codex auth proxy stopped: %w", err)
	}
	return nil
}
