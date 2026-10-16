package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestSecretFileModes: a credential is the owner's alone, except in systemd's
// credentials directory, where newer systemd hands it over as 0440.
func TestSecretFileModes(t *testing.T) {
	creds := t.TempDir()
	other := t.TempDir()
	t.Setenv("CREDENTIALS_DIRECTORY", creds)
	// Each case gets a file of its own: a file an earlier case left
	// read-only cannot be written again by anyone but root.
	n := 0
	write := func(dir string, mode os.FileMode) string {
		n++
		p := filepath.Join(dir, fmt.Sprintf("token%d", n))
		if err := os.WriteFile(p, []byte(" tok\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, c := range []struct {
		dir  string
		mode os.FileMode
		ok   bool
	}{
		{creds, 0o400, true},
		{creds, 0o440, true},
		{creds, 0o600, true},
		{creds, 0o460, false},
		{creds, 0o444, false},
		{other, 0o600, true},
		{other, 0o440, false},
		{other, 0o604, false},
	} {
		got, err := readSecretFile(write(c.dir, c.mode))
		if c.ok && (err != nil || got != "tok") {
			t.Errorf("%v in %s: %q, %v; want tok", c.mode, map[bool]string{true: "credentials", false: "elsewhere"}[c.dir == creds], got, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%v in %s: accepted", c.mode, map[bool]string{true: "credentials", false: "elsewhere"}[c.dir == creds])
		}
	}
}
