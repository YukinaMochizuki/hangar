//go:build testbed

package vms

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"
	"google.golang.org/protobuf/encoding/protodelim"

	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chvm"
)

// consoleClient is a client of a VM's serial console, with a terminal of
// its own that it draws what it is sent on.
type consoleClient struct {
	t    *testing.T
	conn net.Conn

	mu     sync.Mutex
	term   *vt.Emulator
	raw    bytes.Buffer
	states []*nodev1.ConsoleState
	err    error
}

func attachConsole(t *testing.T, hv *chvm.Hypervisor, uid string, start *nodev1.ConsoleStart) *consoleClient {
	t.Helper()
	conn, err := hv.AttachConsole(context.Background(), uid)
	if err != nil {
		t.Fatalf("attaching to %s's console: %v", uid, err)
	}
	c := &consoleClient{t: t, conn: conn, term: vt.NewEmulator(80, 24)}
	go func() { _, _ = io.Copy(io.Discard, c.term) }()
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := protodelim.MarshalTo(conn, &nodev1.ConsoleRequest{Message: &nodev1.ConsoleRequest_Start{Start: start}}); err != nil {
		t.Fatal(err)
	}
	go c.read()
	return c
}

func (c *consoleClient) read() {
	r := bufio.NewReader(c.conn)
	for {
		res := &nodev1.ConsoleResponse{}
		err := protodelim.UnmarshalOptions{MaxSize: 1 << 20}.UnmarshalFrom(r, res)
		c.mu.Lock()
		if err != nil {
			c.err = err
			c.mu.Unlock()
			return
		}
		if st := res.GetState(); st != nil {
			c.states = append(c.states, st)
			c.term.Resize(int(st.GetSize().GetColumns()), int(st.GetSize().GetRows()))
		} else {
			c.raw.Write(res.GetOutput())
			_, _ = c.term.Write(res.GetOutput())
		}
		c.mu.Unlock()
	}
}

func (c *consoleClient) typeIn(s string) {
	c.t.Helper()
	if _, err := protodelim.MarshalTo(c.conn, &nodev1.ConsoleRequest{Message: &nodev1.ConsoleRequest_Input{Input: []byte(s)}}); err != nil {
		c.t.Fatal(err)
	}
}

// screen is the client's terminal as lines of text.
func (c *consoleClient) screen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var lines []string
	for y := range c.term.Height() {
		var b strings.Builder
		for x := range c.term.Width() {
			if cell := c.term.CellAt(x, y); cell != nil && cell.Content != "" {
				b.WriteString(cell.Content)
			} else if cell == nil || cell.Width != 0 {
				b.WriteByte(' ')
			}
		}
		lines = append(lines, strings.TrimRight(b.String(), " "))
	}
	return lines
}

// rawSince is what the client was sent past offset from.
func (c *consoleClient) rawSince(from int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.raw.String()[min(from, c.raw.Len()):]
}

func (c *consoleClient) rawLen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.raw.Len()
}

func (c *consoleClient) lastState() *nodev1.ConsoleState {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.states) == 0 {
		return nil
	}
	return c.states[len(c.states)-1]
}

// waitRaw waits, for up to d, until what the client was sent past from
// holds want, and returns where it ends.
func (c *consoleClient) waitRaw(from int, want string, d time.Duration) int {
	c.t.Helper()
	deadline := time.Now().Add(d)
	for {
		got := c.rawSince(from)
		if i := strings.Index(got, want); i >= 0 {
			return from + i + len(want)
		}
		c.mu.Lock()
		err := c.err
		c.mu.Unlock()
		if err != nil || time.Now().After(deadline) {
			c.t.Fatalf("the console never sent %q (%v); its end:\n%s", want, err, tail(got))
		}
		time.Sleep(100 * time.Millisecond) //nolint:forbidigo // a guest at serial speed
	}
}

// waitScreen waits, for up to d, until the client's terminal shows want.
func (c *consoleClient) waitScreen(want string, d time.Duration) {
	c.t.Helper()
	deadline := time.Now().Add(d)
	for !strings.Contains(strings.Join(c.screen(), "\n"), want) {
		if time.Now().After(deadline) {
			c.t.Fatalf("the screen never showed %q:\n%s", want, strings.Join(c.screen(), "\n"))
		}
		time.Sleep(100 * time.Millisecond) //nolint:forbidigo // a guest at serial speed
	}
}

// consoleHelperPID is the pid of the machine's console helper.
func consoleHelperPID(t *testing.T, uid string) int {
	t.Helper()
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cmd, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		args := strings.Split(string(cmd), "\x00")
		if len(args) > 2 && args[1] == "vmm-console" && strings.Contains(string(cmd), "/"+uid+"/") {
			return pid
		}
	}
	t.Fatalf("no console helper for %s", uid)
	return 0
}

// checkHelperConfined checks the machine's console helper holds nothing
// but sockets, pipes and what the Go runtime opens (its epoll, and its
// cgroup's cpu.max, which it reads its CPUs from), and runs in the VM's
// unit; and that the console socket is root's alone, which the VM's user,
// Cloud Hypervisor's, cannot connect to.
func checkHelperConfined(t *testing.T, uid, user string) {
	t.Helper()
	pid := consoleHelperPID(t, uid)
	fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		t.Fatal(err)
	}
	var held []string
	for _, fd := range fds {
		link, _ := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, fd.Name()))
		held = append(held, fd.Name()+"="+link)
		switch {
		case strings.HasPrefix(link, "socket:"), strings.HasPrefix(link, "pipe:"), strings.HasPrefix(link, "anon_inode:"), link == "/dev/null",
			link == "/sys/fs/cgroup/hangar.slice/hangar-vm-"+uid+".service/cpu.max":
		default:
			t.Errorf("the console helper holds fd %s, %s", fd.Name(), link)
		}
	}
	t.Logf("the console helper's fds: %s", strings.Join(held, " "))
	cg, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil || !strings.Contains(string(cg), "hangar-vm-"+uid+".service") {
		t.Errorf("the console helper's cgroup %q is not its VM's unit's (%v)", cg, err)
	}

	sock := filepath.Join(vmDir, uid, "console.sock")
	var sst syscall.Stat_t
	if err := syscall.Lstat(sock, &sst); err != nil {
		t.Fatal(err)
	}
	if sst.Mode&syscall.S_IFMT != syscall.S_IFSOCK || sst.Mode&0o7777 != 0o600 || sst.Uid != 0 {
		t.Errorf("the console socket: mode %o, uid %d; want a socket, 0600, root's", sst.Mode, sst.Uid)
	}
	out, err := exec.Command("setpriv", "--reuid", user, "--regid", user, "--clear-groups", "--",
		"python3", "-c", "import socket,sys; s=socket.socket(socket.AF_UNIX); s.connect(sys.argv[1])", sock).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "PermissionError") {
		t.Errorf("the VM's user connecting to the console socket: %v, %s; want it refused", err, out)
	}
}

// TestAVMsSerialConsoleIsInteractive: an Ubuntu VM's serial console,
// attached to by two clients, is typed at by the first: it logs in and
// runs less, whose screen the second, attaching then, is drawn exactly as
// the first sees it; the second's typing reaches nothing, though it is
// sent what comes. The console's helper runs as the VM's user, with no
// capability. The guest rebooting, on its own, the clients stay attached,
// are told the screen's size again, and see the new boot; the log holds
// both boots.
func TestAVMsSerialConsoleIsInteractive(t *testing.T) {
	const uid, password = "consolevm", "hangar-l4-console"
	_, hv := bootWithSeed(t, uid, "echo 'ubuntu:"+password+"' | chpasswd",
		func(_ *host, s *driver.VMSpec) { s.SystemSerialNumber = uid })

	st := statusOf(t, consoleHelperPID(t, uid))
	user := fmt.Sprint(vmUserOf(t, uid))
	for k, v := range map[string]string{
		"Uid": strings.Repeat(user+" ", 3) + user, "Gid": strings.Repeat(user+" ", 3) + user,
		"CapInh": "0000000000000000", "CapPrm": "0000000000000000", "CapEff": "0000000000000000",
		"CapBnd": "0000000000000000", "CapAmb": "0000000000000000", "NoNewPrivs": "1", "Groups": "",
	} {
		if st[k] != v {
			t.Errorf("the console helper's %s is %q, want %q", k, st[k], v)
		}
	}
	t.Logf("the console helper: Uid %s, CapEff %s, CapBnd %s, NoNewPrivs %s, Seccomp %s", st["Uid"], st["CapEff"], st["CapBnd"], st["NoNewPrivs"], st["Seccomp"])
	checkHelperConfined(t, uid, user)

	a := attachConsole(t, hv, uid, &nodev1.ConsoleStart{})
	at := a.waitRaw(0, "\x1b[", time.Minute) // the drawing
	if st := a.lastState(); !st.GetInput() || st.GetSize().GetColumns() != 80 || st.GetSize().GetRows() != 24 {
		t.Fatalf("the first client is told %v, want the input at 80x24", st)
	}
	a.typeIn("\r")
	at = a.waitRaw(at, "login: ", 2*time.Minute)
	a.typeIn("ubuntu\r")
	at = a.waitRaw(at, "Password: ", time.Minute)
	a.typeIn(password + "\r")
	at = a.waitRaw(at, "$ ", 2*time.Minute)
	a.typeIn("stty rows 24 cols 80; TERM=xterm-256color less /etc/passwd\r")
	a.waitScreen("root:x:0:0", 2*time.Minute)

	b := attachConsole(t, hv, uid, &nodev1.ConsoleStart{})
	b.waitRaw(0, "root:x:0:0", time.Minute)
	if st := b.lastState(); st.GetInput() {
		t.Error("the second client holds the input")
	}
	// The guest draws at serial speed: once it is done, the second client,
	// drawn the screen and sent what came after, sees what the first does.
	deadline := time.Now().Add(time.Minute)
	for {
		as, bs := strings.Join(a.screen(), "\n"), strings.Join(b.screen(), "\n")
		if as == bs {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the second client sees\n%s\nwhere the first sees\n%s", bs, as)
		}
		time.Sleep(time.Second) //nolint:forbidigo // a guest at serial speed
	}
	// q would end less; it reaches nothing. G, from the first, takes less
	// to its end, which both see.
	b.typeIn("q")
	a.typeIn("G")
	a.waitScreen("(END)", time.Minute)
	b.waitScreen("(END)", time.Minute)

	a.typeIn("q")
	at = a.waitRaw(at, "$ ", time.Minute)
	bAt := b.rawLen()
	a.typeIn("sudo reboot\r")
	a.waitRaw(at, "Linux version", 5*time.Minute)
	b.waitRaw(bAt, "Linux version", time.Minute)
	b.waitRaw(bAt, "login: ", 5*time.Minute)
	for name, c := range map[string]*consoleClient{"first": a, "second": b} {
		c.mu.Lock()
		n, err := len(c.states), c.err
		c.mu.Unlock()
		if n < 2 || err != nil {
			t.Errorf("after the reboot the %s client was told %d states, its connection %v", name, n, err)
		}
	}
	if n := strings.Count(serialOf(t, uid), "login: "); n < 2 {
		t.Errorf("the log holds %d login prompts, want both boots'", n)
	}
}
