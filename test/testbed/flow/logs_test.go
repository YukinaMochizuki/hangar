//go:build testbed

package flow

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chvm"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/vmtest"
)

// chattyScript says three lines on its first boot, then bye on stderr,
// and exits 4, leaving a mark on its root disk; always restarts it into a
// boot that ticks once a second.
const chattyScript = `if [ ! -e /restarted ]; then
i=0; while [ $i -lt 3 ]; do i=$((i+1)); echo "first $i"; sleep 1; done
touch /restarted; sync; echo bye >&2; exit 4
fi
i=0; while :; do i=$((i+1)); echo "tick $i"; sleep 1; done`

// logsCLI runs hangar logs, returning its stdout and stderr.
func (c *cluster) logsCLI(args ...string) (string, string, error) {
	cmd := exec.Command(vmtest.Artifacts().CLI, append([]string{"logs"}, args...)...)
	cmd.Env = append(os.Environ(), "HANGAR_CONFIG="+filepath.Join(workDir, "cli.yaml"))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

var tickLine = regexp.MustCompile(`(?m)^tick (\d+)$`)

// lastTick is the highest tick in out, 0 if none.
func lastTick(out string) int {
	n := 0
	for _, m := range tickLine.FindAllStringSubmatch(out, -1) {
		if i, _ := strconv.Atoi(m[1]); i > n {
			n = i
		}
	}
	return n
}

// nodePID is the node agent's pid.
func (c *cluster) nodePID(t *testing.T) int {
	t.Helper()
	if installed {
		pid, _ := strconv.Atoi(serviceProps(t, nodeUnit, "MainPID")["MainPID"])
		return pid
	}
	return c.node.Process.Pid
}

// openLogs counts the files of the machine's console log the agent holds
// open.
func openLogs(t *testing.T, pid int, uid string) int {
	t.Helper()
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	fds, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, fd := range fds {
		target, _ := os.Readlink(filepath.Join(dir, fd.Name()))
		if strings.HasPrefix(target, filepath.Join(vmDir, uid, chvm.ConsoleLog)) {
			n++
		}
	}
	return n
}

// Scenario 9: hangar logs reads a container's output from the host, over
// the agent's tunnel, as one stream across a restart: stdout to stdout,
// stderr and the exit to stderr, and every option; logs -f sees output as
// it comes, and killing it ends the agent's read of the log.
func (f *flow) logs(t *testing.T) {
	c := f.c
	c.applyYAML(containerYAML("chatty", busyboxRef, "RESTART_POLICY_ALWAYS", "1Gi", chattyScript))
	var uid string
	eventually(t, 3*time.Minute, "chatty's machine on the host", func() (bool, string) {
		m, ok := get(c, "machine", "ct-chatty", &computev1alpha1.Machine{})
		uid = m.GetMetadata().GetUid()
		return ok && uid != "" && unitActive(uid), fmt.Sprintf("machine %v", m.GetMetadata())
	})
	f.uids["chatty"] = uid

	var out, errOut string
	eventually(t, 2*time.Minute, "both boots in hangar logs", func() (bool, string) {
		var err error
		out, errOut, err = c.logsCLI("ct", "chatty")
		return err == nil && strings.Contains(out, "first 1\nfirst 2\nfirst 3\n") && lastTick(out) >= 2 &&
				strings.Contains(errOut, "bye\nhangar: exited with code 4 (boot 1)\n"),
			fmt.Sprintf("stdout %q, stderr %q, %v", out, errOut, err)
	})
	if i, j := strings.Index(out, "first 3"), strings.Index(out, "tick 1"); i < 0 || j < i {
		t.Errorf("the boots are out of order:\n%s", out)
	}
	if strings.Contains(out, "bye") || strings.Contains(out, "hangar:") {
		t.Errorf("stderr and hangar's own lines on stdout:\n%s", out)
	}
	if nRestarts(t, uid) < 1 {
		t.Errorf("chatty restarted %d times", nRestarts(t, uid))
	}
	t.Logf("hangar logs ct chatty:\n--- stdout:\n%s--- stderr:\n%s", out, errOut)

	// -o json: one record a line, with its stream and boot.
	js, _, err := c.logsCLI("ct", "chatty", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var sawExit, sawBoot2 bool
	for _, line := range strings.Split(strings.TrimSpace(js), "\n") {
		var rec struct {
			Boot   int64
			Stream string
			Data   string
			Exit   *struct{ Code int32 }
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		sawExit = sawExit || rec.Stream == "exit" && rec.Boot == 1 && rec.Exit != nil && rec.Exit.Code == 4
		sawBoot2 = sawBoot2 || rec.Stream == "stdout" && rec.Boot == 2 && strings.HasPrefix(rec.Data, "tick ")
	}
	if !sawExit || !sawBoot2 {
		t.Errorf("-o json lacks boot 1's exit (%t) or boot 2's ticks (%t):\n%s", sawExit, sawBoot2, js)
	}

	tail, tailErr, err := c.logsCLI("ct", "chatty", "--tail", "1")
	if err != nil || !tickLine.MatchString(strings.TrimSpace(tail)) || strings.Count(tail, "\n") != 1 || tailErr != "" {
		t.Errorf("--tail 1: stdout %q, stderr %q, %v", tail, tailErr, err)
	}
	since, _, err := c.logsCLI("ct", "chatty", "--since", "3s")
	if err != nil || strings.Contains(since, "first") || strings.Count(since, "\n") > 5 {
		t.Errorf("--since 3s: %q %v", since, err)
	}
	stamped, _, err := c.logsCLI("ct", "chatty", "--tail", "2", "--timestamps")
	if err != nil || !regexp.MustCompile(`^(\d{4}-\d\d-\d\dT[0-9:.]+Z tick \d+\n){2}$`).MatchString(stamped) {
		t.Errorf("--timestamps: %q %v", stamped, err)
	}

	// -f: new ticks arrive as they are written, and killing the CLI ends
	// the agent's read of the log.
	pid := c.nodePID(t)
	before := openLogs(t, pid, uid)
	follow := exec.Command(vmtest.Artifacts().CLI, "logs", "-f", "ct", "chatty", "--tail", "1")
	follow.Env = append(os.Environ(), "HANGAR_CONFIG="+filepath.Join(workDir, "cli.yaml"))
	pipe, err := follow.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := follow.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	lines := make(chan string, 100)
	go func() {
		sc := bufio.NewScanner(pipe)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
		waited <- follow.Wait()
	}()
	var followed []string
	newest := lastTick(out)
	deadline := time.After(time.Minute)
	for seen := 0; seen < 3; {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatalf("logs -f ended after %q", followed)
			}
			followed = append(followed, l)
			if lastTick(l) > newest {
				seen++
			}
		case <-deadline:
			t.Fatalf("logs -f saw no new ticks past %d: %q", newest, followed)
		}
	}
	t.Logf("hangar logs -f ct chatty --tail 1 printed, live: %q", followed)
	if n := openLogs(t, pid, uid); n <= before {
		t.Errorf("the agent holds %d files of chatty's log while following, %d before", n, before)
	}
	_ = follow.Process.Signal(syscall.SIGKILL)
	<-waited
	eventually(t, 30*time.Second, "the agent's read of the log ended with the killed CLI", func() (bool, string) {
		n := openLogs(t, pid, uid)
		return n <= before, fmt.Sprintf("%d files of the log open", n)
	})

	c.cli("delete", "container", "chatty")
	c.cli("wait", "container", "chatty", "--for=delete", "--timeout=2m")
}
