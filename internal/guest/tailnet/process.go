package tailnet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/guest/supervise"
)

// MountDisk mounts the platform's tailscale disk, device, at Mount,
// read-only, and checks it holds tailscaled and the CLI.
func MountDisk(device string) error {
	if device == "" {
		return errors.New("this guest's host has no tailscale disk to give it")
	}
	if err := os.MkdirAll(Mount, 0o755); err != nil {
		return err
	}
	if err := unix.Mount(device, Mount, "ext4", unix.MS_RDONLY|unix.MS_NODEV|unix.MS_NOSUID, ""); err != nil {
		return fmt.Errorf("mounting the tailscale disk %s on %s: %w", device, Mount, err)
	}
	for _, name := range []string{"tailscaled", "tailscale"} {
		if fi, err := os.Stat(filepath.Join(BinDir, name)); err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("the tailscale disk %s holds no %s", device, filepath.Join(BinDir, name))
		}
	}
	return nil
}

// Env is tailscaled's environment. The guest's kernel has nftables alone,
// which tailscaled's firewall detection does not pick by itself; and its
// log files go in StateDir, not in /var/lib/tailscale, which is the
// image's.
func Env() []string {
	return []string{"PATH=" + supervise.DefaultPath, "TS_DEBUG_FIREWALL_MODE=nftables", "TS_LOGS_DIR=" + StateDir}
}

// DaemonArgs is tailscaled's command line: TUN mode, its state on the root
// disk, its socket where the CLI looks, and no log upload to Tailscale, so
// a tenant's node ships nothing of its own to a third party.
func DaemonArgs() []string {
	return []string{
		filepath.Join(BinDir, "tailscaled"),
		"--no-logs-no-support",
		"--tun=tailscale0",
		"--statedir=" + StateDir,
		"--state=" + filepath.Join(StateDir, "tailscaled.state"),
		"--socket=" + Socket,
	}
}

// startDaemon starts tailscaled in a session of its own, so a stop of the
// container's process group leaves it be, its output into LogFile.
func startDaemon(reaper *supervise.Reaper) (*supervise.Process, error) {
	for _, dir := range []string{StateDir, filepath.Dir(Socket)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	defer func() { _ = devnull.Close() }()
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer func() { _ = w.Close() }()
	argv := DaemonArgs()
	proc, err := reaper.Start(argv, &os.ProcAttr{
		Dir: "/", Env: Env(), Files: []*os.File{devnull, w, w},
		Sys: &syscall.SysProcAttr{Setsid: true},
	})
	if err != nil {
		_ = r.Close()
		return nil, err
	}
	go func() {
		defer func() { _ = r.Close() }()
		_, _ = io.Copy(&cappedLog{path: LogFile, max: maxLog}, r)
	}()
	return proc, nil
}

// runCLI runs the tailscale CLI with args, the key, if any, readable on
// keyFD from a pipe, and returns its output, at most 64 KiB of it.
func runCLI(ctx context.Context, reaper *supervise.Reaper, args []string, key []byte) (string, error) {
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return "", err
	}
	defer func() { _ = devnull.Close() }()
	r, w, err := os.Pipe()
	if err != nil {
		return "", err
	}
	defer func() { _ = r.Close() }()
	files := []*os.File{devnull, w, w}
	if key != nil {
		kr, kw, err := os.Pipe()
		if err != nil {
			_ = w.Close()
			return "", err
		}
		defer func() { _ = kr.Close() }()
		// A key is far smaller than a pipe's buffer: the write never
		// waits for the reader.
		_, err = kw.Write(key)
		_ = kw.Close()
		if err != nil {
			_ = w.Close()
			return "", err
		}
		files = append(files, kr)
	}
	argv := CLIArgs(cliPath, args)
	proc, err := reaper.Start(argv, &os.ProcAttr{
		Dir: "/", Env: Env(), Files: files, Sys: &syscall.SysProcAttr{Setsid: true},
	})
	_ = w.Close()
	if err != nil {
		return "", err
	}
	out := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(io.LimitReader(r, 64<<10))
		_, _ = io.Copy(io.Discard, r)
		out <- string(b)
	}()
	ctx, cancel := context.WithTimeout(ctx, upTimeout+30*time.Second)
	defer cancel()
	select {
	case <-proc.Done():
	case <-ctx.Done():
		_ = unix.Kill(-proc.Pid, unix.SIGKILL)
	}
	exit := proc.Wait()
	o := <-out
	if exit.Code != 0 {
		return o, fmt.Errorf("exit code %d", exit.Code)
	}
	return o, nil
}

// cliPath is the tailscale CLI runCLI runs.
var cliPath = filepath.Join(BinDir, "tailscale")

// CLIArgs is the command line of the tailscale CLI at path running args
// against tailscaled's socket. A key is never one of them: it is read from
// keyFD.
func CLIArgs(path string, args []string) []string {
	return append([]string{path, "--socket=" + Socket}, args...)
}

// cappedLog appends to path, moving it to path.1 once it holds max bytes.
// It never fails a write: tailscaled is never held up by its log, which is
// lost instead.
type cappedLog struct {
	path string
	max  int64
	f    *os.File
	n    int64
}

func (c *cappedLog) Write(p []byte) (int, error) {
	if c.f == nil || c.n >= c.max {
		if c.f != nil {
			_ = c.f.Close()
			_ = os.Rename(c.path, c.path+".1")
		}
		c.f, c.n = nil, 0
		if f, err := os.OpenFile(c.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600); err == nil {
			c.f = f
		}
	}
	if c.f != nil {
		n, _ := c.f.Write(p)
		c.n += int64(n)
	}
	return len(p), nil
}

// WithPath returns env with BinDir at the end of its PATH, or of the
// default PATH where it sets none, so a process finds the CLI after its
// image's own programs.
func WithPath(env []string) []string {
	path := supervise.DefaultPath
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
			continue
		}
		out = append(out, kv)
	}
	if !slices.Contains(filepath.SplitList(path), BinDir) {
		path += ":" + BinDir
	}
	return append(out, "PATH="+path)
}
