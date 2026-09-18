package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func webFixture(t *testing.T) *monitor {
	t.Helper()
	t.Setenv("VIBEMON_STATE", t.TempDir())
	now := time.Now()
	in5h, in3d := now.Add(5*time.Hour), now.Add(3*24*time.Hour)
	return &monitor{state: panelState{UpdatedAt: now, Accounts: []panelAccount{
		{UUID: "a", Kind: kindClaude, Email: "a@x.io", Plan: "Max 20×", Active: true, HasLogin: true, HasToken: true, Turns: 3,
			Usage: &Usage{Session: Gauge{Label: "Session", Percent: 46, Severity: "normal", ResetsAt: &in5h},
				Weekly: Gauge{Label: "Weekly", Percent: 63, Severity: "normal", ResetsAt: &in3d},
				Scoped: &Gauge{Label: "Fable", Percent: 97, Severity: "critical", ResetsAt: &in3d}, FetchedAt: now}},
		{UUID: "b", Kind: kindClaude, Email: "b@x.io", Plan: "Pro", HasLogin: true, Preferred: true,
			Usage: &Usage{Session: Gauge{Label: "Session"}, Weekly: Gauge{Label: "Weekly", Percent: 82, ResetsAt: &in3d}, FetchedAt: now}},
		{UUID: "codex:c@x.io", Kind: kindCodex, Email: "c@x.io", Plan: "ChatGPT · pro", HasLogin: true, HasToken: true,
			Usage: &Usage{Session: Gauge{Label: "Session"}, Weekly: Gauge{Label: "Weekly", Percent: 8, ResetsAt: &in3d},
				Extra: []Gauge{{Label: "gpt-reserve", Percent: 0}}, FetchedAt: now}},
	}}}
}

func get(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "127.0.0.1:6660"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body)
	}
	return rec.Body.String()
}

func post(t *testing.T, h http.Handler, path, form string, htmx bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = "127.0.0.1:6660"
	if htmx {
		req.Header.Set("HX-Request", "true")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func headers(body string) []string {
	var out []string
	for _, part := range strings.Split(body, "<th>")[1:] {
		out = append(out, part[:strings.Index(part, "</th>")])
	}
	return out
}

// The column picker is the whole point of the page: what it saves is what the next render shows.
func TestWebColumnsPersist(t *testing.T) {
	m := webFixture(t)
	h := m.webHandler()

	got := headers(get(t, h, "/"))
	if strings.Join(got, ",") != "Account,Plan,Status,5h window,Weekly,Fable,Turns / 1h" {
		t.Fatalf("default columns: %v", got)
	}

	rec := post(t, h, "/view", "col=weekly&col=email&col=bogus&range=7d", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /view: %d %s", rec.Code, rec.Body)
	}
	// Catalogue order wins over form order, and unknown ids are dropped.
	if got := headers(rec.Body.String()); strings.Join(got, ",") != "Account,Weekly" {
		t.Fatalf("after save: %v", got)
	}
	if got := headers(get(t, h, "/")); strings.Join(got, ",") != "Account,Weekly" {
		t.Fatalf("after reload: %v", got)
	}
	p := loadPrefs().Web
	if !p.Configured || p.Range != "7d" || len(p.Charts) != 0 {
		t.Fatalf("prefs: %+v", p)
	}

	// Saved with nothing ticked means an empty table, not the defaults back.
	post(t, h, "/view", "range=24h", true)
	if got := headers(get(t, h, "/")); len(got) != 0 {
		t.Fatalf("empty selection rendered %v", got)
	}
}

func TestWebRendersState(t *testing.T) {
	m := webFixture(t)
	body := get(t, m.webHandler(), "/")
	for _, want := range []string{`class="email">a@x.io`, "★ b@x.io", "<th>Fable</th>", `<span class="pct">97%</span>`, `class="gauge warning"`, "Codex · ChatGPT", "polled "} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// A window the account does not have is a dash, never a healthy 0%.
	if strings.Count(body, `<span class="dim">-</span>`) < 2 {
		t.Error("codex and parked accounts should show a dash for their missing session window")
	}
}

func TestWebGuards(t *testing.T) {
	m := webFixture(t)
	h := m.webHandler()
	if rec := post(t, h, "/view", "col=email", false); rec.Code != http.StatusForbidden {
		t.Errorf("a POST without the htmx header must be refused, got %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "evil.example:6660"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a foreign Host must be refused, got %d", rec.Code)
	}
}

// A fragment carries the header stamp out of band, so the "polled" time moves with the table.
func TestWebFragmentSwapsStamp(t *testing.T) {
	m := webFixture(t)
	body := get(t, m.webHandler(), "/view")
	if !strings.Contains(body, `id="stamp" class="stamp num" hx-swap-oob="true">polled `) {
		t.Fatalf("fragment lacks the out-of-band stamp:\n%s", body[len(body)-300:])
	}
	if strings.Count(get(t, m.webHandler(), "/"), `id="stamp"`) != 1 {
		t.Error("the full page must carry the stamp exactly once")
	}
}

// A publish reaches an open stream as a state event, and a closed stream leaves no client behind.
func TestWebEventsPush(t *testing.T) {
	m := webFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/events", nil).WithContext(ctx)
	req.Host = "127.0.0.1:6660"
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { m.webHandler().ServeHTTP(rec, req); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		webClients.Lock()
		n := len(webClients.m)
		webClients.Unlock()
		if n == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	webWake()
	webWake()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	if got := strings.Count(rec.Body.String(), "event: state"); got < 1 || got > 2 {
		t.Fatalf("two publishes should reach the stream as one or two events, got %d in %q", got, rec.Body.String())
	}
	webClients.Lock()
	defer webClients.Unlock()
	if len(webClients.m) != 0 {
		t.Error("a closed stream must unregister its client")
	}
}

func TestBuildCharts(t *testing.T) {
	m := webFixture(t)
	now := time.Now()
	r := rangeOf("6h")
	samples := []sample{
		{At: now.Add(-7 * time.Hour), Account: "a", Gauges: map[string]float64{"Session": 90, "Weekly": 50}}, // out of range
		{At: now.Add(-2 * time.Hour), Account: "a", Gauges: map[string]float64{"Session": 20, "Weekly": 60, "Fable": 80}},
		{At: now.Add(-1 * time.Hour), Account: "a", Gauges: map[string]float64{"Session": 46, "Weekly": 63, "Fable": 97}},
		{At: now.Add(-1 * time.Hour), Account: "codex:c@x.io", Gauges: map[string]float64{"Session": 0, "Weekly": 8}},
	}
	flat := func(rows []chartRow) (names []string, first chart) {
		for _, row := range rows {
			for _, c := range row.Charts {
				names = append(names, c.Email+"/"+c.Gauge)
			}
		}
		if len(rows) > 0 {
			first = rows[0].Charts[0]
		}
		return names, first
	}
	names, session := flat(buildCharts(m.state, samples, setOf([]string{"session", "weekly"}), r, now))
	// One row per account with history, b@x.io has none and gets no row.
	if strings.Join(names, " ") != "a@x.io/Session a@x.io/Weekly c@x.io/Weekly" {
		t.Fatalf("charts: %v", names)
	}
	if n := strings.Count(session.Points, ","); n != 2 {
		t.Errorf("session chart should carry the 2 in-range points, got %q", session.Points)
	}
	if session.EndY != yOf(46) || session.EndX != plotX1-(plotX1-plotX0)/6 {
		t.Errorf("end marker at %.1f,%.1f", session.EndX, session.EndY)
	}
	if len(session.Ticks) == 0 || len(session.Ticks) > 7 {
		t.Errorf("6h range should carry about six hourly ticks, got %d", len(session.Ticks))
	}
	names, model := flat(buildCharts(m.state, samples, setOf([]string{"model"}), r, now))
	if len(names) != 1 || model.Gauge != "Fable" || model.Class != "critical" {
		t.Fatalf("per-model charts: %v %+v", names, model)
	}
}

// Writing the page out is how it gets judged; `just web-preview` sets this and opens the result.
func TestWebPreview(t *testing.T) {
	out := os.Getenv("VIBEMON_WEB_OUT")
	if out == "" {
		t.Skip("set VIBEMON_WEB_OUT to dump the dashboard as HTML")
	}
	m := webFixture(t)
	now := time.Now()
	var polled []map[string]Usage
	for i := 480; i >= 0; i-- {
		at := now.Add(-time.Duration(i) * 3 * time.Minute)
		h := float64(480-i) / 20
		s := Usage{Session: Gauge{Label: "Session", Percent: float64(int(h*23) % 100)},
			Weekly: Gauge{Label: "Weekly", Percent: min(h*2.6, 100)},
			Scoped: &Gauge{Label: "Fable", Percent: min(h*4, 100)}}
		polled = append(polled, map[string]Usage{"a": s, "b": {Session: Gauge{Label: "Session"}, Weekly: Gauge{Label: "Weekly", Percent: min(h/2, 100)}},
			"codex:c@x.io": {Session: Gauge{Label: "Session"}, Weekly: Gauge{Label: "Weekly", Percent: min(h/3, 100)}}})
		if err := recordHistory(polled[len(polled)-1], at); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(out, []byte(get(t, m.webHandler(), "/")), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", out)
}
