package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// secretStore holds the two secrets vibemon touches: Claude Code's own login (ccService) and our
// vault (vaultService). macOS keeps both in the keychain (keychain_darwin.go), Linux in plain files
// (secrets_linux.go). The rules in CLAUDE.md hold for both: write verifies before it returns, and a
// missing secret reads as an error matching fs.ErrNotExist or security's "could not be found".
type secretStore interface {
	read(service string) (string, error)
	write(service, secret string) error
	remove(service string) error
}

// fileSecrets keeps each secret in a file of its own. This is how Claude Code stores its login on
// Linux (read off its 2.1.312 linux-x64 build: <config dir>/.credentials.json, written 0600 by temp
// file and rename, re-read when its mtime changes), so the vault is kept the same way. path maps a
// service to its file; tests point it into a temp dir.
//
// ponytail: no lock of its own. Every vibemon read-modify-write of either secret already runs under
// lockVault, and the rename means no reader ever sees half a file. Claude Code serialises its own
// writes on a lock directory (<config dir>/.storage-write.lock, proper-lockfile, stale after 15 s);
// taking that too would close the last race, a token refresh landing between a switch's read and its
// write, which the keychain on macOS has never closed either.
type fileSecrets struct {
	path func(service string) string
}

// read returns the file exactly as stored, so switchTo's restore writes back the very bytes Claude
// Code wrote.
func (f fileSecrets) read(service string) (string, error) {
	raw, err := os.ReadFile(f.path(service))
	return string(raw), err // the *PathError names the file, and a missing one matches fs.ErrNotExist
}

// write replaces the file atomically, keeping its mode (0600 when it is new), then reads it back to
// confirm, in the spirit of rule 2. There is no single-line rule here: that one exists because
// `security -w` prints a multi-line secret as hex; a file holds its bytes as they are.
func (f fileSecrets) write(service, secret string) error {
	if secret == "" {
		return fmt.Errorf("write %s: refusing to store an empty secret", service)
	}
	path := f.path(service)
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm() // Claude Code sets 0600 on every write; whatever is there now stays
	}
	err := os.MkdirAll(filepath.Dir(path), 0o700)
	if err == nil {
		err = writeFileAtomic(path, []byte(secret), mode)
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", service, err)
	}
	got, err := f.read(service)
	if err != nil {
		return fmt.Errorf("write %s: could not verify: %w", service, err)
	}
	if got != secret {
		return fmt.Errorf("write %s: verification failed (wrote %d bytes, read back %d)", service, len(secret), len(got))
	}
	return nil
}

func (f fileSecrets) remove(service string) error {
	if err := os.Remove(f.path(service)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
