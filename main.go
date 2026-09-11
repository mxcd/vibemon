package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		if err := runGUI(); err != nil {
			fatal(err)
		}
		return
	}
	var err error
	switch os.Args[1] {
	case "capture":
		err = cmdCapture()
	case "list":
		err = cmdList()
	case "switch":
		if len(os.Args) < 3 {
			err = fmt.Errorf("usage: vibemon switch <email>")
			break
		}
		err = cmdSwitch(os.Args[2])
	case "remove", "forget":
		if len(os.Args) < 3 {
			err = fmt.Errorf("usage: vibemon remove <email>")
			break
		}
		err = cmdRemove(os.Args[2])
	case "usage":
		err = cmdUsage()
	case "add-token":
		if len(os.Args) < 3 {
			err = fmt.Errorf("usage: vibemon add-token <email> [token]   (token on stdin when omitted)")
			break
		}
		err = cmdAddToken(os.Args[2], os.Args[3:])
	case "token":
		if len(os.Args) < 3 {
			err = fmt.Errorf("usage: vibemon token <email>")
			break
		}
		err = cmdToken(os.Args[2])
	case "pick":
		err = cmdPick(os.Args[2:])
	case "exec":
		os.Exit(cmdExec(os.Args[2:]))
	case "help", "-h", "--help":
		usageText()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usageText() {
	fmt.Println(`vibemon — Claude Code usage monitor and account switcher

  vibemon                  run the menu bar app
  vibemon capture         store the account Claude Code is currently logged into
  vibemon list             list stored accounts (* marks the active one)
  vibemon switch <email>   make an account active for the next Claude Code start
  vibemon remove <email>   forget an account (does not log Claude Code out)
  vibemon usage            show usage for every stored account

  vibemon add-token <email> [token]   store a 'claude setup-token' token for headless runs
  vibemon token <email>               print an account's headless token (for scripts)
  vibemon pick [--workdir d] [--model m] [--json]
                           show which account exec would use there, and why the others are skipped
  vibemon exec [flags] [--] [claude args...]
                           run claude under the project's first available account; with -p,
                           resume under the next account when a limit hits (vibemon exec --help)`)
}

func cmdCapture() error {
	a, err := capture()
	if err != nil {
		return err
	}
	fmt.Printf("captured %s (%s, %s)\n", a.Email, a.Plan, a.RateTier)
	return nil
}

func cmdList() error {
	v, err := loadVault()
	if err != nil {
		return err
	}
	if len(v) == 0 {
		fmt.Println("no accounts stored — run `vibemon capture` while Claude Code is logged in")
		return nil
	}
	active := activeUUID(v)
	for _, a := range v.sorted() {
		marker := " "
		if a.UUID == active {
			marker = "*"
		}
		flag := ""
		if a.NeedsReauth {
			flag = "  [needs re-auth]"
		}
		if a.HeadlessToken != "" {
			flag += "  [token]"
		}
		if a.OAuth.AccessToken == "" {
			flag += "  [no login captured]"
		}
		fmt.Printf("%s %-34s %-22s %s%s\n", marker, a.Email, a.planLabel(), a.OrgName, flag)
	}
	return nil
}

func cmdRemove(email string) error {
	unlock := lockVault()
	defer unlock()
	v, err := loadVault()
	if err != nil {
		return err
	}
	target, err := findByEmail(v, "", email)
	if err != nil {
		return err
	}
	wasActive := target.UUID == activeUUID(v)
	if err := forget(v, target.UUID); err != nil {
		return err
	}
	fmt.Printf("removed %s from vibemon\n", target.Email)
	if wasActive {
		fmt.Println("note: Claude Code is still signed in as this account — vibemon just stopped tracking it")
	}
	return nil
}

func cmdSwitch(email string) error {
	unlock := lockVault()
	defer unlock()
	v, err := loadVault()
	if err != nil {
		return err
	}
	target, err := findByEmail(v, kindClaude, email)
	if err != nil {
		return err
	}
	if target.UUID == activeUUID(v) {
		fmt.Printf("%s is already active\n", target.Email)
		return nil
	}
	if n := claudeSessionsRunning(); n > 0 {
		fmt.Printf("warning: %d claude process(es) running — they keep the old account until restarted\n", n)
	}
	if err := switchTo(v, target.UUID); err != nil {
		return err
	}
	fmt.Printf("switched to %s — restart Claude Code to pick it up\n", target.Email)
	return nil
}

func cmdUsage() error {
	unlock := lockVault()
	defer unlock()
	v, err := loadVault()
	if err != nil {
		return err
	}
	if len(v) == 0 {
		fmt.Println("no accounts stored — run `vibemon capture` first")
		return nil
	}
	active := activeUUID(v)
	polled := map[string]Usage{}
	defer cacheUsage(polled)
	for _, a := range v.sorted() {
		marker := " "
		if a.UUID == active {
			marker = "*"
		}
		u, err := usageFor(v, a, a.UUID == active)
		if err != nil {
			fmt.Printf("%s %-34s  %v\n", marker, a.Email, err)
			continue
		}
		polled[a.UUID] = u
		line := fmt.Sprintf("%s %-34s  session %3.0f%% (%s)   weekly %3.0f%% (%s)",
			marker, a.Email, u.Session.Percent, until(u.Session.ResetsAt), u.Weekly.Percent, until(u.Weekly.ResetsAt))
		if u.Scoped != nil {
			line += fmt.Sprintf("   %s %3.0f%%", u.Scoped.Label, u.Scoped.Percent)
		}
		fmt.Println(line)
	}
	return saveVault(v)
}

func until(t *time.Time) string {
	if t == nil {
		return "?"
	}
	d := time.Until(*t)
	if d < 0 {
		return "now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%02dh", int(d.Hours())/24, int(d.Hours())%24)
}

// cacheUsage publishes freshly polled numbers to the fleet ledger, where exec reads them. exec never
// polls itself: several runners refreshing the same parked token at once would race the rotation.
func cacheUsage(polled map[string]Usage) {
	if len(polled) == 0 {
		return
	}
	_, _ = updateFleet(func(st *fleetState) {
		for k, u := range polled {
			st.Usage[k] = u
		}
	})
}

func cmdAddToken(email string, rest []string) error {
	token := ""
	if len(rest) > 0 {
		token = rest[0]
	} else {
		fmt.Fprintln(os.Stderr, "paste the token from `claude setup-token`, then Enter:")
		line, err := bufio.NewReader(io.LimitReader(os.Stdin, 4096)).ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		token = strings.TrimSpace(line)
	}
	unlock := lockVault()
	defer unlock()
	v, err := loadVault()
	if err != nil {
		return err
	}
	a, err := addHeadlessToken(v, email, token)
	if err != nil {
		return err
	}
	fmt.Printf("stored headless token for %s\n", a.Email)
	if a.OAuth.AccessToken == "" {
		fmt.Println("note: no login captured for this email, so vibemon cannot show its usage; " +
			"`claude auth login` as this account and `vibemon capture` to add that")
	}
	return nil
}

func cmdToken(email string) error {
	v, err := loadVault()
	if err != nil {
		return err
	}
	a, err := findByEmail(v, kindClaude, email)
	if err != nil {
		return err
	}
	if a.HeadlessToken == "" {
		return fmt.Errorf("%s has no headless token: `claude setup-token` as that account, then `vibemon add-token %s`", a.Email, a.Email)
	}
	fmt.Println(a.HeadlessToken)
	return nil
}

func cmdPick(args []string) error {
	o, err := parseExecArgs(args)
	if err != nil {
		return err
	}
	v, err := loadVault()
	if err != nil {
		return err
	}
	kinds := []string{o.Kind}
	if o.Kind == "" || o.Kind == kindAll {
		kinds = []string{kindClaude, kindCodex}
	}
	st := readFleet()
	p := loadPrefs()
	var runnable, skipped []candidate
	var project *projectPolicy
	for _, kind := range kinds {
		opts := o.pickOptions
		opts.Kind = kind
		run, skip, proj := rank(v, p, &st, opts)
		runnable, skipped = append(runnable, run...), append(skipped, skip...)
		if proj != nil {
			project = proj
		}
	}
	if o.JSON {
		type row struct {
			Kind   string    `json:"kind"`
			Email  string    `json:"email"`
			Usage  *Usage    `json:"usage,omitempty"`
			Reason string    `json:"reason,omitempty"`
			Until  time.Time `json:"until,omitempty"`
		}
		out := map[string]any{"runnable": []row{}, "skipped": []row{}}
		for _, c := range runnable {
			out["runnable"] = append(out["runnable"].([]row), row{Kind: c.Account.kind(), Email: c.Account.Email, Usage: c.Usage})
		}
		for _, c := range skipped {
			out["skipped"] = append(out["skipped"].([]row), row{Kind: c.Account.kind(), Email: c.Account.Email, Usage: c.Usage, Reason: c.Reason, Until: c.Until})
		}
		if project != nil {
			out["project"] = project.Path
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	if project != nil {
		fmt.Printf("project %s\n", project.Path)
	}
	// One ">" per kind: each kind picks its own first runnable account.
	firstOf := map[string]bool{}
	for _, c := range runnable {
		marker := " "
		if !firstOf[c.Account.kind()] {
			marker, firstOf[c.Account.kind()] = ">", true
		}
		fmt.Printf("%s %-7s %-34s %s\n", marker, c.Account.kind(), c.Account.Email, usageNote(c.Usage))
	}
	for _, c := range skipped {
		fmt.Printf("  %-7s %-34s skipped: %s%s\n", c.Account.kind(), c.Account.Email, c.Reason, untilNote(c.Until))
	}
	if len(runnable) == 0 {
		return fmt.Errorf("no account has headroom")
	}
	return nil
}

func usageNote(u *Usage) string {
	if u == nil {
		return "usage unknown (monitor not running, or no login captured)"
	}
	s := fmt.Sprintf("session %3.0f%% (%s)  weekly %3.0f%% (%s)", u.Session.Percent, until(u.Session.ResetsAt), u.Weekly.Percent, until(u.Weekly.ResetsAt))
	if u.Scoped != nil {
		s += fmt.Sprintf("  %s %3.0f%%", u.Scoped.Label, u.Scoped.Percent)
	}
	for _, g := range u.Extra {
		s += fmt.Sprintf("  %s %3.0f%%", g.Label, g.Percent)
	}
	return s + "  as of " + u.FetchedAt.Local().Format("15:04")
}
