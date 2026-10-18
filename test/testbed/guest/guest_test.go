//go:build testbed

package guest

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/test/testbed/internal/vmtest"
)

func process(args ...string) *guestv1.BootConfig {
	return &guestv1.BootConfig{Process: &guestv1.Process{Args: args}}
}

// segment is a run of output on one stream.
type segment struct {
	stream console.Stream
	text   string
}

// outputAndExit splits a guest's console into its process's output, merged
// into runs per stream, and the exit record, which must come last.
func outputAndExit(t *testing.T, v *vmtest.VM) ([]segment, console.ExitStatus) {
	t.Helper()
	recs, err := v.Console()
	if err != nil {
		t.Fatalf("reading the console: %v", err)
	}
	if len(recs) == 0 {
		t.Fatal("nothing on the console")
	}
	last := recs[len(recs)-1]
	exit, err := console.ExitOf(last)
	if err != nil {
		t.Fatalf("the last record: %v", err)
	}
	var segs []segment
	for _, r := range recs[:len(recs)-1] {
		switch r.Stream {
		case console.Stdout, console.Stderr:
		case console.Exit:
			t.Errorf("an exit record before the last one")
			continue
		default:
			continue
		}
		if n := len(segs); n > 0 && segs[n-1].stream == r.Stream {
			segs[n-1].text += string(r.Payload)
		} else {
			segs = append(segs, segment{r.Stream, string(r.Payload)})
		}
	}
	return segs, exit
}

func initMessages(t *testing.T, v *vmtest.VM) string {
	recs, _ := v.Console()
	var b strings.Builder
	for _, r := range recs {
		if r.Stream == console.Init {
			b.Write(r.Payload)
		}
	}
	return b.String()
}

func TestMainProcessOutputAndExitCode(t *testing.T) {
	v := vmtest.New(t, process("sh", "-c",
		`echo out1; sleep 0.2; echo err1 >&2; sleep 0.2; printf 'two\nlines\r\n\0end\n'; exit 3`))
	v.Start()
	v.WaitPowerOff(60 * time.Second)

	segs, exit := outputAndExit(t, v)
	want := []segment{
		{console.Stdout, "out1\n"},
		{console.Stderr, "err1\n"},
		{console.Stdout, "two\nlines\r\n\x00end\n"},
	}
	if !slices.Equal(segs, want) {
		t.Errorf("output = %q\nwant %q", segs, want)
	}
	if exit != (console.ExitStatus{Code: 3}) {
		t.Errorf("exit = %+v, want code 3", exit)
	}
	v.CheckCleanDisk(t)
}

// Many newlines on the real console, a terminal: every record must read back
// whole, with the exit record last.
func TestConsoleFramingManyLines(t *testing.T) {
	v := vmtest.New(t, process("sh", "-c", `seq 1 1000; sleep 0.2; seq 1001 2000 >&2`))
	v.Start()
	v.WaitPowerOff(60 * time.Second)

	segs, exit := outputAndExit(t, v) // fails on any skipped bytes
	seq := func(from, to int) string {
		var b strings.Builder
		for i := from; i <= to; i++ {
			fmt.Fprintf(&b, "%d\n", i)
		}
		return b.String()
	}
	want := []segment{{console.Stdout, seq(1, 1000)}, {console.Stderr, seq(1001, 2000)}}
	if !slices.Equal(segs, want) {
		t.Errorf("output differs: got %d segments, want 2 runs of 1000 lines", len(segs))
	}
	if exit != (console.ExitStatus{}) {
		t.Errorf("exit = %+v, want code 0", exit)
	}
}

// A writer left running in the background is stopped before the power-off,
// and the root filesystem is left clean all the same.
func TestPowerOffLeavesTheDiskClean(t *testing.T) {
	v := vmtest.New(t, process("sh", "-c",
		`(while :; do echo data >> /var/tmp/writer; done) & sleep 0.5; echo started`))
	v.Start()
	v.WaitPowerOff(60 * time.Second)

	if _, exit := outputAndExit(t, v); exit != (console.ExitStatus{}) {
		t.Errorf("exit = %+v, want code 0", exit)
	}
	v.CheckCleanDisk(t)
}

func TestMainProcessKilledBySignal(t *testing.T) {
	v := vmtest.New(t, process("sh", "-c", `echo before; kill -KILL $$`))
	v.Start()
	v.WaitPowerOff(60 * time.Second)

	segs, exit := outputAndExit(t, v)
	if want := []segment{{console.Stdout, "before\n"}}; !slices.Equal(segs, want) {
		t.Errorf("output = %q, want %q", segs, want)
	}
	if exit != (console.ExitStatus{Code: 137, Signal: 9}) {
		t.Errorf("exit = %+v, want code 137, signal 9", exit)
	}
	v.CheckCleanDisk(t)
}

func TestMainProcessCannotStart(t *testing.T) {
	for _, tt := range []struct {
		program string
		code    int32
	}{
		{"no-such-program", 127},
		{"/no/such/program", 127},
		{"/etc/passwd", 126}, // not executable
	} {
		t.Run(tt.program, func(t *testing.T) {
			v := vmtest.New(t, process(tt.program, "arg"))
			v.Start()
			v.WaitPowerOff(60 * time.Second)

			segs, exit := outputAndExit(t, v)
			if len(segs) != 0 {
				t.Errorf("output = %q, want none", segs)
			}
			if exit != (console.ExitStatus{Code: tt.code}) {
				t.Errorf("exit = %+v, want code %d", exit, tt.code)
			}
			if msg := initMessages(t, v); !strings.Contains(msg, tt.program) {
				t.Errorf("hangar-init does not say what could not start: %q", msg)
			}
			v.CheckCleanDisk(t)
		})
	}
}

// TestGuestServices boots one long-running guest and checks what the
// platform set up in it and what the agent does.
func TestGuestServices(t *testing.T) {
	cfg := process("sleep", "3600")
	cfg.Hostname = "tb-guest"
	cfg.Nameservers = []string{"192.0.2.53", "198.51.100.53"}
	cfg.Search = []string{"example.test"}
	v := vmtest.New(t, cfg)
	mac, addr, gw := v.Net()
	v.Start()
	t.Logf("time to Ping: %s", v.Ready(30*time.Second))

	t.Run("Ping", func(t *testing.T) {
		c, done := v.Guest.Client("")
		defer done()
		resp, err := c.Ping(context.Background(), connect.NewRequest(&guestv1.PingRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if ver := resp.Msg.GetVersion(); ver == "" || ver == "(devel)" {
			t.Errorf("version %q is no revision", ver)
		}
	})

	t.Run("Network", func(t *testing.T) {
		links := v.MustExec(t, "ip", "-o", "link")
		var name string
		for _, l := range strings.Split(links, "\n") {
			if strings.Contains(l, "link/ether "+mac) {
				name = strings.TrimSpace(strings.Split(l, ":")[1])
				if !strings.Contains(l, ",UP") && !strings.Contains(l, "<UP") {
					t.Errorf("interface %s is not up: %s", mac, l)
				}
			}
		}
		if name == "" {
			t.Fatalf("no interface has MAC %s:\n%s", mac, links)
		}
		if out := v.MustExec(t, "ip", "-o", "-4", "addr", "show", "dev", name); !strings.Contains(out, " "+addr+" ") {
			t.Errorf("%s has no %s: %s", name, addr, out)
		}
		if out := v.MustExec(t, "ip", "route"); !strings.Contains(out, "default via "+gw+" dev "+name) {
			t.Errorf("no default route via %s: %s", gw, out)
		}
		v.MustExec(t, "ping", "-c", "1", "-W", "2", gw)
	})

	// The agent changes the interface's public IPv6 and default route while
	// the process runs, as when an Address moves to or from it.
	t.Run("SetIPv6", func(t *testing.T) {
		c, done := v.Guest.Client("")
		defer done()
		set := func(addr, gw string) error {
			_, err := c.SetIPv6(context.Background(), connect.NewRequest(&guestv1.SetIPv6Request{
				Interfaces: []*guestv1.Interface{{Mac: mac, Ipv6: addr, Ipv6Gateway: gw}},
			}))
			return err
		}
		holds := func(what, addr, gw string) {
			t.Helper()
			addrs := v.MustExec(t, "ip", "-6", "addr", "show")
			routes := v.MustExec(t, "ip", "-6", "route", "show", "default")
			n := strings.Count(addrs, "scope global")
			switch {
			case addr == "" && n != 0, addr != "" && (n != 1 || !strings.Contains(addrs, "inet6 "+addr+" scope global")):
				t.Errorf("%s: the guest holds %q, want %q", what, addrs, addr)
			case gw == "" && strings.TrimSpace(routes) != "", gw != "" && !strings.Contains(routes, "default via "+gw):
				t.Errorf("%s: the guest's default routes are %q, want one via %q", what, routes, gw)
			}
		}
		holds("booted without one", "", "")
		if err := set("2001:db8::a/128", "fe80::1"); err != nil {
			t.Fatal(err)
		}
		holds("given one", "2001:db8::a/128", "fe80::1")
		if err := set("2001:db8::b/128", "fe80::1"); err != nil {
			t.Fatal(err)
		}
		holds("given another", "2001:db8::b/128", "fe80::1")
		for _, bad := range [][2]string{{"2001:db8::c/64", ""}, {"192.0.2.1/32", ""}, {"2001:db8::c/128", "192.0.2.1"}} {
			if err := set(bad[0], bad[1]); connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("%s via %q: %v, want InvalidArgument", bad[0], bad[1], err)
			}
		}
		holds("refused what is no /128", "2001:db8::b/128", "fe80::1")
		if err := set("", ""); err != nil {
			t.Fatal(err)
		}
		holds("told it has none", "", "")
	})

	t.Run("Identity", func(t *testing.T) {
		ip := strings.Split(addr, "/")[0]
		if got := v.MustExec(t, "hostname"); got != "tb-guest\n" {
			t.Errorf("hostname = %q", got)
		}
		if got := v.MustExec(t, "cat", "/etc/hostname"); got != "tb-guest\n" {
			t.Errorf("/etc/hostname = %q", got)
		}
		if got := v.MustExec(t, "cat", "/etc/hosts"); !strings.Contains(got, "127.0.0.1\tlocalhost\n") || !strings.Contains(got, ip+"\ttb-guest\n") {
			t.Errorf("/etc/hosts = %q", got)
		}
		want := "nameserver 192.0.2.53\nnameserver 198.51.100.53\nsearch example.test\n"
		if got := v.MustExec(t, "cat", "/etc/resolv.conf"); got != want {
			t.Errorf("/etc/resolv.conf = %q, want %q", got, want)
		}
	})

	// With the mitigations built in, the kernel reports none of the CPU's
	// known flaws as unmitigated (docs/decisions/guest-kernel.md).
	t.Run("CPUVulnerabilities", func(t *testing.T) {
		out := v.MustExec(t, "grep", "-r", ".", "/sys/devices/system/cpu/vulnerabilities/")
		t.Logf("guest CPU vulnerabilities:\n%s", out)
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			if strings.Contains(l, "Vulnerable") {
				t.Errorf("unmitigated: %s", strings.TrimPrefix(l, "/sys/devices/system/cpu/vulnerabilities/"))
			}
		}
	})

	ctx := func(t *testing.T) context.Context {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		t.Cleanup(cancel)
		return c
	}

	t.Run("ExecCommand", func(t *testing.T) {
		res, err := v.Exec(ctx(t), []string{"sh", "-c", "echo o; echo e >&2; exit 5"}, vmtest.ExecOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if string(res.Stdout) != "o\n" || string(res.Stderr) != "e\n" || res.Exit.GetCode() != 5 || res.Exit.GetSignal() != 0 {
			t.Errorf("got stdout %q stderr %q exit %v", res.Stdout, res.Stderr, res.Exit)
		}
	})

	t.Run("ExecStdin", func(t *testing.T) {
		in := "hello\nworld\n"
		res, err := v.Exec(ctx(t), []string{"cat"}, vmtest.ExecOpts{Stdin: []byte(in), CloseStdin: true})
		if err != nil {
			t.Fatal(err)
		}
		if string(res.Stdout) != in || res.Exit.GetCode() != 0 {
			t.Errorf("got stdout %q exit %v", res.Stdout, res.Exit)
		}
	})

	t.Run("ExecTTY", func(t *testing.T) {
		res, err := v.Exec(ctx(t), []string{"sh", "-c", "tty; stty size"}, vmtest.ExecOpts{
			TTY: true, Size: &guestv1.WindowSize{Rows: 40, Cols: 120},
		})
		if err != nil {
			t.Fatal(err)
		}
		out := string(res.Stdout)
		if !strings.Contains(out, "/dev/pts/") || !strings.Contains(out, "40 120") || res.Exit.GetCode() != 0 {
			t.Errorf("got stdout %q exit %v", out, res.Exit)
		}
	})

	t.Run("ExecSignal", func(t *testing.T) {
		res, err := v.Exec(ctx(t), []string{"sh", "-c", "echo ready; exec sleep 100"}, vmtest.ExecOpts{Signal: 15})
		if err != nil {
			t.Fatal(err)
		}
		if res.Exit.GetCode() != 143 || res.Exit.GetSignal() != 15 {
			t.Errorf("exit = %v, want code 143, signal 15", res.Exit)
		}
	})

	t.Run("ExecCannotRun", func(t *testing.T) {
		for _, tt := range []struct {
			program string
			code    int32
		}{
			{"no-such-program", 127},
			{"/etc/passwd", 126}, // not executable
		} {
			res, err := v.Exec(ctx(t), []string{tt.program}, vmtest.ExecOpts{})
			if err != nil {
				t.Fatalf("%s: %v", tt.program, err)
			}
			if res.Exit.GetCode() != tt.code || !strings.Contains(string(res.Stderr), tt.program) {
				t.Errorf("%s: exit %v, stderr %q; want code %d and a reason", tt.program, res.Exit, res.Stderr, tt.code)
			}
		}
	})

	t.Run("ExecAfterLoad", func(t *testing.T) {
		const want = "0::/workload\n"
		if got := v.MustExec(t, "cat", "/proc/self/cgroup"); got != want {
			t.Fatalf("an exec'd process is in %q, want %q", got, want)
		}
		// Load inside the guest, and 32 MB through the agent itself, so that
		// hangar-init allocates and collects garbage before the next Exec.
		v.MustExec(t, "sh", "-c", "set -o pipefail; head -c 52428800 /dev/urandom | sha256sum")
		res, err := v.Exec(ctx(t), []string{"head", "-c", fmt.Sprint(32 << 20), "/dev/urandom"}, vmtest.ExecOpts{})
		if err != nil || res.Exit.GetCode() != 0 || len(res.Stdout) != 32<<20 {
			t.Fatalf("streaming 32 MB: %v, exit %v, %d bytes, stderr %q", err, res.Exit, len(res.Stdout), res.Stderr)
		}
		time.Sleep(5 * time.Second)
		if got := v.MustExec(t, "cat", "/proc/self/cgroup"); got != want {
			t.Errorf("after the load, an exec'd process is in %q, want %q", got, want)
		}
	})

	// Signals that would end most programs must not end PID 1, whose exit
	// panics the kernel.
	t.Run("SignalsToPID1", func(t *testing.T) {
		for _, sig := range []string{"TERM", "INT", "HUP"} {
			v.MustExec(t, "kill", "-"+sig, "1")
			time.Sleep(200 * time.Millisecond)
			select {
			case <-v.Exited():
				t.Fatalf("the guest stopped after SIG%s to PID 1", sig)
			default:
			}
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := v.Guest.Ready(c, "")
			cancel()
			if err != nil {
				t.Fatalf("no Ping after SIG%s to PID 1: %v", sig, err)
			}
		}
		v.MustExec(t, "true")
		serial, _ := os.ReadFile(v.Path("serial"))
		if bytes.Contains(serial, []byte("Kernel panic")) {
			t.Errorf("the kernel panicked:\n%s", serial)
		}
	})
}

// Images a guest's Docker pulls, pinned.
const (
	helloWorldImage = "hello-world@sha256:5e23090353324d887c48ad5e5c56d294eab81588df9605b07d1afe895f9cc8f8"
	busyboxImage    = "busybox@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e" // 1.37
)

func TestDockerInGuest(t *testing.T) {
	cfg := process("dockerd")
	cfg.Nameservers = []string{"1.1.1.1"}
	v := vmtest.New(t, cfg)
	_, addr, gw := v.Net()
	v.Start()
	v.Ready(30 * time.Second)

	deadline := time.Now().Add(90 * time.Second)
	var info vmtest.ExecResult
	for {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		res, err := v.Exec(c, []string{"docker", "info", "--format", "{{.Driver}}|{{.CgroupVersion}}|{{json .Warnings}}"}, vmtest.ExecOpts{})
		cancel()
		if err == nil && res.Exit.GetCode() == 0 {
			info = res
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dockerd did not come up: %v %s", err, res.Stderr)
		}
		time.Sleep(500 * time.Millisecond)
	}
	fields := strings.SplitN(strings.TrimSpace(string(info.Stdout)), "|", 3)
	if len(fields) != 3 {
		t.Fatalf("docker info said %q", info.Stdout)
	}
	t.Logf("docker info: storage driver %s, cgroup v%s, warnings %s", fields[0], fields[1], fields[2])
	// Docker 29 stores images in containerd by default, whose overlay
	// snapshotter it reports as overlayfs; the older store says overlay2.
	// Either way, not the vfs fallback a kernel without overlayfs gets.
	if d := fields[0]; d != "overlayfs" && d != "overlay2" {
		t.Errorf("storage driver %q, want overlay", d)
	}
	if fields[1] != "2" {
		t.Errorf("cgroup version %q, want 2", fields[1])
	}
	if w := fields[2]; (w != "null" && w != "[]") || strings.Contains(string(info.Stderr), "WARNING") {
		t.Errorf("docker info warns: %s %s", w, info.Stderr)
	}

	start := time.Now()
	out := v.MustExec(t, "docker", "run", "--rm", helloWorldImage)
	first := time.Since(start)
	if !strings.Contains(out, "Hello from Docker!") {
		t.Errorf("hello-world said %q", out)
	}
	start = time.Now()
	v.MustExec(t, "docker", "run", "--rm", helloWorldImage)
	t.Logf("docker run hello-world: %s with the pull, %s pulled", first, time.Since(start))

	// A container on the default bridge reaches the host's end of the tap:
	// the connection must arrive from the guest's own address, so it went
	// through the guest's NAT.
	ln, err := net.Listen("tcp", gw+":0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	type conn struct {
		from, said string
		err        error
	}
	got := make(chan conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			got <- conn{err: err}
			return
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(30 * time.Second))
		line, err := bufio.NewReader(c).ReadString('\n')
		got <- conn{from: c.RemoteAddr().(*net.TCPAddr).IP.String(), said: line, err: err}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	v.MustExec(t, "docker", "run", "--rm", busyboxImage, "sh", "-c",
		fmt.Sprintf("echo hello-from-a-container | nc -w 5 %s %d", gw, port))
	select {
	case c := <-got:
		if c.err != nil || c.said != "hello-from-a-container\n" {
			t.Errorf("the host's listener got %q, %v", c.said, c.err)
		}
		if ip := strings.Split(addr, "/")[0]; c.from != ip {
			t.Errorf("the connection came from %s, want the guest's %s", c.from, ip)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no connection from the container")
	}
}

// TestBootTime boots ten guests one after another and reports the median
// time from starting Cloud Hypervisor to the agent's first Ping answer.
func TestBootTime(t *testing.T) {
	const boots = 10
	var times []time.Duration
	for i := range boots {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			v := vmtest.New(t, process("sleep", "3600"))
			v.Start()
			d := v.Ready(30 * time.Second)
			times = append(times, d)
			// Where the time went, by the guest's clock (taken after the Ping,
			// so it does not count).
			kinit := v.MustExec(t, "sh", "-c", `dmesg | sed -n 's/^\[ *\([0-9.]*\)\] Run \/init.*/\1/p'`)
			uptime := v.MustExec(t, "cut", "-d", " ", "-f1", "/proc/uptime")
			if kinit = strings.TrimSpace(kinit); kinit == "" {
				kinit = "?"
			}
			t.Logf("to Ping %s; guest clock: kernel ran /init at %ss, uptime after Ping %ss",
				d, kinit, strings.TrimSpace(uptime))
		})
	}
	if len(times) != boots {
		t.Fatal("not every boot answered")
	}
	t.Logf("time to Ping, %d boots: %v", boots, times)
	slices.Sort(times)
	median := (times[boots/2-1] + times[boots/2]) / 2
	t.Logf("median %s, range %s to %s", median, times[0], times[boots-1])
	if median > 2*time.Second {
		t.Errorf("median boot to Ping is %s; docs/runbook/testbed.md records the last median", median)
	}
}
