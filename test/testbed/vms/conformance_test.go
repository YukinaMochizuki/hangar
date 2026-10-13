//go:build testbed

package vms

import (
	"bufio"
	"errors"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chvm"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/drivertest"
)

// TestConformance holds every real driver to the drivers' contract
// together, the hypervisor's parts with real VMs under systemd.
func TestConformance(t *testing.T) {
	drivertest.Run(t, func(t *testing.T) drivertest.Host {
		h := newHost(t)
		return drivertest.Host{
			Images: h.images, Storage: h.storage, Network: h.net, Hypervisor: h.hv, Guest: h.hv.Guest(),
			Publish: h.publish,
			Withdraw: func(_ *testing.T, ref string) {
				h.reg.Withdraw(strings.SplitN(strings.TrimPrefix(ref, h.reg.Host+"/"), ":", 2)[0])
			},
			Reopen: func(t *testing.T) (driver.Images, driver.Storage) { return h.reopen(t) },
			Hold: func(t *testing.T, d driver.Disk) func() {
				f, err := os.OpenFile(d.Path, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				return func() { _ = f.Close() }
			},
			HoldVolume: func(t *testing.T, v driver.Volume) func() {
				f, err := os.OpenFile(v.Path, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				return func() { _ = f.Close() }
			},
			Exit:             exit,
			Run:              driver.RunSpec{Command: []string{"/bin/sh", "-c", guestScript}},
			Exec:             &execPrograms,
			ReopenHypervisor: func(t *testing.T) driver.Hypervisor { return newHypervisor(t, testGrace) },
			AskedToStop: func(t *testing.T, uid string) bool {
				for _, r := range consoleOf(t, uid) {
					if r.Stream == console.Init && strings.Contains(string(r.Payload), "hangar-init: stopping the container") {
						return true
					}
				}
				return false
			},
		}
	})
}

func testNet() driver.NetworkSpec {
	return driver.NetworkSpec{UID: "tbvms", CIDR: mustPrefix("10.0.0.0/24"), Transit: mustPrefix("100.64.0.0/31")}
}

func mustPrefix(s string) netip.Prefix { return netip.MustParsePrefix(s) }
func mustAddr(s string) netip.Addr     { return netip.MustParseAddr(s) }

// consoleOf reads every record of the machine's console log, the rotated
// file first, failing on what is not a whole record.
func consoleOf(t *testing.T, uid string) []console.Record {
	t.Helper()
	var out []console.Record
	for _, f := range []string{chvm.ConsoleLog + ".1", chvm.ConsoleLog} {
		fh, err := os.Open(filepath.Join(vmDir, uid, f))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		r := console.NewReader(bufio.NewReader(fh))
		for {
			rec, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Errorf("%s: %v after %d records", f, err, len(out))
				break
			}
			out = append(out, rec)
		}
		_ = fh.Close()
	}
	return out
}

// stdoutOf is what the machine's processes wrote to stdout, over its runs.
func stdoutOf(t *testing.T, uid string) string {
	t.Helper()
	var b strings.Builder
	for _, r := range consoleOf(t, uid) {
		if r.Stream == console.Stdout {
			b.Write(r.Payload)
		}
	}
	return b.String()
}

// lastExit is the last exit the machine's guest recorded on its console.
func lastExit(t *testing.T, uid string) (console.ExitStatus, bool) {
	t.Helper()
	var st console.ExitStatus
	found := false
	for _, r := range consoleOf(t, uid) {
		if r.Stream == console.Exit {
			if s, err := console.ExitOf(r); err == nil {
				st, found = s, true
			}
		}
	}
	return st, found
}

// execPrograms are the exec part's programs in the guests' busybox.
var execPrograms = drivertest.ExecPrograms{
	Output: func(stdout, stderr string, code int) []string {
		return []string{"/bin/sh", "-c", `printf %s "$1"; printf %s "$2" >&2; exit "$3"`, "sh", stdout, stderr, strconv.Itoa(code)}
	},
	Cat:  []string{"/bin/cat"},
	Size: []string{"/bin/sh", "-c", "stty size; read -r line; stty size"},
	Show: []string{"/bin/sh", "-c", `echo "$HANGAR_EXEC"; pwd; id -u`},
	Wait: func(marker string) []string {
		return []string{"/bin/sh", "-c", `sleep 1000 & echo $! > "/tmp/hangar-wait-$1"; wait`, "sh", marker}
	},
	Running: func(marker string) []string {
		return []string{"/bin/sh", "-c", `kill -0 "$(cat "/tmp/hangar-wait-$1")" 2>/dev/null && echo 1 || echo 0`, "sh", marker}
	},
}
