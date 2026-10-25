package chvm

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// A TPM VM (VMSpec.TPM) gets swtpm from Hangar's bundle (deploy/swtpm), run
// by the launcher in the VM's unit, its state encrypted with the VM's key.
// See docs/decisions/vm-tpm.md and docs/runbook/swtpm.md.

// DefaultTPMKeys is where the agent leaves each TPM VM's key, a file named
// by its machine uid, root's alone.
const DefaultTPMKeys = "/run/hangar/vtpm"

const (
	// tpmCredential is the key's name among the unit's credentials.
	tpmCredential = "vtpm"
	// tpmKeySize is the key's size: swtpm's aes-256-cbc takes 32 bytes.
	tpmKeySize = 32
	// swtpmLoader and swtpmBinary are the bundle's loader and swtpm.
	swtpmLoader = "lib/ld-musl-x86_64.so.1"
	swtpmBinary = "bin/swtpm"
	// tpmTasks are the unit's tasks for swtpm, past the VM's: its own
	// thread, and vmm-exec's Go runtime until it execs.
	tpmTasks = 8
	// tpmMemory is the unit's memory for swtpm, which holds about 4 MiB.
	tpmMemory = 16 << 20
	// unitNoFileSoft is a TPM VM's unit's soft RLIMIT_NOFILE, systemd's
	// default: the hard one is nr_open, so that vmm-exec, without
	// CAP_SYS_RESOURCE, can give swtpm alone that many.
	unitNoFileSoft = 1024
	// maxSwtpmLogLine bounds a line of swtpm's log the launcher logs.
	maxSwtpmLogLine = 4 << 10
)

// checkSwtpm checks a bundle has swtpm, and a loader a VM's unit can run.
func checkSwtpm(dir string, dynamicOK bool) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return fmt.Errorf("the swtpm bundle %q is not a clean absolute path", dir)
	}
	for _, p := range []string{swtpmLoader, swtpmBinary} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			return fmt.Errorf("the swtpm bundle: %w", err)
		}
	}
	if dynamicOK {
		return nil
	}
	return checkStatic(filepath.Join(dir, swtpmLoader))
}

// readNrOpen reads the most fds a process may have, under procSys.
func readNrOpen(procSys string) (uint64, error) {
	b, err := os.ReadFile(filepath.Join(procSys, "fs/nr_open"))
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

// PutTPMKey writes the machine's key into TPMKeys, root's 0600, whole or
// not at all, for its unit's LoadCredential.
func (h *Hypervisor) PutTPMKey(_ context.Context, uid string, key []byte) error {
	if err := checkUID(uid); err != nil {
		return err
	}
	if len(key) != tpmKeySize {
		return fmt.Errorf("machine %s's TPM key is %d bytes, not %d", uid, len(key), tpmKeySize)
	}
	if err := os.MkdirAll(h.cfg.TPMKeys, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(h.cfg.TPMKeys, ".key-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, err = f.Write(key)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(h.cfg.TPMKeys, uid))
}

// RemoveTPMKey removes the machine's key; none is not an error.
func (h *Hypervisor) RemoveTPMKey(_ context.Context, uid string) error {
	if err := checkUID(uid); err != nil {
		return err
	}
	err := os.Remove(filepath.Join(h.cfg.TPMKeys, uid))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// TPMKeys lists the machines TPMKeys holds a key of.
func (h *Hypervisor) TPMKeys(context.Context) ([]string, error) {
	entries, err := os.ReadDir(h.cfg.TPMKeys)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.Type().IsRegular() && checkUID(e.Name()) == nil {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// checkTPMKey checks a VM's key is there for its unit to load: a regular
// file of tpmKeySize bytes. The agent writes it; LoadCredential reads it.
func checkTPMKey(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || fi.Size() != tpmKeySize {
		return fmt.Errorf("%s is not a key of %d bytes", path, tpmKeySize)
	}
	return nil
}

// tpmKey reads the VM's key from the unit's credentials.
func (l *Launcher) tpmKey() ([]byte, error) {
	dir := l.Credentials
	if dir == "" {
		dir = os.Getenv("CREDENTIALS_DIRECTORY")
	}
	if dir == "" {
		return nil, errors.New("the unit has no credentials")
	}
	f, err := os.Open(filepath.Join(dir, tpmCredential))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	key, err := io.ReadAll(io.LimitReader(f, tpmKeySize+1))
	if err != nil {
		return nil, err
	}
	if len(key) != tpmKeySize {
		return nil, fmt.Errorf("the key is %d bytes, not %d", len(key), tpmKeySize)
	}
	return key, nil
}

// ownTPMDir makes the VM's TPM state its user's, uid, for this run: the
// directory, made if need be, root's 0700 first, as the launcher has no
// CAP_FOWNER to chmod what it does not own, then all in it uid's. A link
// is never followed; one in place of the directory is refused.
func ownTPMDir(path string, uid int) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		err = os.Mkdir(path, 0o700)
		if err == nil {
			fi, err = os.Lstat(path)
		}
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	if err := os.Lchown(path, 0, 0); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	return chownTree(path, uid)
}

// disownTPMDir makes a stopped VM's TPM state root's: its user goes back to
// the driver's range, and to another VM.
func disownTPMDir(path string) error {
	err := chownTree(path, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// chownTree gives the directories under path, path too, and the regular
// files of one link to uid. Anything else is skipped and keeps its owner:
// links, and files of more links, which the VM's user may have made of a
// file elsewhere, and which the next VM's user must not be given.
func chownTree(path string, uid int) error {
	return filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
		case d.Type().IsRegular():
			var st unix.Stat_t
			if err := unix.Lstat(p, &st); err != nil {
				return err
			}
			if st.Nlink != 1 {
				return nil
			}
		default:
			return nil
		}
		return os.Lchown(p, uid, uid)
	})
}

// swtpmProc is a TPM VM's swtpm as its launcher runs it.
type swtpmProc struct {
	cmd *exec.Cmd
	// logged ends once the launcher has read swtpm's log to its end.
	logged, exited chan struct{}
	stopping       chan struct{}
}

// startTPM runs the VM's swtpm, as the VM's user through Exec, before Cloud
// Hypervisor: it reads the key from the unit's credentials, makes the TPM
// state the VM's user's, listens on the control socket, root's and the
// VM's group's, and hands swtpm the socket, the key on a pipe it reads once,
// and a pipe for its log, on fds 3, 4 and 5. Never fd 2 for the log: swtpm
// closes its stderr when given a log fd, and then fails writing to it.
func (l *Launcher) startTPM(ln launch, dir string) (*swtpmProc, error) {
	key, err := l.tpmKey()
	if err != nil {
		return nil, fmt.Errorf("reading the key: %w", err)
	}
	uid := int(ln.VMUser)
	state := filepath.Join(dir, tpmDir)
	if err := l.OwnTPM(state, uid); err != nil {
		return nil, fmt.Errorf("the TPM's state: %w", err)
	}
	path := filepath.Join(dir, tpmSocket)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	lis, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	lis.SetUnlinkOnClose(false)
	ctrl, err := lis.File()
	_ = lis.Close()
	if err != nil {
		return nil, err
	}
	defer func() { _ = ctrl.Close() }()
	// Root's and the VM's group's: Cloud Hypervisor connects, and can
	// neither remove nor replace it.
	if uid != 0 {
		if err := os.Chown(path, 0, uid); err != nil {
			return nil, fmt.Errorf("the TPM's socket: %w", err)
		}
	}
	if err := os.Chmod(path, 0o660); err != nil {
		return nil, fmt.Errorf("the TPM's socket: %w", err)
	}
	keyR, keyW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer func() { _ = keyR.Close() }()
	// 32 bytes fit a pipe's buffer: the write never waits on swtpm.
	_, err = keyW.Write(key)
	clear(key)
	if cerr := keyW.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	logR, logW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer func() { _ = logW.Close() }()

	cmd := l.swtpmCommand(ln, state)
	cmd.ExtraFiles = []*os.File{ctrl, keyR, logW}
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		_ = logR.Close()
		return nil, err
	}
	p := &swtpmProc{cmd: cmd, logged: make(chan struct{}), exited: make(chan struct{}), stopping: make(chan struct{})}
	go func() {
		defer close(p.logged)
		defer func() { _ = logR.Close() }()
		l.pumpSwtpmLog(logR)
	}()
	go func() {
		err := cmd.Wait()
		select {
		case <-p.stopping:
		default:
			l.Log.Error("vmm: swtpm ended; the guest's TPM fails until the VM is started again", "error", errString(err))
		}
		close(p.exited)
	}()
	return p, nil
}

// swtpmCommand runs the bundle's swtpm on the VM's state, under its own
// loader, as the VM's user with the most fds the host allows, as command
// runs Cloud Hypervisor: never --terminate, whose swtpm ends with Cloud
// Hypervisor's first connection, which a guest's reboot ends.
func (l *Launcher) swtpmCommand(ln launch, state string) *exec.Cmd {
	args := []string{
		"--library-path", filepath.Join(ln.Swtpm, "lib"), filepath.Join(ln.Swtpm, swtpmBinary),
		"socket", "--tpm2", "--tpmstate", "dir=" + state,
		"--ctrl", "type=unixio,fd=3",
		"--key", "fd=4,format=binary,mode=aes-256-cbc",
		// A wrong key fails swtpm as it starts, not Cloud Hypervisor's boot.
		"--flags", "not-need-init",
		"--log", "fd=5,level=1",
	}
	loader := filepath.Join(ln.Swtpm, swtpmLoader)
	if ln.VMUser == 0 {
		return exec.Command(loader, args...)
	}
	id := fmt.Sprint(ln.VMUser)
	argv := append(slices.Clone(l.Exec[1:]), "--uid", id, "--gid", id, "--fds", "3")
	if ln.NoFile > 0 {
		argv = append(argv, "--nofile", fmt.Sprint(ln.NoFile))
	}
	argv = append(argv, "--", loader)
	return exec.Command(l.Exec[0], append(argv, args...)...)
}

// pumpSwtpmLog logs swtpm's log, a line at a time, each cut at
// maxSwtpmLogLine.
func (l *Launcher) pumpSwtpmLog(r io.Reader) {
	br := bufio.NewReaderSize(r, maxSwtpmLogLine)
	for {
		line, err := br.ReadSlice('\n')
		if len(line) > 0 {
			l.Log.Info("vmm: swtpm", "log", strings.TrimRight(string(line), "\n"))
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return
		}
	}
}

// stop ends swtpm once Cloud Hypervisor has exited: SIGTERM, which it
// saves its state on, then SIGKILL past grace.
func (p *swtpmProc) stop(grace time.Duration) {
	close(p.stopping)
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.exited:
	case <-time.After(grace):
		_ = p.cmd.Process.Kill()
		<-p.exited
	}
	<-p.logged
}
