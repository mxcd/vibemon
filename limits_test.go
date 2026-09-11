package main

import (
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	limitJSON := `{"type":"result","is_error":true,"result":"You've hit your session limit · resets 9:40am (Europe/Rome)"}`
	cases := []struct {
		name         string
		stdout, serr string
		exit         int
		want         outcome
		detail       string
	}{
		{"normal", `{"type":"result","result":"done"}`, "", 0, outOK, ""},
		{"session limit json", limitJSON, "", 1, outLimit, ""},
		{"weekly limit on stderr", "", "You've hit your weekly limit · resets Sep 14th, 2026 12:22 AM", 1, outLimit, ""},
		{"reached limit without window", "You've reached your limit · resets 4pm", "", 1, outLimit, ""},
		{"model cap", `{"is_error":true,"result":"You've reached your Fable limit · resets 3pm"}`, "", 1, outModelCap, "fable"},
		{"model cap opus usage", "You've reached your Opus usage limit", "", 1, outModelCap, "opus"},
		{"missing transcript", "", "No conversation found with session ID 1234", 1, outNoSession, ""},
		{"empty", "  \n", "", 0, outEmpty, ""},
		{"too long", `{"is_error":true,"result":"Prompt is too long"}`, "", 1, outTooLong, ""},
		{"auth", "", "OAuth 401: keeping the user-supplied CLAUDE_CODE_OAUTH_TOKEN", 1, outAuth, ""},
		{"other failure", "boom", "segfault", 2, outFailed, "segfault"},
		// A successful answer that talks about limits is an answer. Benching on prose once sent a
		// whole fleet through every account for a summary of this very README.
		{"prose mentions limits", `{"is_error":false,"result":"vibemon shows usage limits and the session limit."}`, "", 0, outOK, ""},
		{"prose mentions too long", "The prompt is too long for that, so I trimmed it.", "", 0, outOK, ""},
		{"stderr-only launch failure", "", "error: unknown option '--bogus'", 1, outFailed, "error: unknown option '--bogus'"},
		{"hit your model limit", "You've hit your Opus limit · resets 3pm", "", 1, outModelCap, "opus"},
		{"codex usage limit", "", "ERROR: You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at Sep 15th, 2026 7:59 PM.", 1, outLimit, ""},
		{"codex out of credits", "", "ERROR: You're out of credits.", 1, outLimit, ""},
		{"codex not logged in", "", "ERROR: unexpected status 401 Unauthorized: Missing bearer or basic authentication in header, url: https://api.openai.com/v1/responses", 1, outAuth, ""},
		{"codex prose about limits", "The usage limit is documented in the README.", "", 0, outOK, ""},
	}
	for _, tc := range cases {
		got, detail := classify(tc.stdout, tc.serr, tc.exit)
		if got != tc.want {
			t.Errorf("%s: want %s, got %s (%s)", tc.name, tc.want, got, detail)
		}
		if tc.detail != "" && detail != tc.detail {
			t.Errorf("%s: detail want %q, got %q", tc.name, tc.detail, detail)
		}
	}
}

// The real JSON result line carries ~110 bytes of fields before "result"; the reset time must
// survive whatever display truncation happens later.
func TestClassifyKeepsResetTimeInLongJSON(t *testing.T) {
	line := `{"type":"result","subtype":"error_during_execution","is_error":true,"duration_ms":1234,"duration_api_ms":1000,"num_turns":1,"result":"You've hit your weekly limit · resets Sep 14th, 2026 12:22 AM","session_id":"2d182dd5-4bf7-4021-8ce3-bbf2e331f4d2","total_cost_usd":0}`
	kind, detail := classify(line, "", 1)
	if kind != outLimit {
		t.Fatalf("want limit, got %s", kind)
	}
	berlin, _ := time.LoadLocation("Europe/Berlin")
	when, ok := parseReset(detail, time.Date(2026, 9, 8, 10, 0, 0, 0, berlin), berlin)
	if !ok || when.Day() != 14 {
		t.Fatalf("reset time lost from detail %q: %v %v", detail, when, ok)
	}
}

func TestParseReset(t *testing.T) {
	berlin, _ := time.LoadLocation("Europe/Berlin")
	rome, _ := time.LoadLocation("Europe/Rome")
	now := time.Date(2026, 9, 8, 10, 0, 0, 0, berlin)
	cases := []struct {
		msg  string
		want time.Time
	}{
		{"You've hit your session limit · resets 9:40am (Europe/Rome)", time.Date(2026, 9, 9, 9, 40, 0, 0, rome)},
		{"resets 2:40pm", time.Date(2026, 9, 8, 14, 40, 0, 0, berlin)},
		{"resets 4pm (America/New_York)", time.Date(2026, 9, 8, 16, 0, 0, 0, mustLoc("America/New_York"))},
		{"resets at 15:00", time.Date(2026, 9, 8, 15, 0, 0, 0, berlin)},
		{"limit · resets 10:00am", time.Date(2026, 9, 8, 10, 0, 0, 0, berlin)},
		{"resets Sep 14th, 2026 12:22 AM", time.Date(2026, 9, 14, 0, 22, 0, 0, berlin)},
		{"resets 9:40am (Mars/Olympus)", time.Date(2026, 9, 9, 9, 40, 0, 0, berlin)},
		{"purchase more credits or try again at Sep 15th, 2026 7:59 PM.", time.Date(2026, 9, 15, 19, 59, 0, 0, berlin)},
		{"try again at 11:59 PM", time.Date(2026, 9, 8, 23, 59, 0, 0, berlin)},
	}
	// Across the DST switch the next 9:00 is 25 hours away, not 24.
	dstEve := time.Date(2026, 10, 24, 10, 0, 0, 0, berlin)
	if got, _ := parseReset("resets 9:00am", dstEve, berlin); got.Hour() != 9 || got.Day() != 25 {
		t.Errorf("DST: want 25.10. 09:00, got %s", got)
	}
	for _, tc := range cases {
		got, ok := parseReset(tc.msg, now, berlin)
		if !ok {
			t.Errorf("%q: not parsed", tc.msg)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("%q: want %s, got %s", tc.msg, tc.want, got)
		}
	}
	if _, ok := parseReset("nothing here", now, berlin); ok {
		t.Error("garbage must not parse")
	}
}

func mustLoc(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

func TestNextModel(t *testing.T) {
	if got := nextModel("Fable", modelOrder); got != "opus" {
		t.Errorf("after fable: want opus, got %q", got)
	}
	if got := nextModel("haiku", modelOrder); got != "" {
		t.Errorf("after haiku: want none, got %q", got)
	}
	if got := nextModel("opus", []string{"opus", "haiku"}); got != "haiku" {
		t.Errorf("custom order: want haiku, got %q", got)
	}
}

// codex names its reset in the same line as the limit; without it the bench would be an hour and
// the account would be retried three days early, four times an hour.
func TestBenchForCodexLimit(t *testing.T) {
	const msg = "ERROR: You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at Sep 15th, 2026 7:59 PM."
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.Local)
	want := time.Date(2026, 9, 15, 20, 1, 0, 0, time.Local)
	if got := benchFor(msg, now); !got.Equal(want) {
		t.Errorf("bench until: want %s, got %s", want, got)
	}
}
