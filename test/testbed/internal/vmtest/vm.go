//go:build testbed

package vmtest

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/internal/guest/bootdisk"
	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chguest"
)

var vmCount atomic.Int32

// GuestCID is the vsock CID every guest is given.
const GuestCID = 3

// VM is one guest under Cloud Hypervisor.
type VM struct {
	// Guest reaches the guest's agent; the machine UID it is given is
	// ignored.
	Guest chguest.Guest
	// Disks are attached after the boot disk, as vdc on, raw.
	Disks []string
	// CPUs and MemoryMB size the guest; 0 is 2 and 2048.
	CPUs, MemoryMB int

	t       testing.TB
	dir     string
	n       int
	cmd     *exec.Cmd
	started time.Time
	exited  chan struct{}
	exitErr error
	stop    func()
}

// Net returns the guest's MAC, its address with the prefix length, and its
// gateway, the host's end of the guest's tap.
func (v *VM) Net() (mac, addr, gw string) {
	return fmt.Sprintf("02:48:47:00:%02x:02", v.n), fmt.Sprintf("172.31.%d.2/24", v.n), fmt.Sprintf("172.31.%d.1", v.n)
}

// New prepares a guest's disks: a private copy of the root image, and a boot
// disk with cfg, whose interface is filled in when it has none. Start boots
// it; it is stopped and removed when the test ends.
func New(t testing.TB, cfg *guestv1.BootConfig) *VM {
	t.Helper()
	v := &VM{t: t, n: int(vmCount.Add(1)), exited: make(chan struct{})}
	if v.n > 250 {
		t.Fatal("out of guest subnets")
	}
	dir, err := os.MkdirTemp(workDir, "vm-")
	if err != nil {
		t.Fatal(err)
	}
	v.dir = dir
	removeDir := atExit(func() { _ = os.RemoveAll(dir) })
	t.Cleanup(func() {
		if t.Failed() {
			v.dump()
		}
		if v.stop != nil {
			v.stop()
		}
		removeDir()
	})
	v.Guest = chguest.Guest{Socket: func(string) string { return v.Path("vsock") }}

	// Sparse, and a reflink where the filesystem has them.
	if out, err := exec.Command("cp", "--sparse=always", "--reflink=auto", in.Rootfs, v.Path("root.img")).CombinedOutput(); err != nil {
		t.Fatalf("copying the root disk: %v: %s", err, out)
	}
	// Flushed before the boot: writeback of the fresh copy otherwise races
	// the guest's first reads, and the root mount alone takes 300ms or more.
	if out, err := exec.Command("sync", v.Path("root.img")).CombinedOutput(); err != nil {
		t.Fatalf("flushing the root disk: %v: %s", err, out)
	}
	v.SetBootConfig(cfg)
	return v
}

// SetBootConfig writes the boot disk anew with cfg, whose interface is
// filled in when it has none, for the next Start.
func (v *VM) SetBootConfig(cfg *guestv1.BootConfig) {
	t := v.t
	t.Helper()
	mac, addr, gw := v.Net()
	if len(cfg.GetInterfaces()) == 0 {
		cfg.Interfaces = []*guestv1.Interface{{Mac: mac, Address: addr, Gateway: gw}}
	}
	var disk bytes.Buffer
	if err := bootdisk.Write(&disk, cfg); err != nil {
		t.Fatal(err)
	}
	// A disk is whole sectors; the reader takes the length from the header.
	if r := disk.Len() % 512; r != 0 {
		disk.Write(make([]byte, 512-r))
	}
	if err := os.WriteFile(v.Path("boot.img"), disk.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Addr is the guest's IPv4 address, without the prefix length.
func (v *VM) Addr() string {
	_, addr, _ := v.Net()
	return strings.TrimSuffix(addr, "/24")
}

// Path is a file in the guest's directory: root.img, boot.img, console (the
// framed virtio console), serial (the kernel's), ch.log, vsock.
func (v *VM) Path(name string) string { return filepath.Join(v.dir, name) }

// Put copies the host's file src into the root disk's top directory as name,
// owned by root with mode 0755, before Start.
func (v *VM) Put(src, name string) {
	t := v.t
	t.Helper()
	if strings.Contains(name, "/") {
		t.Fatalf("Put: %q is not a name in the top directory", name)
	}
	cmds := fmt.Sprintf("cd /\nwrite %s %s\nsif %s mode 0100755\nsif %s uid 0\nsif %s gid 0\n", src, name, name, name, name)
	cmd := exec.Command("debugfs", "-w", "-f", "-", v.Path("root.img"))
	cmd.Stdin = strings.NewReader(cmds)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("debugfs: %v: %s", err, out)
	}
	// debugfs exits 0 whatever its commands did.
	out, err := exec.Command("debugfs", "-R", "stat /"+name, v.Path("root.img")).CombinedOutput()
	if err != nil || !regexp.MustCompile(`Type: regular\s+Mode:\s+0755`).Match(out) || !regexp.MustCompile(`User:\s+0\s+Group:\s+0\s`).Match(out) {
		t.Fatalf("%s did not land as a root-owned 0755 file in the root disk: %v: %s", name, err, out)
	}
}

// Start launches Cloud Hypervisor as the node will: the root disk on vda,
// the boot disk on vdb, Disks after them, the virtio console to a file,
// hybrid vsock. A guest that is off may be started again, on the same
// disks; its console and serial logs then start anew.
func (v *VM) Start() {
	t := v.t
	t.Helper()
	if v.cmd != nil {
		select {
		case <-v.exited:
		default:
			t.Fatal("Start: the guest is running")
		}
		v.stop()
		v.exited = make(chan struct{})
		// Cloud Hypervisor will not listen where a socket is left.
		_ = os.Remove(v.Path("vsock"))
	}
	mac, _, gw := v.Net()
	cpus, mem := cmp.Or(v.CPUs, 2), cmp.Or(v.MemoryMB, 2048)
	disks := []string{"path=" + v.Path("root.img") + ",image_type=raw", "path=" + v.Path("boot.img") + ",image_type=raw,readonly=on"}
	for _, d := range v.Disks {
		disks = append(disks, "path="+d+",image_type=raw")
	}
	args := []string{
		"--kernel", in.Kernel,
		"--initramfs", in.Initramfs,
		"--cmdline", in.Cmdline,
		"--cpus", fmt.Sprintf("boot=%d", cpus),
		"--memory", fmt.Sprintf("size=%dM", mem),
		"--disk",
	}
	args = append(append(args, disks...),
		"--net", fmt.Sprintf("tap=%s%d,ip=%s,mask=255.255.255.0,mac=%s", tapPrefix, v.n, gw, mac),
		"--vsock", fmt.Sprintf("cid=%d,socket=%s", GuestCID, v.Path("vsock")),
		"--console", "file="+v.Path("console"),
		"--serial", "file="+v.Path("serial"),
	)
	v.cmd = exec.Command("cloud-hypervisor", args...)
	// The guest dies with the test binary, however that ends.
	v.cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	log, err := os.Create(v.Path("ch.log"))
	if err != nil {
		t.Fatal(err)
	}
	v.cmd.Stdout, v.cmd.Stderr = log, log
	v.started = time.Now()
	if err := v.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		v.exitErr = v.cmd.Wait()
		_ = log.Close()
		close(v.exited)
	}()
	v.stop = atExit(func() {
		_ = v.cmd.Process.Kill()
		<-v.exited
	})
}

// Exited is closed once Cloud Hypervisor has exited.
func (v *VM) Exited() <-chan struct{} { return v.exited }

// Kill kills Cloud Hypervisor, as a crash of the host's side would end the
// guest, and waits for it to exit.
func (v *VM) Kill() {
	_ = v.cmd.Process.Kill()
	<-v.exited
}

// dump logs what the host saw of a failed guest.
func (v *VM) dump() {
	for _, f := range []string{"ch.log", "serial"} {
		b, _ := os.ReadFile(v.Path(f))
		v.t.Logf("--- %s:\n%s", f, b)
	}
	recs, err := v.Console()
	for _, r := range recs {
		v.t.Logf("console %d: %q", r.Stream, r.Payload)
	}
	if err != nil {
		v.t.Logf("console: %v", err)
	}
}

// Ready waits for the guest agent to answer a Ping, returning how long it
// took since Cloud Hypervisor was started.
func (v *VM) Ready(timeout time.Duration) time.Duration {
	v.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		err := v.Guest.Ready(ctx, "")
		cancel()
		if err == nil {
			return time.Since(v.started)
		}
		select {
		case <-v.exited:
			v.t.Fatalf("the guest stopped before answering: %v", v.exitErr)
		default:
		}
		if time.Now().After(deadline) {
			v.t.Fatalf("no Ping answer in %s: %v", timeout, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// WaitPowerOff waits for Cloud Hypervisor to exit by itself, as it does
// when the guest powers off.
func (v *VM) WaitPowerOff(timeout time.Duration) {
	v.t.Helper()
	select {
	case <-v.exited:
		if v.exitErr != nil {
			v.t.Errorf("cloud-hypervisor exited with %v", v.exitErr)
		}
	case <-time.After(timeout):
		v.t.Fatalf("the guest did not power off within %s", timeout)
	}
}

// CheckCleanDisk checks, once the guest is off, that it left its root
// filesystem clean: marked so, and with nothing for a full check to find.
func (v *VM) CheckCleanDisk(t testing.TB) {
	t.Helper()
	out, err := exec.Command("dumpe2fs", "-h", v.Path("root.img")).CombinedOutput()
	if err != nil {
		t.Fatalf("dumpe2fs: %v: %s", err, out)
	}
	if !regexp.MustCompile(`(?m)^Filesystem state:\s+clean$`).Match(out) {
		state := regexp.MustCompile(`(?m)^Filesystem state:.*$`).Find(out)
		t.Errorf("the root filesystem was left %q", state)
	}
	if regexp.MustCompile(`(?m)^Filesystem features:.*needs_recovery`).Match(out) {
		t.Error("the root filesystem's journal needs recovery")
	}
	if out, err := exec.Command("e2fsck", "-fn", v.Path("root.img")).CombinedOutput(); err != nil {
		t.Errorf("e2fsck -fn: %v:\n%s", err, out)
	}
}

// Console reads every record the guest framed on its console.
func (v *VM) Console() ([]console.Record, error) {
	f, err := os.Open(v.Path("console"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	r := console.NewReader(bufio.NewReader(f))
	var out []console.Record
	for {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, rec)
	}
}

// ExecResult is what an Exec returned.
type ExecResult struct {
	Stdout, Stderr []byte
	Exit           *guestv1.ExecExit
}

// ExecOpts shapes an Exec beyond its process.
type ExecOpts struct {
	Stdin      []byte
	CloseStdin bool
	TTY        bool
	Size       *guestv1.WindowSize
	// User runs the process as Process.user says; empty is root.
	User string
	// Signal is sent once the first output arrives.
	Signal int32
}

// Exec runs a process in the guest through the guest agent.
func (v *VM) Exec(ctx context.Context, args []string, o ExecOpts) (ExecResult, error) {
	c, done := v.Guest.Client("")
	defer done()
	stream := c.Exec(ctx)
	defer func() { _ = stream.CloseResponse() }()
	start := &guestv1.ExecStart{Process: &guestv1.Process{Args: args, User: o.User}, Tty: o.TTY, Size: o.Size}
	if err := stream.Send(&guestv1.ExecRequest{Message: &guestv1.ExecRequest_Start{Start: start}}); err != nil {
		return ExecResult{}, err
	}
	if o.Stdin != nil {
		if err := stream.Send(&guestv1.ExecRequest{Message: &guestv1.ExecRequest_Stdin{Stdin: o.Stdin}}); err != nil {
			return ExecResult{}, err
		}
	}
	if o.CloseStdin {
		if err := stream.Send(&guestv1.ExecRequest{Message: &guestv1.ExecRequest_StdinClose{StdinClose: &guestv1.StdinClose{}}}); err != nil {
			return ExecResult{}, err
		}
	}
	var res ExecResult
	signalled := false
	for {
		msg, err := stream.Receive()
		if err != nil {
			return res, err
		}
		switch m := msg.GetMessage().(type) {
		case *guestv1.ExecResponse_Stdout:
			res.Stdout = append(res.Stdout, m.Stdout...)
		case *guestv1.ExecResponse_Stderr:
			res.Stderr = append(res.Stderr, m.Stderr...)
		case *guestv1.ExecResponse_Exit:
			res.Exit = m.Exit
			_ = stream.CloseRequest()
			return res, nil
		}
		if o.Signal != 0 && !signalled {
			signalled = true
			if err := stream.Send(&guestv1.ExecRequest{Message: &guestv1.ExecRequest_Signal{Signal: o.Signal}}); err != nil {
				return res, err
			}
		}
	}
}

// MustExec runs a process and fails t unless it exits 0, returning its
// stdout.
func (v *VM) MustExec(t testing.TB, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := v.Exec(ctx, args, ExecOpts{})
	if err != nil {
		t.Fatalf("exec %q: %v", args, err)
	}
	if res.Exit.GetCode() != 0 {
		t.Fatalf("exec %q: exit %d\nstdout: %s\nstderr: %s", args, res.Exit.GetCode(), res.Stdout, res.Stderr)
	}
	return string(res.Stdout)
}
