package chvm

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chguest"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/hostnet"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/zfsstore"
)

// Log sizes: a file of the guest's console holds this much before it is
// rotated, so a VM keeps at most twice as much of either.
const (
	DefaultConsoleMax = 16 << 20
	DefaultSerialMax  = 1 << 20
)

// Launcher runs one VM in the foreground: it is hangar-node vmm, the main
// process of the VM's unit, started again by systemd as the restart policy
// says. It needs nothing of the node agent: what it runs is in the VM's
// directory. It opens the VM's root disk, checked to be the machine's, and
// its taps, and runs Cloud Hypervisor on them; it keeps the guest's console
// in a log of bounded size, and exits as the container's process did, so
// systemd's restart policy and restart count see the container. Stopped
// (ctx ends, on SIGTERM), it stops Cloud Hypervisor and exits, recording no
// exit: stopped by the driver, which has stopped the guest already, at
// once; stopped by systemd alone, as at host shutdown, once it has stopped
// the guest as the driver's Stop does, so the container gets its SIGTERM
// and grace. Either way the VM reads as stopped or interrupted, never as
// exited, however its process ended.
type Launcher struct {
	// Dir holds the VMs' directories, as the driver's Config.Dir.
	Dir string
	// OpenRoot opens the root disk at path for the VM, refusing one that
	// is not what path names. Nil means OpenZvol.
	OpenRoot func(path string) (*os.File, error)
	// OpenTap opens an interface's tap. Nil means hostnet.OpenTap.
	OpenTap func(driver.Interface) (*os.File, error)
	// ConsoleMax and SerialMax bound each file of the logs; zero means
	// DefaultConsoleMax and DefaultSerialMax.
	ConsoleMax, SerialMax int64
	// KillGrace bounds how long Cloud Hypervisor has to exit once
	// SIGTERMed before it is killed; zero means 2 seconds.
	KillGrace time.Duration
	// Guest reaches the guest's agent. Nil means chguest's, at the VM's
	// vsock socket.
	Guest driver.Guest
	Log   *slog.Logger
}

// LauncherMain is hangar-node vmm [--dir DIR] UID, returning its exit code.
func LauncherMain(args []string) int {
	fs := flag.NewFlagSet("vmm", flag.ContinueOnError)
	dir := fs.String("dir", DefaultDir, "the directory of the VMs' directories")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: hangar-node vmm [--dir DIR] MACHINE_UID")
		return ExitFailed
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	l := &Launcher{Dir: *dir, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	return l.Run(ctx, fs.Arg(0))
}

// OpenZvol opens the zvol a /dev/zvol link names, refusing what it leads to
// unless the device says, by BLKZNAME, that it is that zvol: a stale link
// can lead to another machine's disk (docs/decisions/storage-zfs.md).
func OpenZvol(path string) (*os.File, error) {
	want, ok := strings.CutPrefix(path, "/dev/zvol/")
	if !ok || want == "" || filepath.Clean(path) != path {
		return nil, fmt.Errorf("%s is not a zvol's link", path)
	}
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_EXCL, 0)
	if err != nil {
		return nil, err
	}
	got, err := zfsstore.VolumeOf(f)
	if err == nil && got != want {
		err = fmt.Errorf("%s leads to the zvol %s", path, got)
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("refusing the root disk: %w", err)
	}
	return f, nil
}

func (l *Launcher) defaults() {
	if l.OpenRoot == nil {
		l.OpenRoot = OpenZvol
	}
	if l.OpenTap == nil {
		l.OpenTap = hostnet.OpenTap
	}
	if l.ConsoleMax == 0 {
		l.ConsoleMax = DefaultConsoleMax
	}
	if l.SerialMax == 0 {
		l.SerialMax = DefaultSerialMax
	}
	if l.KillGrace == 0 {
		l.KillGrace = killGrace
	}
	if l.Guest == nil {
		l.Guest = chguest.Guest{Socket: func(uid string) string { return filepath.Join(l.Dir, uid, vsockFile) }}
	}
	if l.Log == nil {
		l.Log = slog.New(slog.DiscardHandler)
	}
}

// Run runs the machine's VM until it powers off, or ctx ends, and returns
// the code to exit with.
func (l *Launcher) Run(ctx context.Context, uid string) int {
	l.defaults()
	if err := checkUID(uid); err != nil {
		l.Log.Error("vmm", "error", err)
		return ExitFailed
	}
	dir := filepath.Join(l.Dir, uid)
	started := time.Now()
	// A record of an earlier run would say this one ended.
	if err := os.Remove(filepath.Join(dir, exitFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		l.Log.Error("vmm: removing the last exit record", "error", err)
	}
	code, err := l.run(ctx, uid, dir, started)
	if err != nil {
		l.Log.Error("vmm: the VM did not start", "machine_uid", uid, "error", err)
		l.record(dir, exitRecord{Code: ExitFailed, Error: err.Error(), Started: started, Ended: time.Now()})
		code = ExitFailed
	}
	if code < 0 { // stopped: no record, and no restart either way
		return 0
	}
	// A VM being stopped is not started again: the launcher waits for the
	// stop, so systemd sees no exit to restart it for.
	if exists(filepath.Join(dir, stopFile)) {
		l.Log.Info("vmm: the guest is off, waiting to be stopped", "machine_uid", uid)
		<-ctx.Done()
	}
	return code
}

// run runs the VM. It returns the code to exit with, or -1 if ctx ended,
// and an error if the VM could not be started.
func (l *Launcher) run(ctx context.Context, uid, dir string, started time.Time) (int, error) {
	ln, err := readLaunch(dir)
	if err != nil {
		return 0, err
	}
	if ln.Spec.MachineUID != uid {
		return 0, fmt.Errorf("%s is machine %q's, not %q's", filepath.Join(dir, specFile), ln.Spec.MachineUID, uid)
	}
	root, err := l.OpenRoot(ln.Spec.Root.Path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = root.Close() }()
	files := []*os.File{root}
	defer func() {
		for _, f := range files[1:] {
			_ = f.Close()
		}
	}()
	for _, i := range ln.Spec.Interfaces {
		tap, err := l.OpenTap(i)
		if err != nil {
			return 0, err
		}
		files = append(files, tap)
	}

	clog, err := openCapped(filepath.Join(dir, ConsoleLog), l.ConsoleMax)
	if err != nil {
		return 0, err
	}
	defer func() { _ = clog.Close() }()
	serialLog, err := openCapped(filepath.Join(dir, SerialLog), l.SerialMax)
	if err != nil {
		return 0, err
	}
	defer func() { _ = serialLog.Close() }()
	serial, err := openFIFO(filepath.Join(dir, serialFIFO))
	if err != nil {
		return 0, err
	}
	defer func() { _ = serial.Close() }()
	if err := os.Remove(filepath.Join(dir, vsockFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}

	// The console comes on Cloud Hypervisor's stdout; its stdin, the
	// console's input, is held open and never written.
	outR, outW, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	defer func() { _ = outR.Close() }()
	inR, inW, err := os.Pipe()
	if err != nil {
		_ = outW.Close()
		return 0, err
	}
	defer func() { _ = inW.Close() }()
	cmd := exec.Command(ln.CloudHypervisor, chArgs(ln, dir, len(files)-1)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, os.Stderr
	cmd.ExtraFiles = files // fd 3 is the root disk, the taps follow
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	err = cmd.Start()
	_ = inR.Close()
	_ = outW.Close()
	if err != nil {
		return 0, err
	}
	l.Log.Info("vmm: started", "machine_uid", uid, "machine", ln.Spec.Machine, "pid", cmd.Process.Pid, "spec", ln.Hash)

	consoleDone := make(chan *console.ExitStatus, 1)
	go func() {
		consoleDone <- pumpConsole(outR, clog, func(err error) {
			l.Log.Error("vmm: the console log fails; dropping records", "error", err)
		})
	}()
	serialDone := make(chan struct{})
	go func() {
		defer close(serialDone)
		pumpRaw(serial, serialLog, func(err error) {
			l.Log.Error("vmm: the serial log fails; dropping output", "error", err)
		})
	}()
	defer func() {
		_ = serial.Close()
		<-serialDone
	}()
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		// The driver marks a VM before it stops the guest: without the
		// mark, systemd stops the unit on its own.
		if exists(filepath.Join(dir, stopFile)) || !l.stopGuest(uid, ln, waited) {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-waited:
			case <-time.After(l.KillGrace):
				_ = cmd.Process.Kill()
				<-waited
			}
		}
		l.drain(consoleDone)
		l.Log.Info("vmm: stopped", "machine_uid", uid)
		return -1, nil
	case err := <-waited:
		st := l.drain(consoleDone)
		code := exitCode(st)
		rec := exitRecord{Code: code, Started: started, Ended: time.Now()}
		if st != nil {
			rec.Signal = int(st.Signal)
		}
		l.record(dir, rec)
		l.Log.Info("vmm: the VM ended", "machine_uid", uid, "cloud_hypervisor", errString(err), "exit_code", code, "recorded", st != nil)
		return code, nil
	}
}

// stopGuest asks the guest to stop its process with the launch's grace, as
// the driver's Stop does, and waits for the guest to power off, Cloud
// Hypervisor exiting, for the grace and the slack. It reports whether Cloud
// Hypervisor has exited, and returns at once if the guest does not take
// the stop.
func (l *Launcher) stopGuest(uid string, ln launch, waited <-chan error) bool {
	grace, slack := ln.stopTimes()
	l.Log.Info("vmm: stopped by systemd; stopping the guest", "machine_uid", uid, "grace", grace)
	ctx, cancel := context.WithTimeout(context.Background(), stopAsk)
	err := l.Guest.Stop(ctx, uid, grace)
	cancel()
	if err != nil {
		l.Log.Error("vmm: the guest did not take the stop", "machine_uid", uid, "error", err)
		return false
	}
	deadline := time.NewTimer(grace + slack)
	defer deadline.Stop()
	select {
	case <-waited:
		return true
	case <-deadline.C:
		l.Log.Error("vmm: the guest did not power off in its grace", "machine_uid", uid)
		return false
	}
}

// drain waits a little for the rest of the console once Cloud Hypervisor
// has exited.
func (l *Launcher) drain(done <-chan *console.ExitStatus) *console.ExitStatus {
	select {
	case st := <-done:
		return st
	case <-time.After(drainWait):
		l.Log.Error("vmm: the console did not end with Cloud Hypervisor")
		return nil
	}
}

func errString(err error) string {
	if err == nil {
		return "exited 0"
	}
	return err.Error()
}

func (l *Launcher) record(dir string, r exitRecord) {
	b, _ := json.Marshal(r)
	if err := writeAtomic(filepath.Join(dir, exitFile), b); err != nil {
		l.Log.Error("vmm: recording the exit", "error", err)
	}
}

// openFIFO makes a FIFO at path, afresh, and opens it to read from. It is
// opened for writing too, so it never reads as ended between writers.
func openFIFO(path string) (*os.File, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := unix.Mkfifo(path, 0o600); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

// chArgs is Cloud Hypervisor's command line for the launch: the root disk
// on fd 3 and the taps on the fds after it, as the launcher passes them.
func chArgs(ln launch, dir string, taps int) []string {
	s := ln.Spec
	args := []string{
		"--kernel", ln.Kernel,
		"--initramfs", ln.Initramfs,
		"--cmdline", ln.Cmdline,
		"--cpus", fmt.Sprintf("boot=%d", s.VCPUs),
		"--memory", fmt.Sprintf("size=%dM", s.MemoryMiB),
		"--disk",
		"path=/dev/fd/3,image_type=raw",
		"path=" + filepath.Join(dir, bootFile) + ",image_type=raw,readonly=on",
	}
	if taps > 0 {
		args = append(args, "--net")
		for n, i := range s.Interfaces[:taps] {
			args = append(args, fmt.Sprintf("fd=%d,mac=%s", 4+n, i.MAC))
		}
	}
	return append(args,
		"--vsock", "cid=3,socket="+filepath.Join(dir, vsockFile),
		"--console", "tty",
		"--serial", "file="+filepath.Join(dir, serialFIFO),
	)
}
