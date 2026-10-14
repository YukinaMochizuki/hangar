//go:build testbed

package disks

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestEveryZvolIsMadeAsDev: the driver's parent datasets carry
// volmode=dev, given on a host whose parent existed without it, so each
// zvol's device is made once, as dev: a disk image's base, and volumes
// cloned from it and grown at once, as a VM's disks are, never have their
// device removed and made again, the window in which OpenZFS 2.4.1 oopses
// on a volsize set (docs/decisions/storage-zfs.md); nor does the kernel log
// a BUG.
func TestEveryZvolIsMadeAsDev(t *testing.T) {
	h := newHost(t)
	ctx := context.Background()
	// An existing host's: a parent without volmode, a volume in it.
	mustRun(t, "zfs", "inherit", "volmode", h.dataset+"/disks")
	mustRun(t, "zfs", "create", "-s", "-V", "64M", "-o", "volmode=dev", "-o", "hangar:disk=old", h.dataset+"/disks/old")

	s := h.storage(t)
	// What giving the parent its volmode did to the volume there (OpenZFS
	// 2.2 makes its device again) is a task of ZFS's own that settle does
	// not wait for.
	time.Sleep(2 * time.Second)
	mustRun(t, "udevadm", "settle")
	bugs := kernelBugs(t)
	events := watchBlockEvents(t)
	for _, p := range []string{"bases", "roots", "disks", "vmbases"} {
		ds := h.dataset + "/" + p
		if got := strings.TrimSpace(mustRun(t, "zfs", "get", "-H", "-o", "value,source", "volmode", ds)); got != "dev\tlocal" {
			t.Errorf("%s has volmode %q, want dev, local", ds, got)
		}
	}

	digest := "sha256:" + strings.Repeat("ab", 32)
	if err := s.EnsureImageBase(ctx, digest, 64<<20, func(dev string) error {
		f, err := os.OpenFile(dev, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = f.Write(bytes.Repeat([]byte("hangar"), 1<<18))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	const n = 20
	for i := range n {
		v, err := s.EnsureVolume(ctx, fmt.Sprint("v", i), 256<<20, digest)
		if err != nil {
			t.Fatal(err)
		}
		if v.Size != 256<<20 {
			t.Errorf("v%d is %d bytes", i, v.Size)
		}
	}
	mustRun(t, "udevadm", "settle")
	time.Sleep(time.Second)
	adds, removes, lines := events()
	t.Logf("zvol devices: %d added, %d removed, for %d clones and their base", adds, removes, n)
	if removes != 0 {
		t.Errorf("%d zvol devices were removed: a device was made and then made again", removes)
	}
	if adds != n+1 {
		t.Errorf("%d zvol devices added, want %d: one each for the base and the clones", adds, n+1)
	}
	if t.Failed() {
		t.Logf("zvol events:\n%s\n%s", strings.Join(lines, "\n"), mustRun(t, "zfs", "list", "-r", "-t", "volume", "-o", "name,volmode", h.dataset))
	}
	if got := kernelBugs(t); got != bugs {
		t.Errorf("the kernel logged %d BUGs during the test", got-bugs)
	}
}

// kernelBugs counts the BUG lines in the kernel's log.
func kernelBugs(t *testing.T) int {
	t.Helper()
	return strings.Count(mustRun(t, "dmesg"), "BUG:")
}

var zvolEvent = regexp.MustCompile(`^KERNEL\[[^]]*\] (add|remove) +\S*/zd\d+ \(block\)$`)

// syncBuffer is a buffer a command writes to while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// watchBlockEvents follows the kernel's block uevents until the returned
// func, which says how many zvol devices were added and removed, and the
// events.
func watchBlockEvents(t *testing.T) func() (adds, removes int, lines []string) {
	t.Helper()
	var out syncBuffer
	cmd := exec.Command("udevadm", "monitor", "--kernel", "--subsystem-match=block")
	cmd.Stdout = &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// It prints its header once it listens.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if strings.Contains(out.String(), "KERNEL") {
			break
		}
	}
	stopped := false
	stop := func() {
		if !stopped {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			stopped = true
		}
	}
	t.Cleanup(stop)
	return func() (adds, removes int, lines []string) {
		stop()
		for _, line := range strings.Split(out.String(), "\n") {
			line = strings.TrimSpace(line)
			switch m := zvolEvent.FindStringSubmatch(line); {
			case m == nil:
				continue
			case m[1] == "add":
				adds++
			default:
				removes++
			}
			lines = append(lines, line)
		}
		return adds, removes, lines
	}
}
