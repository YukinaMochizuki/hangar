// Package chvm is the Hypervisor driver for Cloud Hypervisor: each VM runs
// as a transient systemd unit, hangar-vm-<machine uid>.service, whose main
// process is the launcher (hangar-node vmm, Launcher), so the VM outlives
// the node agent and systemd restarts it as its restart policy says. Each
// VM has a directory, Dir/<machine uid>, with what the launcher runs (the
// spec file and the boot disk), its sockets, the record of how its last
// run ended, and its console logs. The driver keeps nothing in memory: it
// reads the units and the directories back. See
// docs/decisions/node-agent.md.
package chvm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/internal/guest/bootdisk"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chguest"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/keylock"
)

// DefaultDir is where the VMs' directories are.
const DefaultDir = "/var/lib/hangar/vms"

// Config configures the driver.
type Config struct {
	// Dir holds a directory per VM. The driver owns it, and only it.
	Dir string
	// CloudHypervisor, Kernel and Initramfs are the paths of Cloud
	// Hypervisor and of the platform's guest kernel and initramfs; Cmdline
	// is the kernel's command line. A VM keeps those it was started with
	// until it is started again.
	CloudHypervisor, Kernel, Initramfs, Cmdline string
	// Launcher is the launcher's command line, to which the driver adds
	// --dir, --vm-user and the machine uid: hangar-node vmm. The launcher
	// and Cloud Hypervisor must be statically linked, at absolute paths:
	// each VM's unit has an empty root, into which only they, the kernel
	// and the initramfs are bound.
	Launcher []string
	// VMUsers are the ids the VMs run as, one each; zero means
	// DefaultVMUsers. No user or group of the host may have one, nor any
	// subordinate range overlap them, but deploy/host's for the VMs.
	VMUsers UserRange
	// StopGrace is how long a guest's process has between SIGTERM and
	// SIGKILL; zero means driver.StopGrace.
	StopGrace time.Duration
	// StopSlack is how much longer than StopGrace a guest has to power off
	// before its VM is killed; zero means DefaultStopSlack. A VM keeps
	// both as it was started with: its launcher gives its guest the same
	// when systemd stops it, as at host shutdown.
	StopSlack time.Duration

	// systemd and guest, if set, replace systemd over D-Bus and the
	// guests' agents, for tests.
	systemd systemd
	guest   driver.Guest
	// logPoll, if set, is how often a followed log is checked for more.
	logPoll time.Duration
	// procUsers, if set, replaces listing the ids processes run as.
	procUsers func() (map[uint32]bool, error)
	// dynamicOK lets the launcher and Cloud Hypervisor be dynamically
	// linked, for tests whose units are fakes.
	dynamicOK bool
	// etc, if set, replaces /etc, where the host's users and groups are.
	etc string
	// removeAll, if set, replaces os.RemoveAll in removeRuntime.
	removeAll func(string) error
}

// Hypervisor is the driver. It is safe for concurrent use.
type Hypervisor struct {
	cfg     Config
	sd      systemd
	guest   driver.Guest
	locks   keylock.Map
	changes chan struct{}
	// users is held while a Start picks its VM's id and writes it down.
	users     sync.Mutex
	procUsers func() (map[uint32]bool, error)
	removeAll func(string) error
}

var _ driver.Hypervisor = (*Hypervisor)(nil)

// New connects to systemd and makes Dir if need be. Close releases it.
func New(ctx context.Context, cfg Config) (*Hypervisor, error) {
	if cfg.StopGrace == 0 {
		cfg.StopGrace = driver.StopGrace
	}
	if cfg.StopSlack == 0 {
		cfg.StopSlack = DefaultStopSlack
	}
	if !filepath.IsAbs(cfg.Dir) || filepath.Clean(cfg.Dir) != cfg.Dir {
		return nil, fmt.Errorf("the VMs' directory %q is not a clean absolute path", cfg.Dir)
	}
	// The vsock socket's path must fit a unix address, 107 bytes.
	if n := len(filepath.Join(cfg.Dir, strings.Repeat("x", maxUID), vsockFile)); n > 107 {
		return nil, fmt.Errorf("the VMs' directory %q is too long for their sockets", cfg.Dir)
	}
	if len(cfg.Launcher) == 0 {
		return nil, errors.New("no launcher")
	}
	if cfg.VMUsers == (UserRange{}) {
		cfg.VMUsers = DefaultVMUsers
	}
	if err := cfg.VMUsers.check(); err != nil {
		return nil, err
	}
	if cfg.etc == "" {
		cfg.etc = "/etc"
	}
	if err := cfg.VMUsers.checkFree(cfg.etc); err != nil {
		return nil, err
	}
	for _, p := range []string{cfg.Launcher[0], cfg.CloudHypervisor, cfg.Kernel, cfg.Initramfs} {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("%q is not an absolute path", p)
		}
		if _, err := os.Stat(p); err != nil {
			return nil, err
		}
	}
	if !cfg.dynamicOK {
		for _, p := range []string{cfg.Launcher[0], cfg.CloudHypervisor} {
			if err := checkStatic(p); err != nil {
				return nil, err
			}
		}
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, err
	}
	h := &Hypervisor{cfg: cfg, changes: make(chan struct{}, 1), procUsers: cfg.procUsers}
	if h.procUsers == nil {
		h.procUsers = procUsers
	}
	h.removeAll = cfg.removeAll
	if h.removeAll == nil {
		h.removeAll = os.RemoveAll
	}
	h.guest = cfg.guest
	if h.guest == nil {
		h.guest = chguest.Guest{Socket: h.Socket}
	}
	h.sd = cfg.systemd
	if h.sd == nil {
		sd, err := dialSystemd(ctx, h.signal)
		if err != nil {
			return nil, fmt.Errorf("systemd: %w", err)
		}
		h.sd = sd
	}
	return h, nil
}

// Close releases the connection to systemd. The VMs run on.
func (h *Hypervisor) Close() error {
	h.sd.close()
	return nil
}

// Guest is the guest driver for the VMs this driver starts.
func (h *Hypervisor) Guest() driver.Guest { return h.guest }

// Socket is the hybrid vsock socket of a machine's VM.
func (h *Hypervisor) Socket(machineUID string) string {
	if legacy := filepath.Join(h.cfg.Dir, machineUID, legacyVsockFile); isSocket(legacy) {
		return legacy
	}
	return filepath.Join(h.cfg.Dir, machineUID, vsockFile)
}

// LogPath is where a machine's console log is.
func (h *Hypervisor) LogPath(machineUID string) string {
	return filepath.Join(h.cfg.Dir, machineUID, ConsoleLog)
}

func (h *Hypervisor) dir(uid string) string { return filepath.Join(h.cfg.Dir, uid) }

func (h *Hypervisor) signal() {
	select {
	case h.changes <- struct{}{}:
	default:
	}
}

func (h *Hypervisor) Changes() <-chan struct{} { return h.changes }

// restartOf is systemd's Restart= for a policy.
func restartOf(p driver.RestartPolicy) (string, error) {
	switch p {
	case driver.RestartAlways:
		return "always", nil
	case driver.RestartOnFailure:
		return "on-failure", nil
	case driver.RestartNever:
		return "no", nil
	}
	return "", fmt.Errorf("no restart policy %d", p)
}

// running reports a unit whose VM runs, or is about to again: a unit
// waiting to restart is activating.
func running(u unitState) bool {
	switch u.Active {
	case "active", "activating", "deactivating", "reloading", "refreshing":
		return true
	}
	return false
}

// vm reads back the machine's VM: its unit, if any, and its directory.
func (h *Hypervisor) vm(uid string, unit *unitState) (driver.VM, bool, error) {
	dir := h.dir(uid)
	ln, err := readLaunch(dir)
	hasSpec := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return driver.VM{}, false, err
	}
	rec, exited, err := readExit(dir)
	if err != nil {
		return driver.VM{}, false, err
	}
	vm := driver.VM{MachineUID: uid, SpecHash: ln.Hash, Run: ln.Spec.Run}
	// A guest that powered off while it was being stopped, its launcher
	// waiting for the stop, has exited.
	offInStop := exited && exists(filepath.Join(dir, stopFile))
	switch {
	case unit != nil && running(*unit) && !offInStop:
		vm.State, vm.Started = driver.VMRunning, unit.Started
	case hasSpec && exited:
		vm.State, vm.ExitCode, vm.Started = driver.VMExited, rec.Code, rec.Started
	case hasSpec && ln.Spec.Restart == driver.RestartNever && !exists(filepath.Join(dir, stopFile)):
		// Interrupted: its unit gone with no record, as by the host
		// rebooting or its launcher killed outright, and not by a stop.
		// Its policy keeps it down, as if it had crashed.
		vm.State, vm.ExitCode = driver.VMExited, ExitNoRecord
	default:
		// Not started, stopped, or interrupted under a policy that
		// starts it again: the agent starts it again.
		return driver.VM{}, false, nil
	}
	return vm, true, nil
}

func (h *Hypervisor) VMs(ctx context.Context) ([]driver.VM, error) {
	units, err := h.sd.list(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing units: %w", err)
	}
	byUID := map[string]*unitState{}
	for i := range units {
		if uid, ok := uidOfUnit(units[i].Name); ok {
			byUID[uid] = &units[i]
		}
	}
	entries, err := os.ReadDir(h.cfg.Dir)
	if err != nil {
		return nil, err
	}
	uids := map[string]bool{}
	for uid := range byUID {
		uids[uid] = true
	}
	for _, e := range entries {
		if e.IsDir() && checkUID(e.Name()) == nil {
			uids[e.Name()] = true
		}
	}
	var out []driver.VM
	for _, uid := range slices.Sorted(func(yield func(string) bool) {
		for u := range uids {
			if !yield(u) {
				return
			}
		}
	}) {
		vm, ok, err := h.vm(uid, byUID[uid])
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, vm)
		}
	}
	return out, nil
}

// unit returns the machine's unit, nil if systemd has none.
func (h *Hypervisor) unit(ctx context.Context, uid string) (*unitState, error) {
	units, err := h.sd.list(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing units: %w", err)
	}
	for _, u := range units {
		if u.Name == unitName(uid) {
			return &u, nil
		}
	}
	return nil, nil
}

func (h *Hypervisor) Start(ctx context.Context, spec driver.VMSpec) error {
	uid := spec.MachineUID
	if err := checkSpec(spec); err != nil {
		return err
	}
	restart, err := restartOf(spec.Restart)
	if err != nil {
		return err
	}
	unlock := h.locks.Lock(uid)
	defer unlock()
	unit, err := h.unit(ctx, uid)
	if err != nil {
		return err
	}
	vm, listed, err := h.vm(uid, unit)
	if err != nil {
		return err
	}
	hash := spec.Hash()
	if listed && vm.SpecHash != hash {
		return fmt.Errorf("machine %s's VM runs another spec; stop it first", uid)
	}
	if listed && vm.State == driver.VMRunning {
		// A stop cut short may have left its mark on the VM, which is
		// wanted running after all: its next exit is the restart
		// policy's again.
		if err := os.Remove(filepath.Join(h.dir(uid), stopFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if unit != nil && running(*unit) {
		// Its launcher waits to be stopped, or it is not the driver's.
		return fmt.Errorf("machine %s's unit %s is %s; stop it first", uid, unit.Name, unit.Active)
	}
	if unit != nil {
		if err := h.sd.resetFailed(ctx, unit.Name); err != nil {
			return err
		}
	}

	dir := h.dir(uid)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, f := range []string{exitFile, stopFile, legacyVsockFile} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.RemoveAll(filepath.Join(dir, chDir)); err != nil {
		return err
	}
	disk, err := h.bootDisk(spec)
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, bootFile), disk); err != nil {
		return err
	}
	user, err := h.writeLaunch(dir, launch{
		Spec: spec, Hash: hash, CloudHypervisor: h.cfg.CloudHypervisor,
		Kernel: h.cfg.Kernel, Initramfs: h.cfg.Initramfs, Cmdline: h.cfg.Cmdline,
		StopGrace: h.cfg.StopGrace, StopSlack: h.cfg.StopSlack,
	})
	if err != nil {
		return err
	}
	argv := append(slices.Clone(h.cfg.Launcher), "--dir", h.cfg.Dir, "--vm-user", fmt.Sprint(user), uid)
	sb := sandbox{Dir: dir, Binds: []string{h.cfg.Launcher[0], h.cfg.CloudHypervisor, h.cfg.Kernel, h.cfg.Initramfs}}
	if err := h.sd.start(ctx, unitName(uid), unitProps(spec, argv, restart, stopTimeout(h.cfg.StopGrace, h.cfg.StopSlack), sb)); err != nil {
		return fmt.Errorf("starting %s: %w", unitName(uid), err)
	}
	return nil
}

// writeLaunch writes the spec file, with the VM's id, and returns the id.
func (h *Hypervisor) writeLaunch(dir string, ln launch) (uint32, error) {
	h.users.Lock()
	defer h.users.Unlock()
	// And against another driver on the same directory.
	lock, err := os.Open(h.cfg.Dir)
	if err != nil {
		return 0, err
	}
	defer func() { _ = lock.Close() }()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return 0, err
	}
	user, err := h.vmUser(ln.Spec.MachineUID)
	if err != nil {
		return 0, err
	}
	ln.VMUser = user
	b, err := json.Marshal(ln)
	if err != nil {
		return 0, err
	}
	return user, writeAtomic(filepath.Join(dir, specFile), b)
}

// checkSpec refuses a spec no VM can be started with.
func checkSpec(s driver.VMSpec) error {
	if err := checkUID(s.MachineUID); err != nil {
		return err
	}
	if s.VCPUs < 1 || s.MemoryMiB < 1 {
		return fmt.Errorf("machine %s: %d vCPUs and %d MiB of memory", s.MachineUID, s.VCPUs, s.MemoryMiB)
	}
	if s.Root.MachineUID != s.MachineUID || s.Root.Path == "" {
		return fmt.Errorf("machine %s: root disk %+v is not its", s.MachineUID, s.Root)
	}
	for _, i := range s.Interfaces {
		if i.MachineUID != s.MachineUID || i.Device == "" {
			return fmt.Errorf("machine %s: interface %+v is not its, or has no device", s.MachineUID, i)
		}
	}
	return nil
}

// bootDisk is the boot disk of spec: what to run, the network, the
// hostname, the resolvers.
func (h *Hypervisor) bootDisk(spec driver.VMSpec) ([]byte, error) {
	cfg := &guestv1.BootConfig{
		Process: &guestv1.Process{
			Args: spec.Run.Command, Env: spec.Run.Env, WorkingDir: spec.Run.WorkingDir, User: spec.Run.User,
		},
		Hostname:    hostname(spec),
		Nameservers: spec.Nameservers,
		Search:      spec.Search,
	}
	for _, i := range spec.Interfaces {
		gw := ""
		if i.Gateway.IsValid() {
			gw = i.Gateway.String()
		}
		cfg.Interfaces = append(cfg.Interfaces, &guestv1.Interface{Mac: i.MAC, Address: i.Address.String(), Gateway: gw})
	}
	var buf bytes.Buffer
	if err := bootdisk.Write(&buf, cfg); err != nil {
		return nil, err
	}
	// A disk is whole sectors; the reader takes the length from the header.
	if r := buf.Len() % 512; r != 0 {
		buf.Write(make([]byte, 512-r))
	}
	return buf.Bytes(), nil
}

// hostname is the machine's name, as a hostname takes it: the name after
// its project, in lower case, with what a hostname cannot hold made dashes,
// and at most 63 characters; the uid's first 12 if nothing is left.
func hostname(spec driver.VMSpec) string {
	name := spec.Machine
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	if h := asHostname(name, 63); h != "" {
		return h
	}
	return asHostname(spec.MachineUID, 12)
}

func asHostname(s string, n int) string {
	b := []byte(strings.ToLower(s))
	for i, c := range b {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			b[i] = '-'
		}
	}
	return strings.Trim(string(b[:min(len(b), n)]), "-")
}

func (h *Hypervisor) Stop(ctx context.Context, uid string) error {
	if err := checkUID(uid); err != nil {
		return err
	}
	unlock := h.locks.Lock(uid)
	defer unlock()
	unit, err := h.unit(ctx, uid)
	if err != nil {
		return err
	}
	dir := h.dir(uid)
	if unit != nil && running(*unit) {
		if exists(dir) {
			// From here on the launcher waits for the unit's stop once the
			// guest is off: it never exits for systemd to restart it.
			if err := os.WriteFile(filepath.Join(dir, stopFile), nil, 0o600); err != nil {
				return err
			}
		}
		h.stopGuest(ctx, uid)
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := h.sd.stop(ctx, unit.Name); err != nil {
			return fmt.Errorf("stopping %s: %w", unit.Name, err)
		}
	}
	if unit != nil {
		if err := h.sd.resetFailed(ctx, unit.Name); err != nil {
			return err
		}
	}
	return h.removeRuntime(dir)
}

// stopGuest asks the guest to stop its process and waits for the guest to
// power off, for the grace period and the slack; it returns at once if the
// guest does not take the stop.
func (h *Hypervisor) stopGuest(ctx context.Context, uid string) {
	gctx, cancel := context.WithTimeout(ctx, stopAsk)
	err := h.guest.Stop(gctx, uid, h.cfg.StopGrace)
	cancel()
	if err != nil {
		return
	}
	deadline := time.NewTimer(h.cfg.StopGrace + h.cfg.StopSlack)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for !h.poweredOff(ctx, uid) {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-tick.C:
		}
	}
}

// poweredOff reports a VM whose guest is off: its run is recorded, or its
// unit is gone.
func (h *Hypervisor) poweredOff(ctx context.Context, uid string) bool {
	if exists(filepath.Join(h.dir(uid), exitFile)) {
		return true
	}
	unit, err := h.unit(ctx, uid)
	return err == nil && (unit == nil || !running(*unit))
}

// removeRuntime removes what a VM's directory holds but its logs, and the
// directory too if no log is left. The spec file goes last: while anything
// else of the VM's is left, its id is not given to another VM.
func (h *Hypervisor) removeRuntime(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	kept := 0
	for _, e := range entries {
		if slices.Contains(logFiles, e.Name()) {
			kept++
			continue
		}
		if e.Name() == specFile {
			continue
		}
		if err := h.removeAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	if err := os.Remove(filepath.Join(dir, specFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if kept == 0 {
		return os.Remove(dir)
	}
	return nil
}

func (h *Hypervisor) Logs(context.Context) ([]string, error) {
	entries, err := os.ReadDir(h.cfg.Dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || checkUID(e.Name()) != nil {
			continue
		}
		if slices.ContainsFunc(logFiles, func(f string) bool { return exists(filepath.Join(h.cfg.Dir, e.Name(), f)) }) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

func (h *Hypervisor) DeleteLogs(ctx context.Context, uid string) error {
	if err := checkUID(uid); err != nil {
		return err
	}
	unlock := h.locks.Lock(uid)
	defer unlock()
	unit, err := h.unit(ctx, uid)
	if err != nil {
		return err
	}
	if _, listed, err := h.vm(uid, unit); err != nil || listed {
		if err == nil {
			err = fmt.Errorf("machine %s's VM is listed; stop it first", uid)
		}
		return err
	}
	if unit != nil && running(*unit) {
		return fmt.Errorf("machine %s's unit %s is %s; stop it first", uid, unit.Name, unit.Active)
	}
	return os.RemoveAll(h.dir(uid))
}
