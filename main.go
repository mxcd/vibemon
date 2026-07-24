package main

import (
	"fmt"
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
  vibemon usage            show usage for every stored account`)
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
		fmt.Printf("%s %-34s %-22s %s%s\n", marker, a.Email, a.planLabel(), a.OrgName, flag)
	}
	return nil
}

// findByEmail resolves the user-facing identifier used by every command that takes one.
func findByEmail(v Vault, email string) (*Account, error) {
	for _, a := range v {
		if strings.EqualFold(a.Email, email) {
			return a, nil
		}
	}
	return nil, fmt.Errorf("no stored account matching %q — run `vibemon list`", email)
}

func cmdRemove(email string) error {
	v, err := loadVault()
	if err != nil {
		return err
	}
	target, err := findByEmail(v, email)
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
	v, err := loadVault()
	if err != nil {
		return err
	}
	target, err := findByEmail(v, email)
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
	v, err := loadVault()
	if err != nil {
		return err
	}
	if len(v) == 0 {
		fmt.Println("no accounts stored — run `vibemon capture` first")
		return nil
	}
	active := activeUUID(v)
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
