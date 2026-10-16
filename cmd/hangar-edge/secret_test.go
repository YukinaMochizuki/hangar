package main

import (
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
	write := func(dir string, mode os.FileMode) string {
		p := filepath.Join(dir, "token")
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
