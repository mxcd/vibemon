package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestHistoryRoundTrip(t *testing.T) {
	t.Setenv("VIBEMON_STATE", t.TempDir())
	now := time.Now()
	old := map[string]Usage{"a": {Session: Gauge{Label: "Session", Percent: 10}, Weekly: Gauge{Label: "Weekly", Percent: 5}}}
	fresh := map[string]Usage{
		"a": {Session: Gauge{Label: "Session", Percent: 40}, Weekly: Gauge{Label: "Weekly", Percent: 7}, Scoped: &Gauge{Label: "Fable", Percent: 90}},
		"b": {Session: Gauge{Label: "Session"}, Weekly: Gauge{Label: "Weekly", Percent: 1}, Extra: []Gauge{{Label: "gpt-reserve", Percent: 3}}},
	}
	if err := recordHistory(old, now.Add(-20*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := recordHistory(fresh, now); err != nil {
		t.Fatal(err)
	}
	if got := readHistory(now.Add(-time.Hour)); len(got) != 2 {
		t.Fatalf("want the 2 fresh samples, got %d", len(got))
	}
	all := readHistory(time.Time{})
	if len(all) != 3 || all[0].Account != "a" || all[0].Gauges["Session"] != 10 {
		t.Fatalf("file order and content: %+v", all)
	}
	for _, s := range all[1:] {
		switch s.Account {
		case "a":
			if s.Gauges["Fable"] != 90 {
				t.Errorf("scoped gauge lost: %v", s.Gauges)
			}
		case "b":
			if s.Gauges["gpt-reserve"] != 3 || s.Gauges["Session"] != 0 {
				t.Errorf("extra gauge lost: %v", s.Gauges)
			}
		}
	}

	if err := compactHistory(now.Add(-historyKeep)); err != nil {
		t.Fatal(err)
	}
	if got := readHistory(time.Time{}); len(got) != 2 {
		t.Fatalf("compaction should drop the 20-day-old sample, kept %d", len(got))
	}
	raw, _ := os.ReadFile(historyPath())
	if strings.Count(string(raw), "\n") != 2 {
		t.Errorf("compacted file is not one line per sample: %q", raw)
	}
	// A torn line must cost one point, not the whole file.
	if err := os.WriteFile(historyPath(), append(raw, []byte(`{"at":"2026-`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readHistory(time.Time{}); len(got) != 2 {
		t.Errorf("a corrupt tail line should be skipped, got %d samples", len(got))
	}
}
