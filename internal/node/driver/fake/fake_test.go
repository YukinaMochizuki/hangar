package fake_test

import (
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/drivertest"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
)

// TestConformance holds the fake to the drivers' contract, which the real
// drivers are held to on the testbed.
func TestConformance(t *testing.T) {
	drivertest.Run(t, func(t *testing.T) drivertest.Host {
		h := fake.New()
		published := 0
		return drivertest.Host{
			Images: h.Images(), Storage: h.Storage(), Network: h.Network(), Hypervisor: h.Hypervisor(), Guest: h.Guest(),
			Publish: func(_ *testing.T, name string) (string, string) {
				published++
				digest := fmt.Sprintf("sha256:%d", published)
				h.AddImage(name, digest, driver.ImageConfig{Cmd: []string{"true"}})
				return name, digest
			},
			Withdraw:         func(_ *testing.T, ref string) { h.FailPull(ref, errors.New("registry down")) },
			Reopen:           func(*testing.T) (driver.Images, driver.Storage) { return h.Images(), h.Storage() },
			Hold:             func(_ *testing.T, d driver.Disk) func() { return h.HoldDisk(d.MachineUID) },
			ReopenHypervisor: func(*testing.T) driver.Hypervisor { return h.Hypervisor() },
			AskedToStop:      func(_ *testing.T, uid string) bool { return h.GuestStops(uid) > 0 },
			Exit: func(t *testing.T, machineUID string, code int) {
				if err := h.Exit(machineUID, code); err != nil {
					t.Fatal(err)
				}
			},
			Exec: &drivertest.ExecPrograms{
				Output: func(stdout, stderr string, code int) []string {
					return []string{"exit", strconv.Itoa(code), stdout, stderr}
				},
				Cat:     []string{"cat"},
				Size:    []string{"size"},
				Show:    []string{"show", "HANGAR_EXEC"},
				Wait:    func(marker string) []string { return []string{"wait", marker} },
				Running: func(marker string) []string { return []string{"running", marker} },
			},
		}
	})
}
