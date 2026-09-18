package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// The usage history feeds the dashboard's rundown charts: one JSON line per account per poll,
// appended by whoever polled (the monitor every tick, `vibemon usage` on demand). Nothing else in
// vibemon reads it, so a lost or corrupt line costs one point on a chart and nothing more.
//
// ponytail: append-only JSONL, compacted by size. A week of three accounts is well under a
// megabyte; move to SQLite if the retention ever grows past a few months.

type sample struct {
	At      time.Time          `json:"at"`
	Account string             `json:"account"` // vault key
	Gauges  map[string]float64 `json:"gauges"`  // by gauge label: Session, Weekly, the model names
}

const (
	historyKeep      = 14 * 24 * time.Hour
	historyCompactAt = 8 << 20
)

func historyPath() string { return filepath.Join(stateDir(), "history.jsonl") }

// recordHistory appends one sample per polled account. Lines are far below PIPE_BUF, so O_APPEND
// keeps the monitor and a concurrent CLI poll from interleaving without a lock.
func recordHistory(polled map[string]Usage, now time.Time) error {
	if len(polled) == 0 {
		return nil
	}
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(historyPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	for key, u := range polled {
		line, err := json.Marshal(sample{At: now, Account: key, Gauges: gaugesOf(u)})
		if err != nil {
			continue
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			f.Close()
			return err
		}
	}
	info, err := f.Stat()
	f.Close()
	if err == nil && info.Size() > historyCompactAt {
		return compactHistory(now.Add(-historyKeep))
	}
	return nil
}

func gaugesOf(u Usage) map[string]float64 {
	g := map[string]float64{u.Session.Label: u.Session.Percent, u.Weekly.Label: u.Weekly.Percent}
	if u.Scoped != nil {
		g[u.Scoped.Label] = u.Scoped.Percent
	}
	for _, e := range u.Extra {
		g[e.Label] = e.Percent
	}
	return g
}

// readHistory returns every sample taken at or after since, in file order. The monitor and the
// CLI may append out of order by a few seconds; chart builders sort per series.
func readHistory(since time.Time) []sample {
	f, err := os.Open(historyPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []sample
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var s sample
		if json.Unmarshal(sc.Bytes(), &s) != nil || s.At.Before(since) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// compactHistory rewrites the file with only the samples newer than since, atomically, so a reader
// never sees a half-written file.
func compactHistory(since time.Time) error {
	kept := readHistory(since)
	tmp := historyPath() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, s := range kept {
		line, err := json.Marshal(s)
		if err != nil {
			continue
		}
		w.Write(append(line, '\n'))
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, historyPath())
}
