//go:build testbed

// Package vmtest boots guests under Cloud Hypervisor for the real-host tests
// (L4 in docs/decisions/testing-strategy.md): the platform kernel, its
// initramfs and hangar-init, driven only through what the node uses (the boot
// disk, the framed console, the guest agent over hybrid vsock). A package
// using it calls Main from its TestMain. The tests need root, KVM and
// cloud-hypervisor on PATH; make e2e-host runs them (docs/runbook/testbed.md).
//
// Nothing a run creates outlives it: guests die with the test binary
// (Pdeathsig), and the guests' directories, taps and NAT table are removed
// at the end, on SIGINT or SIGTERM, and just before go test's -timeout would
// kill the binary. What a killed run left anyway is swept at the next start.
package vmtest

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Inputs are the artifacts under test.
type Inputs struct {
	// Kernel and Initramfs come from make guest-kernel and make
	// guest-initramfs; Rootfs, an ext4 image of docker:dind, from
	// test/testbed/guest-rootfs.sh.
	Kernel, Initramfs, Rootfs string
	// Cmdline is HANGAR_KERNEL_CMDLINE from guest/kernel/kernel.env, or
	// HANGAR_TESTBED_CMDLINE.
	Cmdline string
	// Node is hangar-node, built by make e2e-host, or HANGAR_TESTBED_NODE;
	// a package that uses it checks that it is there.
	Node string
	// Apiserver, CLI and Edge are hangar-apiserver, hangar and
	// hangar-edge, built by make e2e-host, or HANGAR_TESTBED_APISERVER,
	// HANGAR_TESTBED_CLI and HANGAR_TESTBED_EDGE; a package that uses them
	// checks that they are there.
	Apiserver, CLI, Edge string
	// CHProbe stands in for Cloud Hypervisor to try what a VM's unit
	// lets it do (test/testbed/vms/chprobe), built by make e2e-host, or
	// HANGAR_TESTBED_CHPROBE; a package that uses it checks that it is
	// there.
	CHProbe string
	// VsockProbe tries to reach the guest agent from inside a guest
	// (test/testbed/guest/vsockprobe), built by make e2e-host, or
	// HANGAR_TESTBED_VSOCKPROBE; a package that uses it checks that it is
	// there.
	VsockProbe string
}

var in Inputs

// Artifacts returns what Main loaded.
func Artifacts() Inputs { return in }

// Names of what a run creates on the host, also what the sweep looks for.
const (
	// workDir holds one directory per guest, with its disks and sockets.
	workDir = "/var/tmp/hangar-testbed-guests"
	// lockFile keeps a second test binary waiting until the first is done.
	lockFile  = "/run/lock/hangar-testbed.lock"
	tapPrefix = "hgt"
	natTable  = "hangar_testbed_guest"
)

// Main checks the host and the inputs, sweeps what an earlier run left,
// sets up NAT for the guests, runs the tests and cleans up.
func Main(m *testing.M) {
	flag.Parse()
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "vmtest:", err)
		os.Exit(1)
	}
	if os.Geteuid() != 0 {
		fail(errors.New("must run as root (make e2e-host does)"))
	}
	var err error
	if in, err = loadInputs(); err != nil {
		fail(err)
	}
	// One binary at a time on a testbed: the sweep, the taps and the NAT
	// table are the host's, not this binary's. Held until the process ends.
	lock, err := os.OpenFile(lockFile, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fail(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		fail(fmt.Errorf("locking %s: %w", lockFile, err))
	}
	sweep()
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		fail(err)
	}
	if err := natUp(); err != nil {
		fail(fmt.Errorf("NAT: %w", err))
	}
	atExit(natDown)
	atExit(func() { _ = os.RemoveAll(workDir) })

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sigs
		fmt.Fprintf(os.Stderr, "vmtest: %v: cleaning up\n", s)
		cleanUp()
		os.Exit(1)
	}()
	if d := testTimeout(); d > 0 {
		margin := min(5*time.Second, d/10)
		time.AfterFunc(d-margin, func() {
			buf := make([]byte, 1<<20)
			fmt.Fprintf(os.Stderr, "vmtest: -timeout %s is near; every goroutine, to show what hung:\n%s\n", d, buf[:runtime.Stack(buf, true)])
			fmt.Fprintln(os.Stderr, "vmtest: cleaning up")
			cleanUp()
			os.Exit(1)
		})
	}
	code := m.Run()
	cleanUp()
	os.Exit(code)
}

func testTimeout() time.Duration {
	f := flag.Lookup("test.timeout")
	if f == nil {
		return 0
	}
	d, _ := f.Value.(flag.Getter).Get().(time.Duration)
	return d
}

var (
	exitMu  sync.Mutex
	exitFns = map[int]func(){}
	exitSeq int
)

// atExit runs f when the run ends, however it ends; the returned func runs
// it now instead, once.
func atExit(f func()) (now func()) {
	exitMu.Lock()
	defer exitMu.Unlock()
	exitSeq++
	id := exitSeq
	exitFns[id] = f
	return func() {
		exitMu.Lock()
		f, ok := exitFns[id]
		delete(exitFns, id)
		exitMu.Unlock()
		if ok {
			f()
		}
	}
}

// AtExit runs f once the run ends, however it ends: after the tests, on
// SIGINT or SIGTERM, or just before -timeout.
func AtExit(f func()) { atExit(f) }

// cleanUp runs every pending atExit function, the most recent first.
func cleanUp() {
	exitMu.Lock()
	ids := make([]int, 0, len(exitFns))
	for id := range exitFns {
		ids = append(ids, id)
	}
	exitMu.Unlock()
	slices.Sort(ids)
	slices.Reverse(ids)
	for _, id := range ids {
		exitMu.Lock()
		f, ok := exitFns[id]
		delete(exitFns, id)
		exitMu.Unlock()
		if ok {
			f()
		}
	}
}

// sweep removes what a run that was killed outright left behind: its
// guests, their taps, the NAT table and the guests' directories.
func sweep() {
	procs, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	for _, p := range procs {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		args := strings.Split(string(b), "\x00")
		if filepath.Base(args[0]) != "cloud-hypervisor" || !strings.Contains(string(b), "tap="+tapPrefix) {
			continue
		}
		var pid int
		if _, err := fmt.Sscanf(p, "/proc/%d/cmdline", &pid); err == nil {
			fmt.Fprintf(os.Stderr, "vmtest: sweeping a stale guest, pid %d\n", pid)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	if ifaces, err := net.Interfaces(); err == nil {
		for _, i := range ifaces {
			if strings.HasPrefix(i.Name, tapPrefix) {
				fmt.Fprintf(os.Stderr, "vmtest: sweeping a stale tap, %s\n", i.Name)
				_ = exec.Command("ip", "link", "del", i.Name).Run()
			}
		}
	}
	natDown()
	_ = os.RemoveAll(workDir)
}

func loadInputs() (Inputs, error) {
	root, err := repoRoot()
	if err != nil {
		return Inputs{}, err
	}
	pick := func(env, def string) string {
		if v := os.Getenv(env); v != "" {
			return v
		}
		if def == "" {
			return ""
		}
		return filepath.Join(root, def)
	}
	i := Inputs{
		Kernel:     pick("HANGAR_TESTBED_KERNEL", "bin/guest/vmlinux"),
		Initramfs:  pick("HANGAR_TESTBED_INITRAMFS", "bin/guest/initramfs.cpio"),
		Rootfs:     pick("HANGAR_TESTBED_ROOTFS", ""),
		Node:       pick("HANGAR_TESTBED_NODE", "bin/hangar-node"),
		Apiserver:  pick("HANGAR_TESTBED_APISERVER", "bin/hangar-apiserver"),
		CLI:        pick("HANGAR_TESTBED_CLI", "bin/hangar"),
		Edge:       pick("HANGAR_TESTBED_EDGE", "bin/hangar-edge"),
		CHProbe:    pick("HANGAR_TESTBED_CHPROBE", "bin/ch-probe"),
		VsockProbe: pick("HANGAR_TESTBED_VSOCKPROBE", "bin/vsock-probe"),
	}
	if i.Rootfs == "" {
		return Inputs{}, errors.New("HANGAR_TESTBED_ROOTFS is not set (make e2e-host sets it)")
	}
	for _, p := range []string{i.Kernel, i.Initramfs, i.Rootfs} {
		if _, err := os.Stat(p); err != nil {
			return Inputs{}, fmt.Errorf("%w (docs/runbook/testbed.md says how to build it)", err)
		}
	}
	if _, err := exec.LookPath("cloud-hypervisor"); err != nil {
		return Inputs{}, err
	}
	env, err := os.ReadFile(filepath.Join(root, "guest/kernel/kernel.env"))
	if err != nil {
		return Inputs{}, err
	}
	m := regexp.MustCompile(`(?m)^HANGAR_KERNEL_CMDLINE="([^"]*)"$`).FindSubmatch(env)
	if m == nil {
		return Inputs{}, errors.New("no HANGAR_KERNEL_CMDLINE in guest/kernel/kernel.env")
	}
	i.Cmdline = string(m[1])
	if c := os.Getenv("HANGAR_TESTBED_CMDLINE"); c != "" {
		i.Cmdline = c
	}
	return i, nil
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found above the working directory")
		}
		dir = parent
	}
}

// Guests get 172.31.<n>.2/24 behind a tap at 172.31.<n>.1, NATed out. IPv4
// forwarding is turned on and left on.
func natUp() error {
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0); err != nil {
		return err
	}
	rules := fmt.Sprintf(`table ip %s {
	chain post { type nat hook postrouting priority srcnat; ip saddr 172.31.0.0/16 ip daddr != 172.31.0.0/16 masquerade; }
}`, natTable)
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

func natDown() { _ = exec.Command("nft", "delete", "table", "ip", natTable).Run() }
