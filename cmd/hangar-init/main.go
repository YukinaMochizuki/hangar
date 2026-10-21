// Command hangar-init is PID 1 in every container's guest, and its agent.
// It comes in the platform's initramfs: it mounts the container's root disk,
// switches to it, sets up the network and the files a container expects,
// serves the guest agent over vsock, and runs the container's process. When
// that process ends, it records how on the console and powers the guest off;
// the host's restart policy decides what comes next. See
// docs/decisions/guest-agent.md. A boot disk naming a FileShare's server
// makes the guest that server instead (docs/decisions/file-shares.md).
package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1/guestv1connect"
	"github.com/YukinaMochizuki/hangar/internal/buildinfo"
	"github.com/YukinaMochizuki/hangar/internal/guest/agent"
	"github.com/YukinaMochizuki/hangar/internal/guest/boot"
	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/guest/supervise"
)

// Exit codes for a process that never ran, as shells give them.
const (
	exitBootFailed = 255
	exitNotFound   = 127
	exitCannotRun  = 126
)

func main() {
	started := time.Now()
	if os.Getpid() != 1 {
		fmt.Fprintln(os.Stderr, "hangar-init runs only as PID 1, in a Hangar guest")
		os.Exit(2)
	}
	// PID 1 must not exit, nor panic: either takes the kernel down.
	supervise.Shield()
	con := openConsole()
	logf := func(format string, args ...any) {
		_ = con.Write(console.Init, []byte(fmt.Sprintf(format, args...)+"\n"))
	}
	defer func() {
		if r := recover(); r != nil {
			logf("hangar-init: %v", r)
			_ = con.WriteExit(console.ExitStatus{Code: exitBootFailed})
		}
		powerOff()
	}()

	exit, err := run(con, logf, started)
	if err != nil {
		logf("hangar-init: %v", err)
	}
	_ = con.WriteExit(exit) // the last record: nothing is written after it
}

// openConsole frames output onto the virtio console. Early mounts come
// first, since the console is a device; were they or the console to fail,
// the kernel's console is all that is left to say so on.
func openConsole() *console.Writer {
	if err := boot.Early(); err != nil {
		fmt.Fprintln(os.Stderr, "hangar-init:", err)
	}
	f, err := os.OpenFile(boot.Console, os.O_WRONLY|unix.O_NOCTTY, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hangar-init: no console:", err)
		return console.NewWriter(os.Stderr, time.Now)
	}
	if err := console.Raw(f); err != nil {
		fmt.Fprintln(os.Stderr, "hangar-init: the console mangles records:", err)
	}
	return console.NewWriter(f, time.Now)
}

// run boots the container and runs its process, returning how it ended.
func run(con *console.Writer, logf func(string, ...any), started time.Time) (console.ExitStatus, error) {
	failed := console.ExitStatus{Code: exitBootFailed}
	cfg, err := boot.ReadConfig()
	if err != nil {
		return failed, fmt.Errorf("reading the boot disk: %w", err)
	}
	if cfg.GetFileShareServer() != nil {
		return runServer(cfg, logf, started)
	}
	// Before the volumes, which a FileShare's are reached over.
	if err := boot.Network(cfg.GetInterfaces()); err != nil {
		return failed, fmt.Errorf("network: %w", err)
	}
	if err := boot.SwitchRoot(cfg.GetVolumes(), cfg.GetMachineUid(), logf); err != nil {
		return failed, err
	}
	workload, err := boot.System()
	if err != nil {
		return failed, err
	}
	if err := boot.Identity("/", cfg); err != nil {
		return failed, err
	}

	reaper := supervise.NewReaper()
	stopper := &supervise.Stopper{}
	v6 := boot.NewIPv6(cfg.GetInterfaces())
	if ln, err := vsock.Listen(agent.Port, nil); err != nil {
		// The container runs all the same; the node sees it does not answer.
		logf("hangar-init: guest agent: %v", err)
	} else {
		serveAgent(ln, reaper, stopper, workload, v6, started, logf)
	}

	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return failed, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return failed, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		return failed, err
	}
	if err := supervise.MakeWorkingDir("/", cfg.GetProcess()); err != nil {
		return failed, err
	}
	argv, attr, err := supervise.Spec("/", cfg.GetProcess(), [3]*os.File{devnull, outW, errW})
	if errors.Is(err, supervise.ErrNotFound) {
		return console.ExitStatus{Code: exitNotFound}, err
	}
	if err != nil {
		return failed, err
	}
	attr.Sys.UseCgroupFD = true
	attr.Sys.CgroupFD = int(workload.Fd())
	proc, err := reaper.Start(argv, attr)
	if err != nil {
		code := int32(exitBootFailed)
		switch {
		case errors.Is(err, syscall.ENOENT):
			code = exitNotFound
		case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.ENOEXEC):
			code = exitCannotRun
		}
		return console.ExitStatus{Code: code}, fmt.Errorf("starting %s: %w", argv[0], err)
	}
	stopper.Started(proc)
	_ = devnull.Close()
	_ = outW.Close()
	_ = errW.Close()
	var output sync.WaitGroup
	for r, s := range map[*os.File]console.Stream{outR: console.Stdout, errR: console.Stderr} {
		output.Add(1)
		go func() {
			defer output.Done()
			_, _ = io.Copy(con.StreamWriter(s), r)
		}()
	}
	exit := proc.Wait()
	// What it left running may hold its output open: wait a little.
	drained := make(chan struct{})
	go func() { output.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
	}
	return console.ExitStatus{Code: int32(exit.Code), Signal: int32(exit.Signal)}, nil
}

// serveAgent serves the guest agent on the vsock listener ln to the host
// alone, processes it starts going to the workload cgroup; a Stop goes to
// stopper. It returns at once; serving ends when ln is closed.
func serveAgent(ln net.Listener, reaper *supervise.Reaper, stopper *supervise.Stopper, workload *os.File, v6 *boot.IPv6, started time.Time, logf func(string, ...any)) {
	ln = agent.HostOnly(ln, logf)
	_, h := guestv1connect.NewGuestAgentServiceHandler(&agent.Server{
		Reaper: reaper, Root: "/", Version: buildinfo.Version() + buildMarker, Started: started, Cgroup: workload,
		OnPanic: func(r any) { logf("hangar-init: guest agent: %v", r) },
		ApplyIPv6: func(ifaces []*guestv1.Interface) error {
			err := v6.Set(ifaces)
			if err != nil {
				logf("hangar-init: setting IPv6: %v", err)
			}
			return err
		},
		StopMain: func(grace time.Duration) {
			logf("hangar-init: stopping the container, %v before SIGKILL", grace)
			stopper.Stop(grace)
		},
	}, connect.WithReadMaxBytes(agent.MaxMessage))
	serveHTTP(ln, h)
}

// serveHTTP serves h over HTTP/2 without TLS on ln until it is closed.
func serveHTTP(ln net.Listener, h http.Handler) {
	protocols := &http.Protocols{}
	protocols.SetUnencryptedHTTP2(true)
	go func() { _ = (&http.Server{Handler: h, Protocols: protocols}).Serve(ln) }()
}

// powerOff ends the guest: every process is asked to stop, then made to,
// the disks are left clean, and the guest powers off.
func powerOff() {
	_ = unix.Kill(-1, unix.SIGTERM)
	waitForOthers(2 * time.Second)
	_ = unix.Kill(-1, unix.SIGKILL)
	waitForOthers(2 * time.Second) // until they let go of their files
	unix.Sync()
	boot.Unmount()
	_ = unix.Mount("", "/", "", unix.MS_REMOUNT|unix.MS_RDONLY, "")
	unix.Sync()
	for {
		_ = unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
		time.Sleep(time.Second) // never reached when the power-off works
	}
}

func waitForOthers(d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) && othersRunning() {
		time.Sleep(50 * time.Millisecond)
	}
}

// othersRunning reports whether any process other than this one is left.
func othersRunning() bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if pid := e.Name(); pid != "1" && pid[0] >= '0' && pid[0] <= '9' {
			if _, err := os.Stat("/proc/" + pid + "/exe"); err == nil {
				return true // a kernel thread has no exe
			}
		}
	}
	return false
}
