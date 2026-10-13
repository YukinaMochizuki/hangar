package node

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// Drivers are the host as the agent sees it.
type Drivers struct {
	Images     driver.Images
	DiskImages driver.DiskImages
	Storage    driver.Storage
	Network    driver.Network
	Hypervisor driver.Hypervisor
	Guest      driver.Guest
}

// stuck is a step the machine cannot get past yet, said the way its owner
// shows it. The reconcile is retried.
type stuck struct {
	reason string
	err    error
}

func (s *stuck) Error() string { return s.reason + ": " + s.err.Error() }
func (s *stuck) Unwrap() error { return s.err }

func blocked(reason string, err error) error { return &stuck{reason, err} }

// reconcileMachine brings the host in line with one machine placed on this
// node. Anything left on the host by machines that are gone is the sweep's.
func (a *Agent) reconcileMachine(ctx context.Context, k controller.Key) (err error) {
	a.host.Lock()
	defer a.host.Unlock()
	m, err := a.getMachine(ctx, k)
	if err != nil || m == nil || m.GetSpec().GetNode() != a.cfg.Name {
		return err
	}
	uid := m.GetMetadata().GetUid()
	if a.stops.running[uid] {
		// Its stop's success, or its own retry, checks it again.
		return stopping(uid)
	}
	defer delete(a.stops.ended, uid)
	defer func() {
		if err == nil {
			// What it needed is done: a later stop is no retry.
			delete(a.stops.failed, uid)
		}
	}()
	if m.GetMetadata().GetDeleteTime() != nil {
		return a.release(ctx, m)
	}
	vm, err := a.vmOf(ctx, m.GetMetadata().GetUid())
	if err != nil {
		return err
	}
	applied := m.GetStatus().GetObservedGeneration() == m.GetMetadata().GetGeneration()
	epoch := a.netEpoch.Load()
	var report *observation
	switch {
	case m.GetSpec().GetRunState() == computev1alpha1.MachineRunState_MACHINE_RUN_STATE_STOPPED:
		// Stopping needs neither the image nor the network.
		report, err = a.stop(ctx, vm)
	default:
		report, err = a.converge(ctx, m, vm, epoch)
	}
	var s *stuck
	if errors.As(err, &s) {
		// The VM as the failed step left it.
		if vm, err2 := a.vmOf(ctx, m.GetMetadata().GetUid()); err2 == nil {
			// The host runs the current spec; a step the agent could not
			// check again does not change that, so the status stands,
			// unless the networking failing to be checked has changed
			// since it was last found whole.
			netSuspect := (s.reason == computeapi.ReasonNetworkFailed || s.reason == computeapi.ReasonNetworkNotReady) &&
				a.netChecked[m.GetMetadata().GetUid()] != epoch
			if applied && vm != nil && vm.State == driver.VMRunning && !netSuspect {
				return err
			}
			report = notReady(phaseOf(vm), s.reason, s.err.Error())
		}
	}
	if report != nil {
		if werr := a.writeStatus(ctx, m, report); werr != nil {
			return werr
		}
	}
	return err
}

// observation is what the node reports about a machine.
type observation struct {
	phase computev1alpha1.MachinePhase
	// conds are the node's conditions, each replacing its type's.
	conds []*metav1.Condition
	// image and digest are the reference the root disk was built from and
	// what it resolved to.
	image, digest string
	started       *timestamppb.Timestamp
	// applied says the host runs the machine's current generation, as far
	// as it ever will: running, stopped or exited.
	applied bool
}

func notReady(phase computev1alpha1.MachinePhase, reason, msg string) *observation {
	return &observation{phase: phase, conds: []*metav1.Condition{{
		Type: computeapi.MachineReady, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: reason, Message: msg,
	}}}
}

// phaseOf is the phase of a machine whose VM is vm, nil if it has none.
func phaseOf(vm *driver.VM) computev1alpha1.MachinePhase {
	switch {
	case vm == nil:
		return computev1alpha1.MachinePhase_MACHINE_PHASE_CREATING
	case vm.State == driver.VMRunning:
		return computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING
	case vm.ExitCode == 0:
		return computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED
	}
	return computev1alpha1.MachinePhase_MACHINE_PHASE_FAILED
}

// exited reports a VM its restart policy keeps down.
func exited(vm *driver.VM) *observation {
	obs := notReady(phaseOf(vm), computeapi.ReasonExited, fmt.Sprintf("the container exited with code %d", vm.ExitCode))
	obs.applied = true
	return obs
}

func (a *Agent) vmOf(ctx context.Context, uid string) (*driver.VM, error) {
	vms, err := a.cfg.Drivers.Hypervisor.VMs(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing VMs: %w", err)
	}
	if i := slices.IndexFunc(vms, func(v driver.VM) bool { return v.MachineUID == uid }); i >= 0 {
		return &vms[i], nil
	}
	return nil, nil
}

// stop stops the machine's VM, keeping its disk and interfaces.
func (a *Agent) stop(ctx context.Context, vm *driver.VM) (*observation, error) {
	if vm != nil {
		if err := a.stopVM(ctx, vm.MachineUID); err != nil {
			return nil, err
		}
	}
	obs := notReady(computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, computeapi.ReasonStopped, "the machine is stopped")
	obs.applied = true
	return obs, nil
}

// converge makes the host run m as specified and says what it then runs.
// epoch is the network driver's change count when the reconcile began.
func (a *Agent) converge(ctx context.Context, m *computev1alpha1.Machine, vm *driver.VM, epoch int64) (*observation, error) {
	uid := m.GetMetadata().GetUid()
	d := a.cfg.Drivers
	boot := m.GetSpec().GetContainer()
	if boot == nil {
		return notReady(computev1alpha1.MachinePhase_MACHINE_PHASE_FAILED, computeapi.ReasonBootFailed,
			"the machine has no boot source this node knows"), nil
	}

	// A reference is resolved once, so a tag moving upstream never rebuilds
	// a container's disk; only naming another image does. The image is
	// prepared off the host lock, so it never holds up other machines.
	pinned := ""
	if m.GetStatus().GetImage() == boot.GetImage() {
		pinned = m.GetStatus().GetImageDigest()
	}
	digest, image, ready, err := a.puller.get(uid, boot.GetImage(), pinned)
	if err != nil {
		return nil, err // stuck, saying on which step
	}
	if !ready {
		waiting := fmt.Errorf("%w: machine %s's image %s", controller.ErrWaiting, uid, boot.GetImage())
		if vm != nil && m.GetStatus().GetObservedGeneration() == m.GetMetadata().GetGeneration() {
			// The VM is of the current spec, as after the agent restarts:
			// the image is only being checked again.
			return nil, waiting
		}
		return notReady(phaseOf(vm), computeapi.ReasonPulling, "pulling "+boot.GetImage()), waiting
	}
	bases, err := d.Storage.Bases(ctx)
	if err != nil {
		return nil, blocked(computeapi.ReasonDiskFailed, err)
	}
	if !slices.Contains(bases, digest) {
		// Gone since it was made: make it again.
		a.puller.forget(digest)
		return notReady(phaseOf(vm), computeapi.ReasonPulling, "pulling "+boot.GetImage()),
			fmt.Errorf("%w: machine %s's image %s, made again", controller.ErrWaiting, uid, boot.GetImage())
	}

	// A root disk of another image is rebuilt: the container's disk lives
	// with it, but only as long as its image.
	roots, err := d.Storage.Roots(ctx)
	if err != nil {
		return nil, blocked(computeapi.ReasonDiskFailed, err)
	}
	if i := slices.IndexFunc(roots, func(r driver.Disk) bool { return r.MachineUID == uid }); i >= 0 && roots[i].Digest != digest {
		if vm != nil {
			if err := a.stopVM(ctx, uid); err != nil {
				return nil, err
			}
		}
		vm = nil
		if err := d.Storage.DeleteRoot(ctx, uid); err != nil {
			return nil, blocked(computeapi.ReasonDiskFailed, err)
		}
	}
	size, err := diskSize(m.GetSpec().GetResources())
	if err != nil {
		return nil, blocked(computeapi.ReasonDiskFailed, err)
	}
	root, err := d.Storage.EnsureRoot(ctx, uid, digest, size)
	switch {
	case errors.Is(err, driver.ErrDiskTooSmall):
		return nil, blocked(computeapi.ReasonDiskTooSmall, err)
	case err != nil:
		return nil, blocked(computeapi.ReasonDiskFailed, err)
	}

	vols, err := a.volumes(ctx, m)
	if err != nil {
		return nil, err
	}
	want, err := a.interfaces(ctx, m)
	if err != nil {
		return nil, err
	}
	ifaces, vm, err := a.ensureInterfaces(ctx, uid, want, vm)
	if err != nil {
		return nil, err
	}
	a.netChecked[uid] = epoch

	vcpus, memoryMiB, err := vmSize(m.GetSpec().GetResources())
	if err != nil {
		return nil, blocked(computeapi.ReasonBootFailed, err)
	}
	nameservers, search := guestDNS(m.GetMetadata().GetProject(), ifaces)
	spec := driver.VMSpec{
		MachineUID:  uid,
		Machine:     m.GetMetadata().GetProject() + "/" + m.GetMetadata().GetName(),
		VCPUs:       vcpus,
		MemoryMiB:   memoryMiB,
		Root:        root,
		Disks:       vols,
		Interfaces:  ifaces,
		Nameservers: nameservers,
		Search:      search,
		Run:         runSpec(boot, image),
		Restart:     restartPolicy(m.GetSpec().GetRestartPolicy()),
	}
	obs, err := a.ensureVM(ctx, spec, vm)
	if obs != nil {
		obs.image, obs.digest = boot.GetImage(), digest
	}
	return obs, err
}

// interfaces works out the host side of each of m's interfaces, making
// their networks on the way.
func (a *Agent) interfaces(ctx context.Context, m *computev1alpha1.Machine) ([]driver.Interface, error) {
	var out []driver.Interface
	for _, nic := range m.GetSpec().GetInterfaces() {
		n, err := a.getNetwork(ctx, m.GetMetadata().GetProject(), nic.GetNetwork())
		if err != nil {
			return nil, err
		}
		cidr, err1 := netip.ParsePrefix(n.GetSpec().GetIpv4Cidr())
		transit, err2 := netip.ParsePrefix(n.GetStatus().GetTransitIpv4())
		addr, err3 := netip.ParsePrefix(nic.GetIpv4())
		if n.GetMetadata().GetUid() == "" || err1 != nil || err2 != nil || err3 != nil {
			return nil, blocked(computeapi.ReasonNetworkNotReady, fmt.Errorf(
				"network %s or interface %s has no address yet", nic.GetNetwork(), nic.GetName()))
		}
		spec := driver.NetworkSpec{UID: n.GetMetadata().GetUid(), CIDR: cidr, Transit: transit}
		if err := a.cfg.Drivers.Network.EnsureNetwork(ctx, spec); err != nil {
			return nil, blocked(computeapi.ReasonNetworkFailed, err)
		}
		out = append(out, driver.Interface{
			MachineUID: m.GetMetadata().GetUid(),
			Name:       nic.GetName(),
			NetworkUID: spec.UID,
			MAC:        mac(m.GetMetadata().GetUid(), nic.GetName()),
			Address:    addr,
			Gateway:    cidr.Addr().Next(),
		})
	}
	return out, nil
}

// ensureInterfaces makes the host hold exactly want for the machine and
// returns them as made, with the VM as left. A running VM holds its taps, so
// it is stopped first when one is changed, taken away or gone from the host.
func (a *Agent) ensureInterfaces(ctx context.Context, uid string, want []driver.Interface, vm *driver.VM) ([]driver.Interface, *driver.VM, error) {
	d := a.cfg.Drivers
	all, err := d.Network.Interfaces(ctx)
	if err != nil {
		return nil, vm, blocked(computeapi.ReasonNetworkFailed, err)
	}
	var have []driver.Interface
	for _, i := range all {
		if i.MachineUID == uid {
			have = append(have, i)
		}
	}
	stale := slices.ContainsFunc(have, func(h driver.Interface) bool {
		return !slices.ContainsFunc(want, func(w driver.Interface) bool { return sameInterface(h, w) })
	})
	missing := slices.ContainsFunc(want, func(w driver.Interface) bool {
		return !slices.ContainsFunc(have, func(h driver.Interface) bool { return sameInterface(h, w) })
	})
	if vm != nil && vm.State == driver.VMRunning && (stale || missing) {
		if err := a.stopVM(ctx, uid); err != nil {
			return nil, vm, err
		}
		vm = nil
	}
	if stale {
		if err := d.Network.DeleteInterfaces(ctx, uid); err != nil {
			return nil, vm, blocked(computeapi.ReasonNetworkFailed, err)
		}
	}
	made := make([]driver.Interface, 0, len(want))
	for _, w := range want {
		i, err := d.Network.EnsureInterface(ctx, w)
		if err != nil {
			return nil, vm, blocked(computeapi.ReasonNetworkFailed, err)
		}
		made = append(made, i)
	}
	return made, vm, nil
}

// sameInterface compares interfaces by what the agent asks for; the device
// is the driver's choice.
func sameInterface(a, b driver.Interface) bool {
	a.Device, b.Device = "", ""
	return a == b
}

// guestTimeout bounds asking a guest agent whether it answers, which happens
// under the host lock.
const guestTimeout = 2 * time.Second

// ensureVM runs the VM as specified, starting it again if it runs anything
// else, and says how it runs.
func (a *Agent) ensureVM(ctx context.Context, spec driver.VMSpec, vm *driver.VM) (*observation, error) {
	d := a.cfg.Drivers
	if vm != nil && vm.SpecHash != spec.Hash() {
		if err := a.stopVM(ctx, spec.MachineUID); err != nil {
			return nil, err
		}
		vm = nil
	}
	if vm == nil {
		if err := d.Hypervisor.Start(ctx, spec); err != nil {
			return nil, blocked(computeapi.ReasonBootFailed, err)
		}
		return notReady(computev1alpha1.MachinePhase_MACHINE_PHASE_CREATING, computeapi.ReasonBooting, "the VM is starting"),
			fmt.Errorf("%w: machine %s booting", controller.ErrWaiting, spec.Machine)
	}
	if vm.State == driver.VMExited {
		return exited(vm), nil
	}
	gctx, cancel := context.WithTimeout(ctx, guestTimeout)
	defer cancel()
	if err := d.Guest.Ready(gctx, spec.MachineUID); err != nil {
		return notReady(computev1alpha1.MachinePhase_MACHINE_PHASE_CREATING, computeapi.ReasonBooting, err.Error()),
			fmt.Errorf("%w: machine %s's guest agent: %w", controller.ErrWaiting, spec.Machine, err)
	}
	return &observation{
		phase: computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING,
		conds: []*metav1.Condition{{
			Type: computeapi.MachineReady, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: computeapi.ReasonRunning,
		}},
		started: timestamppb.New(vm.Started),
		applied: true,
	}, nil
}

// release takes a deleted machine off the host and says so, only once
// nothing of it is left.
func (a *Agent) release(ctx context.Context, m *computev1alpha1.Machine) error {
	if conditions.IsTrue(m.GetStatus().GetConditions(), computeapi.MachineReleased) {
		return nil
	}
	uid := m.GetMetadata().GetUid()
	d := a.cfg.Drivers
	vm, err := a.vmOf(ctx, uid)
	if err != nil {
		return err
	}
	// A VM not listed has nothing to stop; DeleteLogs removes what is left
	// of it.
	if vm != nil {
		if err := a.stopVM(ctx, uid); err != nil {
			return err
		}
	}
	if err := d.Hypervisor.DeleteLogs(ctx, uid); err != nil {
		return err
	}
	if err := d.Network.DeleteInterfaces(ctx, uid); err != nil {
		return err
	}
	if err := d.Storage.DeleteRoot(ctx, uid); err != nil {
		return err
	}
	a.puller.drop(uid)
	delete(a.netChecked, uid)
	obs := notReady(computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, computeapi.ReasonStopped, "the machine was deleted")
	obs.conds = append(obs.conds, &metav1.Condition{
		Type: computeapi.MachineReleased, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Cleaned",
		Message: "the VM, its log, its disk and its interfaces are gone from the host",
	})
	return a.writeStatus(ctx, m, obs)
}

// writeStatus records obs on m as read, keeping what others own (the
// scheduler's condition), and writes nothing if nothing changed. The write
// is conditional on m's resource version.
func (a *Agent) writeStatus(ctx context.Context, m *computev1alpha1.Machine, obs *observation) error {
	gen := m.GetMetadata().GetGeneration()
	status := &computev1alpha1.MachineStatus{}
	if m.GetStatus() != nil {
		status = proto.Clone(m.GetStatus()).(*computev1alpha1.MachineStatus)
	}
	status.Phase = obs.phase
	if obs.digest != "" {
		status.Image, status.ImageDigest = obs.image, obs.digest
	}
	if obs.started != nil {
		status.BootTime = obs.started
	}
	if obs.applied {
		status.ObservedGeneration = gen
	}
	for _, c := range obs.conds {
		c = proto.Clone(c).(*metav1.Condition)
		c.ObservedGeneration = gen
		status.Conditions = conditions.Set(status.GetConditions(), c, a.cfg.Clock.Now())
	}
	if proto.Equal(status, m.GetStatus()) {
		return nil
	}
	rv := m.GetMetadata().GetResourceVersion()
	_, err := a.machines.UpdateMachineStatus(ctx, connect.NewRequest(&computev1alpha1.UpdateMachineStatusRequest{
		Project: m.GetMetadata().GetProject(), Name: m.GetMetadata().GetName(), Status: status, ExpectedResourceVersion: &rv,
	}))
	return err
}

func (a *Agent) getMachine(ctx context.Context, k controller.Key) (*computev1alpha1.Machine, error) {
	res, err := a.machines.GetMachine(ctx, connect.NewRequest(&computev1alpha1.GetMachineRequest{Project: k.Project, Name: k.Name}))
	if connect.CodeOf(err) == connect.CodeNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return res.Msg.GetMachine(), nil
}

func (a *Agent) getNetwork(ctx context.Context, project, name string) (*networkv1alpha1.Network, error) {
	res, err := a.networks.GetNetwork(ctx, connect.NewRequest(&networkv1alpha1.GetNetworkRequest{Project: project, Name: name}))
	if connect.CodeOf(err) == connect.CodeNotFound {
		return nil, blocked(computeapi.ReasonNetworkNotReady, fmt.Errorf("network %s does not exist", name))
	}
	if err != nil {
		return nil, err
	}
	return res.Msg.GetNetwork(), nil
}

// runSpec is what the guest runs: the container's command and arguments
// where given, the image's otherwise. As in Kubernetes, a command given
// without arguments drops the image's arguments too.
func runSpec(boot *computev1alpha1.ContainerBoot, image driver.ImageConfig) driver.RunSpec {
	command, args := image.Entrypoint, image.Cmd
	if len(boot.GetCommand()) > 0 {
		command, args = boot.GetCommand(), nil
	}
	if len(boot.GetArgs()) > 0 {
		args = boot.GetArgs()
	}
	env := slices.Clone(image.Env)
	for _, e := range boot.GetEnv() {
		env = withEnv(env, e.GetName(), e.GetValue())
	}
	return driver.RunSpec{
		Command:    append(slices.Clone(command), args...),
		Env:        env,
		WorkingDir: image.WorkingDir,
		User:       image.User,
	}
}

// withEnv returns a copy of env, KEY=VALUE pairs, with name set to value.
func withEnv(env []string, name, value string) []string {
	prefix := name + "="
	env = slices.DeleteFunc(slices.Clone(env), func(kv string) bool { return strings.HasPrefix(kv, prefix) })
	return append(env, prefix+value)
}

func restartPolicy(p computev1alpha1.RestartPolicy) driver.RestartPolicy {
	switch p {
	case computev1alpha1.RestartPolicy_RESTART_POLICY_ON_FAILURE:
		return driver.RestartOnFailure
	case computev1alpha1.RestartPolicy_RESTART_POLICY_NEVER:
		return driver.RestartNever
	}
	return driver.RestartAlways
}

// mac derives a stable, locally administered unicast MAC for an interface.
func mac(machineUID, name string) string {
	sum := sha256.Sum256([]byte(machineUID + "/" + name))
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", sum[0], sum[1], sum[2], sum[3], sum[4])
}
