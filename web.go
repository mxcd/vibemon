package main

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// The dashboard: the panel's state as a table plus rundown charts of every limit, served on
// loopback by the menu bar app. Server-rendered html/template with htmx for the swaps; the browser
// runs no code of ours.

//go:embed web
var webAssets embed.FS

// Loopback only: the page lists every account's email and headroom. The port is not 6666 because
// Chrome, Firefox and Safari all refuse it (it sits on their IRC port blocklist, ERR_UNSAFE_PORT).
const defaultWebAddr = "127.0.0.1:6660"

func webAddr() string {
	if a := os.Getenv("VIBEMON_WEB"); a != "" {
		return a
	}
	return defaultWebAddr
}

type column struct{ ID, Name string }

// The per-model column is named after the model when every Claude account reports the same one
// (see modelName); "Per-model" is only the fallback.
var columns = []column{
	{"email", "Account"}, {"plan", "Plan"}, {"org", "Org"}, {"status", "Status"},
	{"session", "5h window"}, {"weekly", "Weekly"}, {"model", "Per-model"},
	{"turns", "Turns / 1h"}, {"creds", "Credentials"}, {"fetched", "Fetched"}, {"error", "Error"},
}

var defaultColumns = []string{"email", "plan", "status", "session", "weekly", "model", "turns"}

var chartGauges = []column{{"session", "5h window"}, {"weekly", "Weekly"}, {"model", "Per-model"}}

var defaultCharts = []string{"session", "weekly", "model"}

// modelName is the scoped limit's model when the Claude accounts agree on one, so the column and
// chart read "Fable" rather than the generic "Per-model".
func modelName(state panelState) string {
	name := ""
	for _, a := range state.Accounts {
		if a.Kind == kindCodex || a.Usage == nil || a.Usage.Scoped == nil {
			continue
		}
		if name != "" && name != a.Usage.Scoped.Label {
			return "Per-model"
		}
		name = a.Usage.Scoped.Label
	}
	if name == "" {
		return "Per-model"
	}
	return name
}

type chartRange struct {
	ID   string
	Span time.Duration
	Step time.Duration // x-axis tick spacing
}

var ranges = []chartRange{
	{"6h", 6 * time.Hour, time.Hour},
	{"24h", 24 * time.Hour, 6 * time.Hour},
	{"7d", 7 * 24 * time.Hour, 24 * time.Hour},
	{"14d", 14 * 24 * time.Hour, 48 * time.Hour},
}

func rangeOf(id string) chartRange {
	for _, r := range ranges {
		if r.ID == id {
			return r
		}
	}
	return ranges[1]
}

// known keeps only ids from the catalogue, in catalogue order, so prefs.json never carries a typo
// and the table columns come out in their fixed order whatever the form sent.
func known(catalogue []column, ids []string) []string {
	var out []string
	for _, c := range catalogue {
		if containsFold(ids, c.ID) {
			out = append(out, c.ID)
		}
	}
	return out
}

func (w webPrefs) effective() (cols, charts []string, r chartRange) {
	cols, charts, r = w.Columns, w.Charts, rangeOf(w.Range)
	if !w.Configured {
		cols, charts = defaultColumns, defaultCharts
	}
	return cols, charts, r
}

// Class is the severity class the panel uses, so the two views agree on what counts as a warning.
func (g Gauge) Class() string {
	switch {
	case g.Severity == "critical" || g.Severity == "severe" || g.Percent >= 95:
		return "critical"
	case g.Severity == "warning" || g.Percent >= 80:
		return "warning"
	}
	return ""
}

// Live mirrors the panel: a window the account does not have (ChatGPT plans report no session
// one) reads as 0% with no reset, and is left out rather than shown as healthy.
func (g Gauge) Live() bool { return g.Percent > 0 || g.ResetsAt != nil }

// --- charts ------------------------------------------------------------------

// Plot area inside the SVG viewBox; the template draws the labels around it.
const (
	plotX0, plotX1 = 28.0, 312.0
	plotY0, plotY1 = 6.0, 86.0
)

// chartRow is one account's charts side by side, so the same window lines up across accounts.
type chartRow struct {
	Email  string
	Charts []chart
}

type chart struct {
	Email, Gauge string
	Class        string
	Percent      float64
	Points       string // polyline
	EndX, EndY   float64
	Ticks        []tick
}

type tick struct {
	X     float64
	Label string
}

func yOf(pct float64) float64 { return plotY1 - min(max(pct, 0), 100)/100*(plotY1-plotY0) }

func xOf(t, since, now time.Time) float64 {
	return plotX0 + float64(t.Sub(since))/float64(now.Sub(since))*(plotX1-plotX0)
}

// ticks walks the range in Step increments from local midnight, so day marks sit on real days
// and hour marks on real hours instead of on UTC-aligned offsets.
func ticks(since, now time.Time, r chartRange) []tick {
	var out []tick
	y, m, d := since.Local().Date()
	for t := time.Date(y, m, d, 0, 0, 0, 0, time.Local); !t.After(now); t = t.Add(r.Step) {
		if t.Before(since) {
			continue
		}
		label := t.Format("15:04")
		if r.Step >= 24*time.Hour {
			label = t.Format("02.01.")
		}
		out = append(out, tick{X: xOf(t, since, now), Label: label})
	}
	return out
}

// buildCharts makes one chart per account and charted gauge that has history in the range.
func buildCharts(state panelState, samples []sample, charted map[string]bool, r chartRange, now time.Time) []chartRow {
	since := now.Add(-r.Span)
	series := map[string][]sample{}
	for _, s := range samples {
		if !s.At.Before(since) {
			series[s.Account] = append(series[s.Account], s)
		}
	}
	var out []chartRow
	for _, a := range state.Accounts {
		if a.Usage == nil {
			continue
		}
		row := chartRow{Email: a.Email}
		var gauges []Gauge
		if charted["session"] && a.Usage.Session.Live() {
			gauges = append(gauges, a.Usage.Session)
		}
		if charted["weekly"] && a.Usage.Weekly.Live() {
			gauges = append(gauges, a.Usage.Weekly)
		}
		if charted["model"] {
			if a.Usage.Scoped != nil {
				gauges = append(gauges, *a.Usage.Scoped)
			}
			gauges = append(gauges, a.Usage.Extra...)
		}
		own := series[a.UUID]
		sort.SliceStable(own, func(i, j int) bool { return own[i].At.Before(own[j].At) })
		for _, g := range gauges {
			c := chart{Email: a.Email, Gauge: g.Label, Class: g.Class(), Percent: g.Percent}
			var b strings.Builder
			for _, s := range own {
				pct, ok := s.Gauges[g.Label]
				if !ok {
					continue
				}
				c.EndX, c.EndY = xOf(s.At, since, now), yOf(pct)
				fmt.Fprintf(&b, "%.1f,%.1f ", c.EndX, c.EndY)
			}
			if b.Len() == 0 {
				continue
			}
			c.Points = strings.TrimSpace(b.String())
			c.Ticks = ticks(since, now, r)
			row.Charts = append(row.Charts, c)
		}
		if len(row.Charts) > 0 {
			out = append(out, row)
		}
	}
	return out
}

// --- rendering ---------------------------------------------------------------

type webPage struct {
	State         panelState
	Columns       []column
	Shown         map[string]bool
	ChartGauges   []column
	Charted       map[string]bool
	Ranges        []chartRange
	Range         string
	Charts        []chartRow
	ModelName     string
	OOB           bool // a fragment response carries the header stamp as an out-of-band swap
	Now           time.Time
	Y50, Y80, Y95 float64
}

var webTmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"until": until,
	"pct":   func(f float64) string { return fmt.Sprintf("%.0f%%", f) },
	"stamp": func(t time.Time) string { return t.Local().Format("02.01.2006 15:04:05") },
	"clock": func(t time.Time) string { return t.Local().Format("15:04:05") },
	"colspan": func(shown map[string]bool) int {
		n := 0
		for _, c := range columns {
			if shown[c.ID] {
				n++
			}
		}
		return n
	},
}).ParseFS(webAssets, "web/index.html"))

func setOf(ids []string) map[string]bool {
	s := map[string]bool{}
	for _, id := range ids {
		s[id] = true
	}
	return s
}

func (m *monitor) snapshot() panelState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *monitor) setWebPrefs(w webPrefs) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.prefs()
	p.Web = w
	return savePrefs(p)
}

func (m *monitor) renderWeb(w http.ResponseWriter, name string) {
	cols, charts, r := loadPrefs().Web.effective()
	now := time.Now()
	page := webPage{
		State: m.snapshot(), Columns: columns, Shown: setOf(cols),
		ChartGauges: chartGauges, Charted: setOf(charts), Ranges: ranges, Range: r.ID,
		Now: now, Y50: yOf(50), Y80: yOf(80), Y95: yOf(95),
	}
	page.ModelName = modelName(page.State)
	if len(charts) > 0 {
		page.Charts = buildCharts(page.State, readHistory(now.Add(-r.Span)), page.Charted, r, now)
	}
	var buf bytes.Buffer
	if err := webTmpl.ExecuteTemplate(&buf, name, page); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if name == "view" {
		page.OOB = true
		if err := webTmpl.ExecuteTemplate(&buf, "stamp", page); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

func (m *monitor) webHandler() http.Handler {
	static, _ := fs.Sub(webAssets, "web")
	mux := http.NewServeMux()
	mux.Handle("GET /htmx.min.js", http.FileServerFS(static))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { m.renderWeb(w, "page") })
	mux.HandleFunc("GET /view", func(w http.ResponseWriter, r *http.Request) { m.renderWeb(w, "view") })
	mux.HandleFunc("POST /view", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		err := m.setWebPrefs(webPrefs{
			Configured: true,
			Columns:    known(columns, r.Form["col"]),
			Charts:     known(chartGauges, r.Form["chart"]),
			Range:      rangeOf(r.FormValue("range")).ID,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		m.renderWeb(w, "view")
	})
	mux.HandleFunc("GET /events", webEvents)
	mux.HandleFunc("POST /refresh", func(w http.ResponseWriter, r *http.Request) {
		m.poll(true)
		m.renderWeb(w, "view")
	})
	return guard(mux)
}

// --- live updates ------------------------------------------------------------

// Every publish wakes each open dashboard tab over Server-Sent Events, and the tab re-fetches the
// view. The page also polls every minute, so a dropped stream costs at most that. Wakeups are
// coalesced per client (a full channel means one is already pending) and never block publish,
// which runs under the monitor's lock.
var webClients = struct {
	sync.Mutex
	m map[chan struct{}]struct{}
}{m: map[chan struct{}]struct{}{}}

func webWake() {
	webClients.Lock()
	defer webClients.Unlock()
	for ch := range webClients.m {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func webEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch := make(chan struct{}, 1)
	webClients.Lock()
	webClients.m[ch] = struct{}{}
	webClients.Unlock()
	defer func() {
		webClients.Lock()
		delete(webClients.m, ch)
		webClients.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			fmt.Fprint(w, "event: state\ndata: 1\n\n")
		case <-keepalive.C:
			fmt.Fprint(w, ": ping\n\n")
		}
		flusher.Flush()
	}
}

// guard is the trust boundary of a loopback server that any page in the browser can reach: a
// Host that is not ours means DNS rebinding, and a POST without htmx's header means a cross-site
// form (a fetch that adds the header needs a CORS preflight, which nothing here answers).
func guard(next http.Handler) http.Handler {
	own, _, _ := net.SplitHostPort(webAddr())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != own && host != "localhost" && host != "127.0.0.1" {
			http.Error(w, "unexpected Host", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost && r.Header.Get("HX-Request") != "true" {
			http.Error(w, "htmx requests only", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (m *monitor) serveWeb() {
	ln, err := net.Listen("tcp", webAddr())
	if err != nil {
		log.Printf("vibemon: dashboard not started: %v", err)
		return
	}
	log.Printf("vibemon: dashboard on http://%s", ln.Addr())
	if err := http.Serve(ln, m.webHandler()); err != nil {
		log.Printf("vibemon: dashboard stopped: %v", err)
	}
}
