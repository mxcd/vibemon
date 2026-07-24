package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"os/user"
	"strings"
)

// ponytail: shells out to /usr/bin/security rather than binding the Security framework via cgo.
// Claude Code creates its keychain item through that same binary, so its ACL already trusts it —
// reads and writes never raise a prompt, and no code-signing identity is needed. A native
// SecItem* call from our own binary would be a different app and could prompt. Upgrade path:
// github.com/keybase/go-keychain, only if the shell-out ever becomes the bottleneck.

func keychainAccount() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolve current user: %w", err)
	}
	return u.Username, nil
}

func keychainRead(service string) (string, error) {
	acct, err := keychainAccount()
	if err != nil {
		return "", err
	}
	cmd := exec.Command("/usr/bin/security", "find-generic-password", "-s", service, "-a", acct, "-w")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("keychain read %q: %w: %s", service, err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimRight(out.String(), "\r\n"), nil
}

// keychainWrite stores secret under service, then reads it back to confirm.
//
// The read-back is not paranoia: `security` exits 0 even when the two prompt reads disagree and it
// ends up storing an *empty* password. Trusting the exit code here would silently wipe the user's
// Claude Code credentials. Verified writes are the only safe kind.
func keychainWrite(service, secret string) error {
	if secret == "" {
		return fmt.Errorf("keychain write %q: refusing to store an empty secret", service)
	}
	if strings.ContainsAny(secret, "\n\r") {
		return fmt.Errorf("keychain write %q: secret must be single-line (got %d bytes with newlines)", service, len(secret))
	}
	acct, err := keychainAccount()
	if err != nil {
		return err
	}
	// ponytail: the secret goes on the argv. Passing it via security's stdin prompt instead would
	// keep it out of `ps`, but that path silently truncates at 128 bytes (measured) and our blobs
	// run to several KB — it stored a 1562-byte vault as 128 bytes and still exited 0. macOS only
	// exposes argv to the owning user, and Claude Code writes this same item the same way.
	// Upgrade path if that ever stops being acceptable: SecItemAdd via cgo, but check first that a
	// native call does not trip the item's keychain ACL.
	cmd := exec.Command("/usr/bin/security",
		"add-generic-password", "-U", "-a", acct, "-s", service, "-l", service, "-w", secret)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("keychain write %q: %w: %s", service, err, strings.TrimSpace(errb.String()))
	}
	got, err := keychainRead(service)
	if err != nil {
		return fmt.Errorf("keychain write %q: could not verify: %w", service, err)
	}
	if got != secret {
		return fmt.Errorf("keychain write %q: verification failed (wrote %d bytes, read back %d)", service, len(secret), len(got))
	}
	return nil
}
