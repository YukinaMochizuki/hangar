//go:build testbed

package flow

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/vmtest"
)

// shellYAML is a busybox container that idles, with an environment of its
// own for its execs to inherit.
var shellYAML = `apiVersion: compute.hangar.dev/v1alpha1
kind: Container
metadata: {name: shell}
spec:
  image: ` + busyboxRef + `
  command: ["/bin/sh", "-c", ` + fmt.Sprintf("%q", idleScript) + `]
  env: [{name: GREETING, value: hello}]
  resources: {cpu: 1, memory: 256Mi, disk: 1Gi}
`

// execCLI runs hangar exec with stdin, returning its stdout, its stderr and
// its exit code.
func execCLI(t *testing.T, stdin io.Reader, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(vmtest.Artifacts().CLI, append([]string{"exec"}, args...)...)
	cmd.Env = append(os.Environ(), "HANGAR_CONFIG="+filepath.Join(workDir, "cli.yaml"))
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		return stdout.String(), stderr.String(), ee.ExitCode()
	case err != nil:
		t.Fatal(err)
	}
	return stdout.String(), stderr.String(), 0
}

// onTerminal runs hangar exec on a terminal of its own, of rows and cols,
// as its controlling terminal; the terminal's other side is returned with
// what it printed so far, and the CLI's exit code once it is done.
type onTerminal struct {
	cmd  *exec.Cmd
	ptmx *os.File
	tty  *os.File
	// before is the terminal's settings before hangar ran.
	before *unix.Termios
	mu     sync.Mutex
	out    bytes.Buffer
	done   chan struct{}
	code   int
}

func execOnTerminal(t *testing.T, rows, cols uint16, args ...string) *onTerminal {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := pty.Setsize(ptmx, &pty.Winsize{Rows: rows, Cols: cols}); err != nil {
		t.Fatal(err)
	}
	before, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	o := &onTerminal{ptmx: ptmx, tty: tty, before: before, done: make(chan struct{})}
	o.cmd = exec.Command(vmtest.Artifacts().CLI, append([]string{"exec"}, args...)...)
	o.cmd.Env = append(os.Environ(), "HANGAR_CONFIG="+filepath.Join(workDir, "cli.yaml"))
	o.cmd.Stdin, o.cmd.Stdout, o.cmd.Stderr = tty, tty, tty
	o.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := o.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			o.mu.Lock()
			o.out.Write(buf[:n])
			o.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	go func() {
		err := o.cmd.Wait()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			o.code = ee.ExitCode()
		}
		close(o.done)
	}()
	t.Cleanup(func() {
		_ = o.cmd.Process.Kill()
		<-o.done
		_ = ptmx.Close()
		_ = tty.Close()
	})
	return o
}

func (o *onTerminal) printed() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.out.String()
}

// waitFor waits until the terminal shows s.
func (o *onTerminal) waitFor(t *testing.T, s string) {
	t.Helper()
	eventually(t, 30*time.Second, fmt.Sprintf("%q on the terminal", s), func() (bool, string) {
		return strings.Contains(o.printed(), s), o.printed()
	})
}

// exited waits for the CLI to exit and returns its exit code.
func (o *onTerminal) exited(t *testing.T) int {
	t.Helper()
	select {
	case <-o.done:
		return o.code
	case <-time.After(30 * time.Second):
		t.Fatalf("hangar exec did not exit; the terminal shows %q", o.printed())
		return 0
	}
}

// guestRuns counts the processes in the guest whose command line holds
// marker, which an argument may hold whole, as /proc separates them with
// NULs.
func guestRuns(t *testing.T, uid, marker string) string {
	t.Helper()
	// [x]yz matches xyz, not this command's own line.
	pattern := "[" + marker[:1] + "]" + marker[1:]
	return strings.TrimSpace(mustExec(t, uid, "grep -l '"+pattern+"' /proc/[0-9]*/cmdline 2>/dev/null | wc -l"))
}

// Scenario 11: hangar exec runs commands in a running busybox container,
// through the apiserver and the agent's tunnel: with the container's
// environment, user and directory unless overridden; stdout and stderr
// apart, and the exit code; stdin piped with -i; on a terminal with -t, of
// the local terminal's size, resized as it is, with Ctrl-C a key, and the
// terminal put back; Ctrl-C passed on without -t; and a CLI killed leaves
// nothing running in the guest. Each exec is in the apiserver's log.
func (f *flow) exec(t *testing.T) {
	c := f.c
	c.applyYAML(shellYAML)
	c.waitReady("shell", 3*time.Minute)
	uid := c.machine("shell").GetMetadata().GetUid()
	f.uids["shell"] = uid

	out, errOut, code := execCLI(t, nil, "ct", "shell", "--", "sh", "-c", `echo "$GREETING"; id -u; pwd`)
	if code != 0 || out != "hello\n0\n/\n" || errOut != "" {
		t.Errorf("the container's environment: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	out, errOut, code = execCLI(t, nil, "--env", "GREETING=bye", "-u", "65534", "-w", "/tmp", "ct", "shell", "--", "sh", "-c", `echo "$GREETING"; id -u; pwd`)
	if code != 0 || out != "bye\n65534\n/tmp\n" {
		t.Errorf("overridden: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	out, errOut, code = execCLI(t, nil, "ct", "shell", "--", "sh", "-c", "echo out; echo err >&2; exit 3")
	if code != 3 || out != "out\n" || errOut != "err\n" {
		t.Errorf("a failing command: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	out, errOut, code = execCLI(t, nil, "ct", "shell", "--", "no-such-program")
	if code != 127 {
		t.Errorf("a program not found: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	t.Logf("hangar exec ct shell -- no-such-program: exit %d, stderr %q", code, errOut)
	// Without -i the command reads no stdin, whatever hangar's is.
	out, _, code = execCLI(t, strings.NewReader("ignored\n"), "ct", "shell", "--", "cat")
	if code != 0 || out != "" {
		t.Errorf("cat without -i: exit %d, stdout %q", code, out)
	}
	out, _, code = execCLI(t, strings.NewReader("one\ntwo\nthree\n"), "-i", "ct", "shell", "--", "sh", "-c", "wc -l; echo done")
	if code != 0 || strings.Fields(out)[0] != "3" || !strings.HasSuffix(out, "done\n") {
		t.Errorf("stdin piped with -i: exit %d, stdout %q", code, out)
	}
	if _, errOut, code := execCLI(t, nil, "ct", "absent", "--", "true"); code != 125 || !strings.Contains(errOut, "not found") {
		t.Errorf("a container that is not there: exit %d, stderr %q", code, errOut)
	}

	// On a terminal: the local one's size, a resize, Ctrl-C as a key.
	term := execOnTerminal(t, 40, 100, "-it", "ct", "shell", "--", "sh", "-c",
		`stty size; trap 'echo trapped; exit 9' INT; read -r line; stty size; echo ready; while :; do sleep 1; done`)
	term.waitFor(t, "40 100")
	if err := pty.Setsize(term.ptmx, &pty.Winsize{Rows: 50, Cols: 120}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second) // for SIGWINCH to reach the guest before the line
	if _, err := term.ptmx.Write([]byte("\r")); err != nil {
		t.Fatal(err)
	}
	term.waitFor(t, "50 120")
	term.waitFor(t, "ready")
	if _, err := term.ptmx.Write([]byte{3}); err != nil { // Ctrl-C
		t.Fatal(err)
	}
	if code := term.exited(t); code != 9 || !strings.Contains(term.printed(), "trapped") {
		t.Errorf("Ctrl-C on a terminal: exit %d, the terminal shows %q", code, term.printed())
	}
	after, err := unix.IoctlGetTermios(int(term.tty.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	if *after != *term.before {
		t.Errorf("the terminal was left %+v, was %+v", *after, *term.before)
	}
	t.Logf("hangar exec -it on a 40x100 terminal, resized to 50x120, then Ctrl-C:\n%s", term.printed())

	// Without -t, SIGINT goes to the command.
	sigint := exec.Command(vmtest.Artifacts().CLI, "exec", "ct", "shell", "--", "sh", "-c",
		`trap 'echo interrupted; exit 7' INT; echo started; while :; do sleep 1; done`)
	sigint.Env = append(os.Environ(), "HANGAR_CONFIG="+filepath.Join(workDir, "cli.yaml"))
	var sigOut syncBuffer
	sigint.Stdout = &sigOut
	if err := sigint.Start(); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "the command started", func() (bool, string) {
		return strings.Contains(sigOut.String(), "started"), sigOut.String()
	})
	_ = sigint.Process.Signal(syscall.SIGINT)
	err = sigint.Wait()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 || !strings.Contains(sigOut.String(), "interrupted") {
		t.Errorf("SIGINT without -t: %v, stdout %q", err, sigOut.String())
	}

	// A CLI killed outright leaves nothing running in the guest, the
	// command's own child included.
	killed := exec.Command(vmtest.Artifacts().CLI, "exec", "ct", "shell", "--", "sh", "-c", "sleep 4321 & sleep 4321; wait")
	killed.Env = append(os.Environ(), "HANGAR_CONFIG="+filepath.Join(workDir, "cli.yaml"))
	if err := killed.Start(); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "the command running in the guest", func() (bool, string) {
		n := guestRuns(t, uid, "4321")
		return n == "3", n // sh -c, and its two sleeps
	})
	_ = killed.Process.Signal(syscall.SIGKILL)
	_ = killed.Wait()
	// Within a ping's interval: the close reaches the guest, not a ping.
	eventually(t, 10*time.Second, "nothing of the killed CLI's command left in the guest", func() (bool, string) {
		n := guestRuns(t, uid, "4321")
		return n == "0", n
	})

	b, err := os.ReadFile(filepath.Join(workDir, "apiserver.log"))
	if err != nil && !installed {
		t.Fatal(err)
	}
	audit := string(b)
	if installed {
		audit = mustRun(t, "journalctl", "-u", apiUnit, "--since", "-10min", "--no-pager")
	}
	if !strings.Contains(audit, `msg="exec started"`) || !strings.Contains(audit, `msg="exec ended"`) ||
		!strings.Contains(audit, `msg="exec refused"`) || !strings.Contains(audit, "argv=") || strings.Contains(audit, "three") {
		t.Errorf("the apiserver's log of execs:\n%s", audit)
	}

	c.cli("delete", "container", "shell")
	c.cli("wait", "container", "shell", "--for=delete", "--timeout=2m")
	if _, ok := get(c, "machine", "ct-shell", &computev1alpha1.Machine{}); ok {
		t.Error("shell's machine outlived it")
	}
}

// syncBuffer is a bytes.Buffer a process writes while the test reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
