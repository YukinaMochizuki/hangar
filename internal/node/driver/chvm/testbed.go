//go:build testbed

package chvm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// Testbed builds only: what the real-host tests need to reach inside the
// driver. hangar-node is never built with the testbed tag.

// BreakSystemd closes the driver's connection to systemd, as a D-Bus
// restart does; the driver connects again on its own.
func (h *Hypervisor) BreakSystemd() {
	if s, ok := h.sd.(*dbusSystemd); ok {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.conn != nil {
			s.conn.Close()
		}
	}
}

// StartUnconfined starts a VM as a driver without VM users did: its spec
// file holds none, and its unit, which runs launcher (a command line such
// as Config.Launcher), has no sandbox, so the launcher runs Cloud
// Hypervisor as root.
func (h *Hypervisor) StartUnconfined(ctx context.Context, spec driver.VMSpec, launcher []string) error {
	restart, err := restartOf(spec.Restart)
	if err != nil {
		return err
	}
	dir := h.dir(spec.MachineUID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	disk, err := h.bootDisk(spec)
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, bootFile), disk); err != nil {
		return err
	}
	b, err := json.Marshal(launch{
		Spec: spec, Hash: spec.Hash(), CloudHypervisor: h.cfg.CloudHypervisor,
		Kernel: h.cfg.Kernel, Initramfs: h.cfg.Initramfs, Cmdline: h.cfg.Cmdline,
		StopGrace: h.cfg.StopGrace, StopSlack: h.cfg.StopSlack,
	})
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, specFile), b); err != nil {
		return err
	}
	argv := append(slices.Clone(launcher), "--dir", h.cfg.Dir, spec.MachineUID)
	return h.sd.start(ctx, unitName(spec.MachineUID), baseProps(spec, argv, restart, stopTimeout(h.cfg.StopGrace, h.cfg.StopSlack)))
}

// SystemdConnects counts the connections to systemd the driver has made.
func (h *Hypervisor) SystemdConnects() int {
	if s, ok := h.sd.(*dbusSystemd); ok {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.connects
	}
	return 0
}
