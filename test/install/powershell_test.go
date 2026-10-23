//go:build windows

package install_test

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

const (
	cliName     = "hangar.exe"
	installerIn = "install.ps1.in"
)

func archiveName(tag string) string {
	return fmt.Sprintf("hangar_%s_windows_%s.zip", tag, runtime.GOARCH)
}

// powershell is what runs install.ps1: Windows PowerShell 5.1, unless
// HANGAR_TEST_POWERSHELL names another, such as pwsh.
func powershell() string {
	if p := os.Getenv("HANGAR_TEST_POWERSHELL"); p != "" {
		return p
	}
	return "powershell.exe"
}

// fakeSource is a hangar.exe that answers `which-release` with its tag, waits
// for its stdin to close on `hold`, and runs the real CLI otherwise.
const fakeSource = `package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
)

var tag, real string

func main() {
	if len(os.Args) > 1 && os.Args[1] == "which-release" {
		fmt.Println(tag)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "hold" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	cmd := exec.Command(real, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
`

var (
	fakesMu sync.Mutex
	fakes   = map[string]string{}
)

// fakeCLI builds fakeSource for tag, once per tag.
func fakeCLI(t *testing.T, tag, cli string) string {
	t.Helper()
	fakesMu.Lock()
	defer fakesMu.Unlock()
	if p, ok := fakes[tag]; ok {
		return p
	}
	src, err := os.MkdirTemp("", "hangar-install-fake")
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"main.go": fakeSource, "go.mod": "module fake\n\ngo 1.22\n"} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(src, "hangar.exe")
	cmd := exec.Command("go", "build", "-o", out, fmt.Sprintf(`-ldflags=-X "main.tag=%s" -X "main.real=%s"`, tag, cli), ".")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build the fake hangar.exe: %v\n%s", err, b)
	}
	fakes[tag] = out
	return out
}

// packCLI packs fakeCLI's hangar.exe for tag alone in a zip, as release.sh
// packs the real one.
func packCLI(t *testing.T, tag, cli string) []byte {
	exe, err := os.ReadFile(fakeCLI(t, tag, cli))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	h := &zip.FileHeader{Name: "hangar.exe", Method: zip.Deflate, Modified: time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)}
	w, err := zw.CreateHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(exe); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// userPath is the user's PATH as the registry holds it, unexpanded.
func userPath(t *testing.T) (string, uint32, bool) {
	t.Helper()
	k, err := registry.OpenKey(registry.CURRENT_USER, "Environment", registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = k.Close() }()
	v, kind, err := k.GetStringValue("Path")
	if errors.Is(err, registry.ErrNotExist) {
		return "", 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return v, kind, true
}

// keepUserPath puts the user's PATH back as it was once the test is done,
// since install.ps1 adds to it.
func keepUserPath(t *testing.T) {
	v, kind, ok := userPath(t)
	t.Cleanup(func() {
		k, err := registry.OpenKey(registry.CURRENT_USER, "Environment", registry.SET_VALUE)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = k.Close() }()
		switch {
		case !ok:
			err = k.DeleteValue("Path")
			if errors.Is(err, registry.ErrNotExist) {
				err = nil
			}
		case kind == registry.EXPAND_SZ:
			err = k.SetExpandStringValue("Path", v)
		default:
			err = k.SetStringValue("Path", v)
		}
		if err != nil {
			t.Errorf("put the user's PATH back: %v", err)
		}
	})
}

func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// psParams are install.sh's options as install.ps1's parameters.
var psParams = map[string]string{"--dir": "-Dir", "--version": "-Version", "--endpoint": "-Endpoint", "--context": "-Context"}

func (s *standIn) installerURL(tag string) string { return s.srv.URL + "/installer/" + tag }

// run runs the installer made for tag as the console's line runs it, with
// args, which are install.sh's options.
func (s *standIn) run(tag, home string, args ...string) result {
	s.t.Helper()
	dir := filepath.Join(home, "bin")
	params := []string{"-Dir", psQuote(dir)}
	for _, a := range args {
		if p, ok := psParams[a]; ok {
			params = append(params, p)
		} else {
			params = append(params, psQuote(a))
		}
	}
	return s.powershell(home, dir, fmt.Sprintf("& ([scriptblock]::Create((irm %s))) %s", psQuote(s.installerURL(tag)), strings.Join(params, " ")))
}

// powershell runs command with powershell() for a user whose home is home,
// and reports on dir.
func (s *standIn) powershell(home, dir, command string, env ...string) result {
	s.t.Helper()
	cmd := exec.Command(powershell(), "-NoProfile", "-NonInteractive", "-Command", command)
	for _, e := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(e), "HANGAR_") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env,
		"LOCALAPPDATA="+filepath.Join(home, "AppData", "Local"),
		"HANGAR_CONFIG="+filepath.Join(home, "config", "config.yaml"),
		"HANGAR_INSTALL_API_URL="+s.srv.URL+"/api",
	)
	if !s.githubBase {
		cmd.Env = append(cmd.Env, "HANGAR_INSTALL_BASE_URL="+s.srv.URL)
	}
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			s.t.Fatal(err)
		}
		code = ee.ExitCode()
	}
	return result{code: code, stderr: string(out), dir: dir}
}

// Under iex install.ps1 runs in the caller's scope, and either way in the
// caller's process: what the caller had set is as it was afterwards.
func TestPowerShellLeavesTheCallerAlone(t *testing.T) {
	const (
		before = "$Version = 'caller-version'; $Dir = 'caller-dir'; $Endpoint = 'caller-endpoint'; " +
			"$ErrorActionPreference = 'Continue'; " +
			"[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls11; " +
			"$global:LASTEXITCODE = 7; "
		after = "; 'caller: ' + (($Version, $Dir, $Endpoint, $ErrorActionPreference, [Net.ServicePointManager]::SecurityProtocol, $LASTEXITCODE) -join '|')"
		want  = "caller: caller-version|caller-dir|caller-endpoint|Continue|Tls11|7"
	)
	for _, tc := range []struct {
		name, form string
	}{
		{"irm | iex", "irm %s | iex"},
		// With -Endpoint, so hangar.exe runs and sets $LASTEXITCODE.
		{"the scriptblock", "& ([scriptblock]::Create((irm %s))) -Dir $env:HANGAR_INSTALL_DIR -Endpoint https://api.example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStandIn(t, map[string]release{"v0.2.0": {}})
			home := t.TempDir()
			dir := filepath.Join(home, "bin")
			r := s.powershell(home, dir, before+fmt.Sprintf(tc.form, psQuote(s.installerURL("v0.2.0")))+after, "HANGAR_INSTALL_DIR="+dir)
			if r.code != 0 || installed(t, dir) != "v0.2.0" {
				t.Fatalf("exit %d, installed %q, want v0.2.0:\n%s", r.code, installed(t, dir), r.stderr)
			}
			if !strings.Contains(r.stderr, want) {
				t.Errorf("want %q after the install:\n%s", want, r.stderr)
			}
		})
	}
}

func TestPowerShellInstallsIntoLocalAppData(t *testing.T) {
	s := newStandIn(t, map[string]release{"v0.2.0": {}})
	home := t.TempDir()
	dir := filepath.Join(home, "AppData", "Local", "Programs", "hangar")
	r := s.powershell(home, dir, fmt.Sprintf("& ([scriptblock]::Create((irm %s)))", psQuote(s.installerURL("v0.2.0"))))
	if r.code != 0 || installed(t, dir) != "v0.2.0" {
		t.Fatalf("exit %d, installed %q in %s, want v0.2.0:\n%s", r.code, installed(t, dir), dir, r.stderr)
	}
}

func TestPowerShellAddsTheDirToThePathOnce(t *testing.T) {
	s := newStandIn(t, map[string]release{"v0.2.0": {}})
	home := t.TempDir()
	// By the directory itself, not its name: Windows PowerShell's
	// GetFullPath writes a TEMP of short 8.3 names, as a CI runner's is, in
	// long ones.
	count := func() int {
		want, err := os.Stat(filepath.Join(home, "bin"))
		if err != nil {
			t.Fatal(err)
		}
		v, _, _ := userPath(t)
		n := 0
		for _, e := range strings.Split(v, ";") {
			if e == "" {
				continue
			}
			if x, err := registry.ExpandString(e); err == nil {
				e = x
			}
			if fi, err := os.Stat(e); err == nil && os.SameFile(fi, want) {
				n++
			}
		}
		return n
	}
	r := s.run("v0.2.0", home)
	if r.code != 0 || !strings.Contains(r.stderr, "to your PATH") {
		t.Fatalf("exit %d, want the dir added to the PATH:\n%s", r.code, r.stderr)
	}
	if n := count(); n != 1 {
		t.Fatalf("the user's PATH holds the dir %d times after an install, want 1", n)
	}
	r = s.run("v0.2.0", home)
	if r.code != 0 || strings.Contains(r.stderr, "to your PATH") {
		t.Fatalf("second install: exit %d, want the PATH left:\n%s", r.code, r.stderr)
	}
	if n := count(); n != 1 {
		t.Errorf("the user's PATH holds the dir %d times after a second install, want 1", n)
	}
}

func TestPowerShellReplacesARunningHangar(t *testing.T) {
	s := newStandIn(t, map[string]release{"v0.2.0": {}, "v0.2.1": {}})
	home := t.TempDir()
	r := s.run("v0.2.0", home, "--version", "v0.2.0")
	if r.code != 0 {
		t.Fatalf("exit %d:\n%s", r.code, r.stderr)
	}
	hold := exec.Command(filepath.Join(r.dir, cliName), "hold")
	stdin, err := hold.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := hold.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		_ = hold.Wait()
	}()

	r = s.run("v0.2.0", home)
	if r.code != 0 || installed(t, r.dir) != "v0.2.1" {
		t.Fatalf("exit %d, installed %q, want v0.2.1 over the running v0.2.0:\n%s", r.code, installed(t, r.dir), r.stderr)
	}
}
