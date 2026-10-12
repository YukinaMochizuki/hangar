package chvm

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// What a VM's directory, Dir/<machine uid>, holds. The logs outlive the VM;
// the rest is the running VM's, and goes with it.
const (
	// specFile is the launch: what the launcher starts, written by Start.
	specFile = "spec.json"
	// bootFile is the boot disk (internal/guest/bootdisk).
	bootFile = "boot.img"
	// vsockFile is the hybrid vsock socket Cloud Hypervisor listens on.
	vsockFile = "vsock"
	// exitFile records how the VM's last run ended, written by the
	// launcher when Cloud Hypervisor exits by itself.
	exitFile = "exit.json"
	// stopFile marks a VM being stopped: once its guest powers off, the
	// launcher waits to be stopped instead of exiting, so the restart
	// policy never starts it again.
	stopFile = "stopping"
	// serialFIFO carries the guest's serial console from Cloud Hypervisor
	// to the launcher.
	serialFIFO = "serial.fifo"

	// ConsoleLog is the guest's framed console (internal/guest/console):
	// what the container wrote, and how it ended. It is rotated to
	// ConsoleLog.1.
	ConsoleLog = "console.log"
	// SerialLog is the guest kernel's console, rotated to SerialLog.1.
	SerialLog = "serial.log"
)

// logFiles are what Stop keeps of a VM's directory.
var logFiles = []string{ConsoleLog, ConsoleLog + ".1", SerialLog, SerialLog + ".1"}

// Exit codes the launcher ends with when it has no exit code of the
// container's to end with. Either is a failure to the restart policy. A
// container may exit with either itself, which reads the same: the exit
// record's Error tells ExitFailed apart.
const (
	// ExitNoRecord means Cloud Hypervisor ended without the guest having
	// recorded how its process ended: the VM crashed, or was killed.
	ExitNoRecord = 253
	// ExitFailed means the launcher could not start the VM, such as for a
	// root disk that is not the machine's.
	ExitFailed = 252
)

// launch is the spec file: the VM and what the launcher starts it with.
type launch struct {
	Spec driver.VMSpec
	// Hash is Spec.Hash() when it was written.
	Hash            string
	CloudHypervisor string
	Kernel          string
	Initramfs       string
	Cmdline         string
}

// exitRecord is the exit file.
type exitRecord struct {
	// Code is what the launcher exited with: the container's exit code, or
	// ExitNoRecord or ExitFailed.
	Code int
	// Signal is the signal that ended the container's process, if the
	// guest recorded one.
	Signal int `json:",omitempty"`
	// Error says why the launcher could not start the VM.
	Error string `json:",omitempty"`
	// Started and Ended bound the run.
	Started, Ended time.Time
}

// unitPrefix starts the name of every unit the driver makes, and of no
// other: a unit outside it is never touched.
const unitPrefix = "hangar-vm-"

func unitName(uid string) string { return unitPrefix + uid + ".service" }

// uidOfUnit returns the machine uid a unit's name is for, if it is one of
// the driver's names.
func uidOfUnit(name string) (string, bool) {
	uid, ok := strings.CutPrefix(name, unitPrefix)
	if !ok {
		return "", false
	}
	uid, ok = strings.CutSuffix(uid, ".service")
	return uid, ok && checkUID(uid) == nil
}

// uidPattern is what names a VM: a unit name and a directory name, short
// enough that the vsock socket's path fits a unix address.
var uidPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// maxUID is the longest uid uidPattern takes.
const maxUID = 64

func checkUID(uid string) error {
	if !uidPattern.MatchString(uid) {
		return fmt.Errorf("machine uid %q cannot name a VM", uid)
	}
	return nil
}

// writeAtomic writes a file whole or not at all.
func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}

func readLaunch(dir string) (launch, error) {
	var l launch
	b, err := os.ReadFile(filepath.Join(dir, specFile))
	if err != nil {
		return l, err
	}
	if err := json.Unmarshal(b, &l); err != nil {
		return l, fmt.Errorf("%s: %w", filepath.Join(dir, specFile), err)
	}
	return l, nil
}

// readExit returns the exit record, if there is one.
func readExit(dir string) (exitRecord, bool, error) {
	var r exitRecord
	b, err := os.ReadFile(filepath.Join(dir, exitFile))
	if errors.Is(err, fs.ErrNotExist) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, false, fmt.Errorf("%s: %w", filepath.Join(dir, exitFile), err)
	}
	return r, true, nil
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
