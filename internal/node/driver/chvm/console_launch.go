package chvm

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync/atomic"
	"syscall"
	"time"
)

// consoleHelper is a firmware VM's console helper as its launcher runs it.
type consoleHelper struct {
	cmd *exec.Cmd
	// listener and logW are the helper's fds, the launcher's copies closed
	// once it runs: the pipe ends when the helper does.
	listener, logW *os.File
	logR           *os.File
	exited         chan struct{}
	stopping       atomic.Bool
}

// openConsole makes the VM's console socket afresh, listening, and the
// pipe the helper hands the guest's output on through. The socket is the
// launcher's, outside chDir: Cloud Hypervisor can neither remove nor replace
// it, and the node dials it as root.
func (l *Launcher) openConsole(ln launch, dir string) (*consoleHelper, error) {
	path := filepath.Join(dir, consoleSocket)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	lis, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	lis.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o600); err != nil {
		_ = lis.Close()
		return nil, err
	}
	f, err := lis.File()
	_ = lis.Close()
	if err != nil {
		return nil, err
	}
	logR, logW, err := os.Pipe()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	cmd := l.consoleCommand(ln, dir)
	cmd.ExtraFiles = []*os.File{f, logW} // consoleFD, consoleLogFD
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	return &consoleHelper{cmd: cmd, listener: f, logW: logW, logR: logR, exited: make(chan struct{})}, nil
}

// consoleCommand runs the console helper as the VM's user, as command runs
// Cloud Hypervisor, with no group but the VM's: it needs no device.
func (l *Launcher) consoleCommand(ln launch, dir string) *exec.Cmd {
	args := append(slices.Clone(l.Console[1:]), "--serial", filepath.Join(dir, serialSocket))
	if ln.VMUser == 0 {
		return exec.Command(l.Console[0], args...)
	}
	id := fmt.Sprint(ln.VMUser)
	argv := append(slices.Clone(l.Exec[1:]), "--uid", id, "--gid", id, "--fds", "2", "--", l.Console[0])
	return exec.Command(l.Exec[0], append(argv, args...)...)
}

// start runs the helper. It is not run again if it ends: running it as the
// VM's user takes the capabilities the launcher gives up once the VM runs,
// so the console, and the log, end with it for the rest of the boot.
func (h *consoleHelper) start(l *Launcher) error {
	err := h.cmd.Start()
	_ = h.listener.Close()
	_ = h.logW.Close()
	if err != nil {
		close(h.exited)
		return err
	}
	go func() {
		err := h.cmd.Wait()
		if !h.stopping.Load() {
			l.Log.Error("vmm: the serial console's helper ended; nothing more of this boot is logged", "error", errString(err))
		}
		close(h.exited)
	}()
	return nil
}

// stop ends the helper, which ends its connection to Cloud Hypervisor
// gently, killing it past grace.
func (h *consoleHelper) stop(grace time.Duration) {
	if h.cmd.Process == nil {
		return
	}
	h.stopping.Store(true)
	_ = h.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-h.exited:
	case <-time.After(grace):
		_ = h.cmd.Process.Kill()
		<-h.exited
	}
}

// close lets go of what openConsole made that start did not.
func (h *consoleHelper) close() {
	_ = h.listener.Close()
	_ = h.logW.Close()
}
