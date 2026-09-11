package main

import (
	"regexp"
	"strings"
	"time"
)

// Fallback order when a per-model cap hits. Anthropic names the model in the message
// ("You've reached your Fable limit"); the next entry is what the same account can still run.
var modelOrder = []string{"fable", "opus", "sonnet", "haiku"}

func nextModel(current string, order []string) string {
	current = strings.ToLower(current)
	for i, m := range order {
		if m == current && i+1 < len(order) {
			return order[i+1]
		}
	}
	return ""
}

func isModel(name string) bool {
	return nextModel(name, modelOrder) != "" || strings.EqualFold(name, modelOrder[len(modelOrder)-1])
}

// outcome classifies one headless attempt. Everything the Paloma One runner learned the hard way
// is a case here: a limit and a per-model cap look alike but call for different moves, a missing
// transcript is not a limit, and empty output is a launch hiccup, not a verdict.
type outcome int

const (
	outOK        outcome = iota
	outLimit             // account limit: bench until the named reset, resume elsewhere
	outModelCap          // per-model weekly cap: same account, next model
	outNoSession         // "No conversation found": the id never got a transcript, start over
	outEmpty             // nothing at all: transient launch failure, retry the same account
	outTooLong           // context exhausted: terminal, the caller needs a fresh brief
	outAuth              // the token itself is rejected
	outFailed            // any other non-zero exit
)

func (o outcome) String() string {
	return [...]string{"ok", "limit", "model-cap", "no-session", "empty", "too-long", "auth", "failed"}[o]
}

var (
	reTooLong   = regexp.MustCompile(`(?i)prompt is too long`)
	reNoSession = regexp.MustCompile(`(?i)no conversation found with session id`)
	reReached   = regexp.MustCompile(`(?i)(?:reached|hit) your (\w+)(?: usage)? limit`)
	reLimit     = regexp.MustCompile(`(?i)(?:hit|reached) your\b[^.\n]*\blimit|usage limit|session limit|weekly limit`)
	reAuth      = regexp.MustCompile(`(?i)oauth 401|"authentication_error"|invalid authentication|token (?:is )?(?:expired|invalid|revoked)`)
)

// classify inspects stdout and stderr together: the limit JSON lands on stdout, launch errors on
// stderr, and Claude Code has moved messages between the two before. detail carries the capped
// model for outModelCap and the raw message line for the others (untruncated: the reset time is
// parsed out of it).
//
// The message patterns only run on a failed turn (non-zero exit or is_error in the JSON result).
// A successful answer that merely talks about usage limits is an answer, not a limit.
func classify(stdout, stderr string, exitCode int) (outcome, string) {
	failed := exitCode != 0 || strings.Contains(stdout, `"is_error":true`)
	if !failed {
		if strings.TrimSpace(stdout) == "" {
			return outEmpty, "empty output"
		}
		return outOK, ""
	}
	text := stdout + "\n" + stderr
	switch {
	case reTooLong.MatchString(text):
		return outTooLong, "prompt is too long"
	case reNoSession.MatchString(text):
		return outNoSession, "no conversation found with session id"
	}
	if m := reReached.FindStringSubmatch(text); m != nil && isModel(m[1]) {
		return outModelCap, strings.ToLower(m[1])
	}
	if m := reLimit.FindString(text); m != "" {
		return outLimit, firstLine(text, m)
	}
	if reAuth.MatchString(text) {
		return outAuth, "token rejected"
	}
	// Nothing at all on either stream is a launch that never happened; a message on stderr with
	// nothing on stdout is a real failure (bad flag, bad cwd) and retrying it would not help.
	if strings.TrimSpace(stdout) == "" && strings.TrimSpace(stderr) == "" {
		return outEmpty, "empty output"
	}
	return outFailed, firstLine(stderr+"\n"+stdout, "")
}

func firstLine(text, containing string) string {
	for _, line := range strings.Split(text, "\n") {
		if containing == "" || strings.Contains(line, containing) {
			if l := strings.TrimSpace(line); l != "" {
				return l
			}
		}
	}
	return strings.TrimSpace(text)
}

// "resets 9:40am (Europe/Rome)", "resets 2:40pm", "resets at 15:00", "resets Sep 14th, 2026 12:22 AM"
var reReset = regexp.MustCompile(`(?i)resets?\s+(?:at\s+)?` +
	`((?:[A-Z][a-z]{2,8}\.? \d{1,2}(?:st|nd|rd|th)?,? \d{4},? )?\d{1,2}(?::\d{2})?\s*(?:am|pm)?)` +
	`(?:\s*\(([^)]+)\))?`)

var reOrdinal = regexp.MustCompile(`(\d)(st|nd|rd|th)\b`)

// parseReset reads the reset time out of a limit message. A bare clock time means the next such
// time in the named zone (today or tomorrow); the zone falls back to loc when absent or unknown.
func parseReset(msg string, now time.Time, loc *time.Location) (time.Time, bool) {
	m := reReset.FindStringSubmatch(msg)
	if m == nil {
		return time.Time{}, false
	}
	if m[2] != "" {
		if z, err := time.LoadLocation(strings.TrimSpace(m[2])); err == nil {
			loc = z
		}
	}
	when := strings.ToUpper(strings.Join(strings.Fields(reOrdinal.ReplaceAllString(m[1], "$1")), " "))
	when = strings.ReplaceAll(strings.ReplaceAll(when, ",", ""), ".", "")
	for _, layout := range []string{"Jan 2 2006 3:04PM", "Jan 2 2006 3:04 PM", "January 2 2006 3:04PM", "January 2 2006 3:04 PM"} {
		if t, err := time.ParseInLocation(layout, when, loc); err == nil {
			return t, true
		}
	}
	for _, layout := range []string{"3:04PM", "3:04 PM", "3PM", "3 PM", "15:04"} {
		t, err := time.ParseInLocation(layout, when, loc)
		if err != nil {
			continue
		}
		local := now.In(loc)
		t = time.Date(local.Year(), local.Month(), local.Day(), t.Hour(), t.Minute(), 0, 0, loc)
		// A minute of grace: "resets 9:40am" printed at 9:40:30 means now, not tomorrow.
		if t.Before(now.Add(-time.Minute)) {
			t = time.Date(local.Year(), local.Month(), local.Day()+1, t.Hour(), t.Minute(), 0, 0, loc) // calendar day, DST-safe
		}
		return t, true
	}
	return time.Time{}, false
}
