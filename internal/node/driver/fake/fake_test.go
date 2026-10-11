package fake_test

import (
	"errors"
	"fmt"
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
			Publish: func(_ *testing.T, ref string) string {
				published++
				digest := fmt.Sprintf("sha256:%d", published)
				h.AddImage(ref, digest, driver.ImageConfig{Cmd: []string{"true"}})
				return digest
			},
			Withdraw: func(_ *testing.T, ref string) { h.FailPull(ref, errors.New("registry down")) },
			Exit: func(t *testing.T, machineUID string, code int) {
				if err := h.Exit(machineUID, code); err != nil {
					t.Fatal(err)
				}
			},
		}
	})
}
