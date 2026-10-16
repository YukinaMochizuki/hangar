package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const tokenStack = `apiVersion: tenancy.hangarlab.cloud/v1alpha1
kind: Tenant
metadata: {name: acme}
spec: {displayName: Acme}
---
apiVersion: tenancy.hangarlab.cloud/v1alpha1
kind: Project
metadata: {name: web}
spec: {tenant: acme}
`

// noSecret fails the test if any of the outputs holds the secret.
func noSecret(t *testing.T, secret string, outs ...string) {
	t.Helper()
	for _, o := range outs {
		if strings.Contains(o, secret[4:]) {
			t.Errorf("the secret was printed: %q", o)
		}
	}
}

// TestTokenCommands: create prints the secret once, alone on stdout; an
// agent uses it from HANGAR_TOKEN or a token-file context; rotate and
// delete end it; nothing else prints it.
func TestTokenCommands(t *testing.T) {
	w := newWorld(t)
	w.as("alice@example.com")
	w.must(tokenStack, "apply", "-f", "-")

	r := w.run("", "token", "create", "deployer", "--role", "project.editor", "--description", "deploys web")
	if r.code != 0 {
		t.Fatalf("token create: exit %d: %s", r.code, r.err)
	}
	secret := strings.TrimSuffix(r.out, "\n")
	if !strings.HasPrefix(secret, "hgr_") || strings.Contains(secret, "\n") {
		t.Fatalf("token create printed %q on stdout, want the secret alone", r.out)
	}
	if !strings.Contains(r.err, "web/deployer") || !strings.Contains(r.err, "project.editor") || !strings.Contains(r.err, "only now") {
		t.Errorf("token create said %q on stderr", r.err)
	}
	noSecret(t, secret, r.err)

	// From the environment, over the context's own credentials.
	env := map[string]string{"HANGAR_TOKEN": secret}
	who := w.runEnv("", env, "whoami")
	if who.code != 0 || who.out != "serviceToken:web/deployer\n" {
		t.Errorf("whoami with HANGAR_TOKEN: exit %d %q %q", who.code, who.out, who.err)
	}
	if r := w.runEnv("", env, "get", "containers"); r.code != 0 {
		t.Errorf("the token listing containers: exit %d: %s", r.code, r.err)
	}

	// From a file a context names.
	dir := t.TempDir()
	file := filepath.Join(dir, "deployer.token")
	if err := os.WriteFile(file, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.must("", "config", "set-context", "agent", "--endpoint", w.url, "--auth", "token", "--token-file", file, "--project", "web", "--use")
	if out := w.must("", "whoami"); out != "serviceToken:web/deployer\n" {
		t.Errorf("whoami with a token file: %q", out)
	}
	view := w.must("", "config", "view", "-o", "json")
	noSecret(t, secret, view)
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	r = w.run("", "whoami")
	if r.code == 0 || !strings.Contains(r.err, "chmod 600") {
		t.Errorf("a token file others may read: exit %d %q", r.code, r.err)
	}
	noSecret(t, secret, r.out, r.err)
	// Through symlinks, as Kubernetes mounts a secret: token -> ..data/token,
	// ..data -> a timestamped directory.
	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatal(err)
	}
	mount := filepath.Join(dir, "mount")
	stamped := filepath.Join(mount, "..2026_10_16")
	if err := os.MkdirAll(stamped, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stamped, "token"), []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..2026_10_16", filepath.Join(mount, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..data/token", filepath.Join(mount, "token")); err != nil {
		t.Fatal(err)
	}
	w.must("", "config", "set-context", "agent", "--endpoint", w.url, "--auth", "token", "--token-file", filepath.Join(mount, "token"), "--project", "web", "--use")
	if out := w.must("", "whoami"); out != "serviceToken:web/deployer\n" {
		t.Errorf("whoami through a symlinked token file: %q", out)
	}
	// What a symlink leads to is checked: one others may read is refused.
	open := filepath.Join(dir, "open.token")
	if err := os.WriteFile(open, []byte(secret+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.token")
	if err := os.Symlink(open, link); err != nil {
		t.Fatal(err)
	}
	// Nor is what is not a regular file read, and a FIFO does not hang it.
	fifo := filepath.Join(dir, "fifo.token")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{link: "chmod 600", dir: "not a regular file", fifo: "not a regular file"} {
		w.must("", "config", "set-context", "agent", "--endpoint", w.url, "--auth", "token", "--token-file", path, "--project", "web", "--use")
		r = w.run("", "whoami")
		if r.code == 0 || !strings.Contains(r.err, want) {
			t.Errorf("token file %s: exit %d %q, want %q", path, r.code, r.err, want)
		}
		noSecret(t, secret, r.out, r.err)
	}
	w.as("alice@example.com")

	// What get shows: which secret, never the secret.
	list := w.must("", "token", "get")
	if !strings.Contains(list, "deployer") || !strings.Contains(list, secret[:12]+"…") || !strings.Contains(list, "deploys web") {
		t.Errorf("token get:\n%s", list)
	}
	one := w.must("", "token", "get", "deployer", "-o", "json")
	noSecret(t, secret, list, one)

	// Rotating into a file prints nothing of the secret, and ends the old one.
	rotated := filepath.Join(dir, "rotated.token")
	r = w.run("", "token", "rotate", "deployer", "--secret-file", rotated)
	if r.code != 0 || !strings.Contains(r.out, rotated) {
		t.Fatalf("token rotate: exit %d %q %q", r.code, r.out, r.err)
	}
	fi, err := os.Stat(rotated)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the secret file: %v, mode %v", err, fi.Mode())
	}
	b, _ := os.ReadFile(rotated)
	second := strings.TrimSpace(string(b))
	noSecret(t, second, r.out, r.err)
	if r := w.runEnv("", env, "whoami"); r.code == 0 {
		t.Error("the rotated-away secret still works")
	}
	newEnv := map[string]string{"HANGAR_TOKEN": second}
	if r := w.runEnv("", newEnv, "whoami"); r.code != 0 {
		t.Errorf("the new secret: exit %d: %s", r.code, r.err)
	}
	// An existing file is refused before anything is issued.
	if r := w.run("", "token", "rotate", "deployer", "--secret-file", rotated); r.code != 2 || !strings.Contains(r.err, "exists") {
		t.Errorf("rotating into an existing file: exit %d %q", r.code, r.err)
	}
	if r := w.runEnv("", newEnv, "whoami"); r.code != 0 {
		t.Errorf("a refused rotate ended the secret: exit %d: %s", r.code, r.err)
	}

	// Deleting ends it.
	if out := w.must("", "token", "delete", "deployer"); !strings.Contains(out, "deleted") {
		t.Errorf("token delete printed %q", out)
	}
	if r := w.runEnv("", newEnv, "whoami"); r.code == 0 {
		t.Error("a deleted token's secret still works")
	}
}

// TestTokenEnvChecks: HANGAR_TOKEN must hold a service token, and is
// refused on the admin socket, without printing what it holds.
func TestTokenEnvChecks(t *testing.T) {
	w := newWorld(t)
	w.as("alice@example.com")
	r := w.runEnv("", map[string]string{"HANGAR_TOKEN": "hga_notaservicetoken"}, "whoami")
	if r.code != 2 || !strings.Contains(r.err, "service token") || strings.Contains(r.err, "notaservicetoken") {
		t.Errorf("a non-service token in HANGAR_TOKEN: exit %d %q", r.code, r.err)
	}
	w.must("", "config", "set-context", "local", "--endpoint", "unix://"+w.admin, "--use")
	r = w.runEnv("", map[string]string{"HANGAR_TOKEN": "hgr_x"}, "whoami")
	if r.code != 2 || !strings.Contains(r.err, "admin socket") {
		t.Errorf("HANGAR_TOKEN on the admin socket: exit %d %q", r.code, r.err)
	}
	r = w.run("", "config", "set-context", "agent", "--endpoint", w.url, "--auth", "token")
	if r.code != 2 || !strings.Contains(r.err, "--token-file") {
		t.Errorf("token auth without a file: exit %d %q", r.code, r.err)
	}
}
