package node

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/types/known/timestamppb"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// convergeFirmware makes the host run m, a machine that boots its root
// disk's own operating system through firmware, as specified, and says what
// it then runs. Nothing runs in such a guest for the agent to ask: the
// machine is ready once its VM runs (docs/decisions/vm-boot.md).
func (a *Agent) convergeFirmware(ctx context.Context, m *computev1alpha1.Machine, vm *driver.VM, epoch int64, vcpus int32, memoryMiB int64) (*observation, error) {
	uid := m.GetMetadata().GetUid()
	// Its root disk first, as the firmware boots the first disk.
	vols, err := a.volumes(ctx, m)
	if err != nil {
		return nil, err
	}
	uids := make([]string, len(vols))
	for i, v := range vols {
		uids[i] = v.DiskUID
	}
	for i, s := range computeapi.DiskSerials(computeapi.MachineDisks(m.GetSpec()), uids) {
		vols[i].Serial = s
	}
	want, err := a.interfaces(ctx, m)
	if err != nil {
		return nil, err
	}
	ifaces, vm, pending, err := a.ensureInterfaces(ctx, uid, want, vm, true)
	if err != nil {
		return nil, err
	}
	a.netChecked[uid] = epoch
	// The guest learns these by DHCP, from the same leases; the spec
	// carries them as a container's does.
	nameservers, search := guestDNS(m.GetMetadata().GetProject(), ifaces)
	// So is its IPv6 address, by DHCPv6; a new one, as when an Address moves
	// to it, is no part of the spec's hash and does not restart the VM.
	spec := driver.VMSpec{
		MachineUID:         uid,
		Machine:            m.GetMetadata().GetProject() + "/" + m.GetMetadata().GetName(),
		VCPUs:              vcpus,
		MemoryMiB:          memoryMiB,
		Disks:              vols,
		Firmware:           true,
		SystemSerialNumber: m.GetSpec().GetFirmware().GetSystemSerialNumber(),
		Interfaces:         ifaces,
		Nameservers:        nameservers,
		Search:             search,
		Restart:            restartPolicy(m.GetSpec().GetRestartPolicy()),
	}
	if vm != nil && !spec.Runs(vm.SpecHash, vm.Interfaces) {
		if err := a.stopVM(ctx, uid); err != nil {
			return nil, err
		}
		vm = nil
	}
	if vm == nil {
		if err := a.cfg.Drivers.Hypervisor.Start(ctx, spec); err != nil {
			return nil, blocked(computeapi.ReasonBootFailed, err)
		}
		// Seen running on the next look, which the hypervisor's signal or
		// the wait brings.
		return notReady(computev1alpha1.MachinePhase_MACHINE_PHASE_CREATING, computeapi.ReasonBooting, "the VM is starting"),
			errors.Join(fmt.Errorf("%w: machine %s booting", controller.ErrWaiting, spec.Machine), pending)
	}
	if vm.State == driver.VMExited {
		reason, msg := computeapi.ReasonPoweredOff, "the guest powered off"
		if vm.ExitCode != 0 {
			reason, msg = computeapi.ReasonExited, fmt.Sprintf("the VM ended without powering off, code %d", vm.ExitCode)
		}
		obs := notReady(phaseOf(vm), reason, msg)
		obs.applied = true
		return obs, pending
	}
	return &observation{
		phase: computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING,
		conds: []*metav1.Condition{{
			Type: computeapi.MachineReady, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: computeapi.ReasonRunning,
		}},
		started: timestamppb.New(vm.Started),
		applied: true,
	}, pending
}
