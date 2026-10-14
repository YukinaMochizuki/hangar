package apiserver

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

const (
	// RootDiskOf is the annotation a root disk made for a VM carries: the
	// VM's uid. It is how the VM's controller knows the disk as its own,
	// whatever its delete policy, which says whether the VM owns it.
	RootDiskOf = registry.Domain + "/virtual-machine"
	// VMReady and VMStopped are a VirtualMachine's conditions, as
	// ContainerReady and ContainerStopped are a Container's.
	VMReady   = "Ready"
	VMStopped = "Stopped"
)

// Sizes a VM gets when its spec leaves them out, and the least memory it
// may ask for: a distribution's image boots in no less.
const (
	defaultVMMemory   = "1Gi"
	defaultVMRootSize = "10Gi"
	minVMMemory       = 256 * quantity.MiB
	// maxSSHKeys, maxSSHKeyLen and maxSSHKeysLen bound the keys a VM's
	// spec lists: how many, each, and all of them.
	maxSSHKeys    = 32
	maxSSHKeyLen  = 16 << 10
	maxSSHKeysLen = 32 << 10
	// maxUserData bounds a VM's user-data (docs/decisions/vm-boot.md).
	// maxVendorData bounds a machine's vendor-data, the platform's, which
	// holds the VM's keys, each quoted, so longer than they are.
	maxUserData   = 16 << 10
	maxVendorData = 64 << 10
)

// prefixedName is name with prefix, shortened with a hash when it would
// pass a DNS label, so names made from different kinds cannot collide.
func prefixedName(prefix, name string) string {
	if len(prefix)+len(name) <= 63 {
		return prefix + name
	}
	sum := sha256.Sum256([]byte(name))
	return prefix + name[:63-len(prefix)-9] + "-" + hex.EncodeToString(sum[:])[:8]
}

// vmMachineName is the machine a VM runs as.
func vmMachineName(vm string) string { return prefixedName("vm-", vm) }

// rootDiskName is the disk a VM's controller makes for it from an image.
func rootDiskName(vm string) string {
	const suffix = "-root"
	if len(vm)+len(suffix) <= 63 {
		return vm + suffix
	}
	sum := sha256.Sum256([]byte(vm))
	return vm[:63-len(suffix)-9] + "-" + hex.EncodeToString(sum[:])[:8] + suffix
}

// vmRootDisk is the disk a VM boots from: one it names, or the one made for
// it.
func vmRootDisk(vm *computev1alpha1.VirtualMachine) string {
	if d := vm.GetSpec().GetRootDisk().GetDisk(); d != "" {
		return d
	}
	return rootDiskName(vm.GetMetadata().GetName())
}

// retains says whether a VM's root disk made from an image outlives it.
func retains(vm *computev1alpha1.VirtualMachine) bool {
	return vm.GetSpec().GetRootDisk().GetFromImage().GetDeletePolicy() == computev1alpha1.VirtualMachineDeletePolicy_VIRTUAL_MACHINE_DELETE_POLICY_RETAIN
}

// vmSizes reads a VM's resources, with the defaults for what it leaves
// empty.
func vmSizes(r *computev1alpha1.VirtualMachineResources) (sizes, error) {
	var s sizes
	var err error
	if s.cpu, err = quantity.CPU(cmp.Or(r.GetCpu(), defaultCPU)); err != nil {
		return s, fmt.Errorf("spec.resources.cpu: %w", err)
	}
	if s.memory, err = quantity.Bytes(cmp.Or(r.GetMemory(), defaultVMMemory)); err != nil {
		return s, fmt.Errorf("spec.resources.memory: %w", err)
	}
	return s, nil
}

func validateVirtualMachine(ctx context.Context, s *Server, _ auth.Principal, old, obj proto.Message) error {
	vm := obj.(*computev1alpha1.VirtualMachine)
	spec := vm.GetSpec()
	sz, err := vmSizes(spec.GetResources())
	if err != nil {
		return invalid("%v", err)
	}
	if sz.memory < minVMMemory {
		return invalid("spec.resources.memory %s is below the minimum of 256Mi", cmp.Or(spec.GetResources().GetMemory(), defaultVMMemory))
	}
	if err := checkMaximums(sz, cmp.Or(spec.GetResources().GetCpu(), defaultCPU), cmp.Or(spec.GetResources().GetMemory(), defaultVMMemory), ""); err != nil {
		return err
	}
	switch spec.GetRunState() {
	case computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_UNSPECIFIED, computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_RUNNING,
		computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_STOPPED:
	default:
		return invalid("spec.run_state %v is not RUNNING or STOPPED", spec.GetRunState())
	}
	if err := s.validateRootDisk(ctx, old, vm); err != nil {
		return err
	}
	seen := map[string]bool{vmRootDisk(vm): true}
	for i, d := range spec.GetDisks() {
		if err := ValidName(d.GetDisk()); err != nil {
			return invalid("spec.disks[%d].disk: %v", i, err)
		}
		if seen[d.GetDisk()] {
			return invalid("spec.disks: disk %q is attached twice, or is the root disk", d.GetDisk())
		}
		seen[d.GetDisk()] = true
	}
	// As a container's: each on a network once, in groups of that network.
	if err := s.validateInterfaces(ctx, vm.GetMetadata().GetProject(), spec.GetInterfaces()); err != nil {
		return err
	}
	if n := len(spec.GetUserData()); n > maxUserData {
		return invalid("spec.user_data is %d bytes, at most %d (16 KiB)", n, maxUserData)
	}
	if n := len(spec.GetSshAuthorizedKeys()); n > maxSSHKeys {
		return invalid("spec.ssh_authorized_keys: %d keys, at most %d", n, maxSSHKeys)
	}
	total := 0
	for i, k := range spec.GetSshAuthorizedKeys() {
		if err := checkAuthorizedKey(k); err != nil {
			return invalid("spec.ssh_authorized_keys[%d]: %v", i, err)
		}
		total += len(k)
	}
	if total > maxSSHKeysLen {
		return invalid("spec.ssh_authorized_keys: %d bytes in all, at most %d", total, maxSSHKeysLen)
	}
	// Quoting lengthens a key, a character such as < to six bytes, and the
	// CA's script adds to it: the machine's limit is on what they become.
	if n := vendorDataSize(vm, s.cfg.SSHCA.PublicKey()); n > maxVendorData {
		return invalid("spec.ssh_authorized_keys: %d bytes once quoted as the guest's vendor-data, at most %d", n, maxVendorData)
	}
	return nil
}

// validateRootDisk checks a VM's root disk: one it names, or one made from
// an image, checked as a Disk of that image and size would be.
func (s *Server) validateRootDisk(ctx context.Context, old proto.Message, vm *computev1alpha1.VirtualMachine) error {
	root := vm.GetSpec().GetRootDisk()
	switch src := root.GetSource().(type) {
	case *computev1alpha1.VirtualMachineRootDisk_Disk:
		if err := ValidName(src.Disk); err != nil {
			return invalid("spec.root_disk.disk: %v", err)
		}
		return nil
	case *computev1alpha1.VirtualMachineRootDisk_FromImage:
		fi := src.FromImage
		switch fi.GetDeletePolicy() {
		case computev1alpha1.VirtualMachineDeletePolicy_VIRTUAL_MACHINE_DELETE_POLICY_UNSPECIFIED, computev1alpha1.VirtualMachineDeletePolicy_VIRTUAL_MACHINE_DELETE_POLICY_DELETE,
			computev1alpha1.VirtualMachineDeletePolicy_VIRTUAL_MACHINE_DELETE_POLICY_RETAIN:
		default:
			return invalid("spec.root_disk.from_image.delete_policy %v is not DELETE or RETAIN", fi.GetDeletePolicy())
		}
		if fi.GetImage().GetName() == "" {
			return invalid("spec.root_disk.from_image.image.name is required")
		}
		d := imageDisk(vm)
		size, err := diskBytes(d.GetSpec())
		if err != nil {
			return invalid("spec.root_disk.from_image.%v", err)
		}
		if size < minDisk || size > maxDisk {
			return invalid("spec.root_disk.from_image.size %s is not between 1Gi and 64Ti", d.GetSpec().GetSize())
		}
		var was *storagev1alpha1.Disk
		if o, ok := old.(*computev1alpha1.VirtualMachine); ok && o.GetSpec().GetRootDisk().GetFromImage() != nil {
			was = imageDisk(o)
		}
		if err := s.validateDiskSource(ctx, was, d, size); err != nil {
			var ce *connect.Error
			if errors.As(err, &ce) {
				return connect.NewError(ce.Code(), fmt.Errorf("spec.root_disk.from_image: %s", ce.Message()))
			}
			return err
		}
		return nil
	}
	return invalid("spec.root_disk is required: from_image, or a disk")
}

// imageDisk is the Disk a VM's spec asks to be made for it from an image.
func imageDisk(vm *computev1alpha1.VirtualMachine) *storagev1alpha1.Disk {
	fi := vm.GetSpec().GetRootDisk().GetFromImage()
	return &storagev1alpha1.Disk{
		Metadata: &metav1.ObjectMeta{Project: vm.GetMetadata().GetProject(), Name: rootDiskName(vm.GetMetadata().GetName())},
		Spec: &storagev1alpha1.DiskSpec{
			Size:   cmp.Or(fi.GetSize(), defaultVMRootSize),
			Source: &storagev1alpha1.DiskSource{Source: &storagev1alpha1.DiskSource_Image{Image: proto.Clone(fi.GetImage()).(*storagev1alpha1.ImageSource)}},
		},
	}
}

// sshKeyTypes are the key types an authorized_keys line may start with.
var sshKeyTypes = []string{
	"ssh-ed25519", "ssh-rsa", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521",
	"sk-ssh-ed25519@openssh.com", "sk-ecdsa-sha2-nistp256@openssh.com",
}

// checkAuthorizedKey checks the shape of an authorized_keys line: a key
// type, the key, whose encoding names the same type, and a comment, on one
// line, with no options before the type. Its characters are printable, or
// tabs: cloud-init's YAML parser refuses the whole vendor-data over some
// others, and changes some.
func checkAuthorizedKey(k string) error {
	if len(k) > maxSSHKeyLen || strings.ContainsAny(k, "\r\n\x00") {
		return fmt.Errorf("not one line of at most %d bytes", maxSSHKeyLen)
	}
	if !utf8.ValidString(k) {
		return errors.New("not UTF-8")
	}
	if i := strings.IndexFunc(k, func(r rune) bool { return r != '\t' && !unicode.IsPrint(r) }); i >= 0 {
		r, _ := utf8.DecodeRuneInString(k[i:])
		return fmt.Errorf("%U at byte %d is not a printable character", r, i)
	}
	f := strings.Fields(k)
	if len(f) < 2 || !slices.Contains(sshKeyTypes, f[0]) {
		return errors.New("does not start with a key type, such as ssh-ed25519, and the key")
	}
	blob, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil {
		return fmt.Errorf("the key is not base64: %w", err)
	}
	if len(blob) < 4 {
		return errors.New("the key is too short")
	}
	n := binary.BigEndian.Uint32(blob)
	if uint64(n) > uint64(len(blob)-4) || string(blob[4:4+n]) != f[0] {
		return fmt.Errorf("the key is not a %s key", f[0])
	}
	return nil
}

// keysConfig is the cloud-config giving a VM's guest its keys, each quoted
// as JSON, which YAML reads as the same string; see vm-boot.md.
func keysConfig(spec *computev1alpha1.VirtualMachineSpec) string {
	keys := spec.GetSshAuthorizedKeys()
	if len(keys) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("#cloud-config\nssh_authorized_keys:\n")
	for _, k := range keys {
		q, _ := json.Marshal(k) // a string always encodes
		fmt.Fprintf(&b, "  - %s\n", q)
	}
	return b.String()
}

// refuseDeletingUnsettledVM keeps a VM from going before its root disk's
// owner reference says what its delete policy does: the disk would go with
// it, or stay, against the policy. Its controller settles it at once; the
// caller retries. A project being deleted takes the disk anyway.
func refuseDeletingUnsettledVM(ctx context.Context, s *Server, obj proto.Message) error {
	vm := obj.(*computev1alpha1.VirtualMachine)
	if vm.GetSpec().GetRootDisk().GetFromImage() == nil {
		return nil
	}
	project, err := s.get(ctx, registry.Project, resource.Key{Name: vm.GetMetadata().GetProject()})
	if err != nil {
		return err
	}
	if project == nil || project.(*tenancyv1alpha1.Project).GetMetadata().GetDeleteTime() != nil {
		return nil
	}
	dobj, err := s.get(ctx, registry.Disk, resource.Key{Project: vm.GetMetadata().GetProject(), Name: rootDiskName(vm.GetMetadata().GetName())})
	if err != nil || dobj == nil {
		return err
	}
	d := dobj.(*storagev1alpha1.Disk)
	if d.GetMetadata().GetAnnotations()[RootDiskOf] != vm.GetMetadata().GetUid() {
		return nil
	}
	if ownsDisk(d, vm) == retains(vm) {
		return connect.NewError(connect.CodeAborted, fmt.Errorf(
			"virtual machine %q's root disk %s does not follow its delete policy yet; retry", vm.GetMetadata().GetName(), d.GetMetadata().GetName()))
	}
	return nil
}

// checkRootDiskOwner admits a VM root disk's VM owner reference only as the
// live VM's delete policy says; see docs/decisions/vm-boot.md, root disks.
func checkRootDiskOwner(ctx context.Context, s *Server, was, d *storagev1alpha1.Disk) error {
	uid := d.GetMetadata().GetAnnotations()[RootDiskOf]
	if uid == "" {
		return nil
	}
	owned := slices.ContainsFunc(d.GetMetadata().GetOwnerReferences(), func(r *metav1.OwnerReference) bool { return r.GetUid() == uid })
	if was != nil && owned == slices.ContainsFunc(was.GetMetadata().GetOwnerReferences(), func(r *metav1.OwnerReference) bool {
		return r.GetUid() == uid
	}) {
		return nil
	}
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.VirtualMachine.Kind, Project: d.GetMetadata().GetProject()})
	if err != nil {
		return err
	}
	var vm *computev1alpha1.VirtualMachine
	for _, o := range objs {
		if v, ok := decodeVM(o); ok && v.GetMetadata().GetUid() == uid {
			vm = v
		}
	}
	switch {
	case vm == nil || vm.GetMetadata().GetDeleteTime() != nil:
		return connect.NewError(connect.CodeAborted, fmt.Errorf(
			"disk %s is the root disk of a virtual machine that is gone or being deleted", d.GetMetadata().GetName()))
	case owned == retains(vm):
		return connect.NewError(connect.CodeAborted, fmt.Errorf(
			"disk %s's owner reference does not follow virtual machine %q's delete policy as it is now; retry",
			d.GetMetadata().GetName(), vm.GetMetadata().GetName()))
	}
	return nil
}

// ownsDisk says whether d carries an owner reference to vm.
func ownsDisk(d *storagev1alpha1.Disk, vm *computev1alpha1.VirtualMachine) bool {
	return slices.ContainsFunc(d.GetMetadata().GetOwnerReferences(), func(r *metav1.OwnerReference) bool {
		return r.GetUid() == vm.GetMetadata().GetUid()
	})
}

// vmRef ties a root disk to the VM that deletes it, not as its controller;
// see docs/decisions/vm-boot.md, root disks.
func vmRef(vm *computev1alpha1.VirtualMachine) *metav1.OwnerReference {
	return &metav1.OwnerReference{
		ApiVersion: registry.VirtualMachine.APIVersion(), Kind: registry.VirtualMachine.Kind,
		Name: vm.GetMetadata().GetName(), Uid: vm.GetMetadata().GetUid(),
	}
}

// reconcileVirtualMachine keeps a VM's root disk and machine in line with
// its spec, and reports the machine's state back on the VM.
func (s *Server) reconcileVirtualMachine(ctx context.Context, k controller.Key) error {
	key := resource.Key{Project: k.Project, Name: k.Name}
	obj, err := s.get(ctx, registry.VirtualMachine, key)
	if err != nil || obj == nil {
		return err
	}
	vm := obj.(*computev1alpha1.VirtualMachine)
	// Deleting cascades to the machine and to a root disk the VM owns.
	if vm.GetMetadata().GetDeleteTime() != nil {
		return nil
	}
	mobj, err := s.get(ctx, registry.Machine, resource.Key{Project: k.Project, Name: vmMachineName(k.Name)})
	if err != nil {
		return err
	}
	m, _ := mobj.(*computev1alpha1.Machine)
	var stuck *blocked
	if err := s.ensureRootDisk(ctx, vm); errors.As(err, &stuck) {
		return s.reportVirtualMachine(ctx, vm, m, nil, stuck)
	} else if err != nil {
		return err
	}
	want, err := vmMachineSpec(vm, s.cfg.SSHCA.PublicKey())
	if err != nil {
		return s.reportVirtualMachine(ctx, vm, m, nil, &blocked{"InvalidResources", err.Error()})
	}
	m, err = s.ensureMachineOf(ctx, registry.VirtualMachine, vm.GetMetadata(), vmMachineName(k.Name), want, m)
	if errors.As(err, &stuck) {
		return s.reportVirtualMachine(ctx, vm, m, nil, stuck)
	}
	if err != nil {
		return err
	}
	var node *computev1alpha1.Node
	if name := m.GetSpec().GetNode(); name != "" {
		nobj, err := s.get(ctx, registry.Node, resource.Key{Name: name})
		if err != nil {
			return err
		}
		node, _ = nobj.(*computev1alpha1.Node)
	}
	return s.reportVirtualMachine(ctx, vm, m, node, nil)
}

// ensureRootDisk makes the root disk a VM asks for from an image, or keeps
// it in line: its size, and an owner reference to the VM as its delete
// policy says. Every write is conditional on the disk as read.
func (s *Server) ensureRootDisk(ctx context.Context, vm *computev1alpha1.VirtualMachine) error {
	if vm.GetSpec().GetRootDisk().GetFromImage() == nil {
		return nil
	}
	want := imageDisk(vm)
	key := resource.Key{Project: want.GetMetadata().GetProject(), Name: want.GetMetadata().GetName()}
	obj, err := s.get(ctx, registry.Disk, key)
	if err != nil {
		return err
	}
	if obj == nil {
		want.Metadata.Annotations = map[string]string{RootDiskOf: vm.GetMetadata().GetUid()}
		if !retains(vm) {
			want.Metadata.OwnerReferences = []*metav1.OwnerReference{vmRef(vm)}
		}
		_, err := s.backend(registry.Disk).Create(s.system(ctx), want)
		return refusedDisk(err)
	}
	d := obj.(*storagev1alpha1.Disk)
	if d.GetMetadata().GetAnnotations()[RootDiskOf] != vm.GetMetadata().GetUid() {
		return &blocked{"RootDiskNameInUse", fmt.Sprintf(
			"disk %s is not this VM's own: a disk an earlier VM of this name retained, or another; delete it, or name it as spec.root_disk.disk", key.Name)}
	}
	if d.GetMetadata().GetDeleteTime() != nil {
		return &blocked{"RootDiskDeleting", fmt.Sprintf("disk %s is being deleted", key.Name)}
	}
	updated := proto.Clone(d).(*storagev1alpha1.Disk)
	updated.Spec.Size = want.GetSpec().GetSize()
	switch owned := ownsDisk(d, vm); {
	case owned && retains(vm):
		updated.Metadata.OwnerReferences = slices.DeleteFunc(updated.GetMetadata().GetOwnerReferences(), func(r *metav1.OwnerReference) bool {
			return r.GetUid() == vm.GetMetadata().GetUid()
		})
	case !owned && !retains(vm):
		updated.Metadata.OwnerReferences = append(updated.Metadata.OwnerReferences, vmRef(vm))
	}
	if proto.Equal(updated, d) {
		return nil
	}
	_, err = s.backend(registry.Disk).Apply(s.system(ctx), updated)
	return refusedDisk(err)
}

// refusedDisk makes admission refusing a VM's root disk what the VM
// reports; other errors are retried.
func refusedDisk(err error) error {
	switch connect.CodeOf(err) {
	case connect.CodeInvalidArgument, connect.CodeFailedPrecondition:
		return &blocked{"RootDiskRefused", err.Error()}
	}
	return err
}

// vmMachineSpec is what a VM's spec asks of its machine, in canonical
// quantities, caKey the platform SSH CA's public key if it has one; node is
// left for the scheduler.
func vmMachineSpec(vm *computev1alpha1.VirtualMachine, caKey string) (*computev1alpha1.MachineSpec, error) {
	spec := vm.GetSpec()
	sz, err := vmSizes(spec.GetResources())
	if err != nil {
		return nil, err
	}
	runState := computev1alpha1.MachineRunState_MACHINE_RUN_STATE_RUNNING
	if spec.GetRunState() == computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_STOPPED {
		runState = computev1alpha1.MachineRunState_MACHINE_RUN_STATE_STOPPED
	}
	out := &computev1alpha1.MachineSpec{
		Resources: &computev1alpha1.Resources{Cpu: quantity.FormatCPU(sz.cpu), Memory: quantity.FormatMemory(sz.memory)},
		RunState:  runState,
		Boot: &computev1alpha1.MachineSpec_Firmware{Firmware: &computev1alpha1.FirmwareBoot{
			RootDisk: vmRootDisk(vm),
			// The VM's uid, not its machine's: a machine made again is the
			// same instance, whose first boot is done.
			CloudInit: &computev1alpha1.MachineCloudInit{
				InstanceId: vm.GetMetadata().GetUid(), UserData: spec.GetUserData(), VendorData: vendorData(vm, caKey),
			},
		}},
		// Only a guest that powers itself off, which ends its boot well,
		// stays off, as a VM's on a cloud does; a crash, or its host
		// shutting down, boots it again. See docs/decisions/vm-boot.md.
		RestartPolicy: computev1alpha1.RestartPolicy_RESTART_POLICY_ON_FAILURE,
		// As a container's: one on default if it lists none, each in the
		// default security group if it names none.
		Interfaces: machineInterfaces(spec.GetInterfaces()),
	}
	for _, d := range spec.GetDisks() {
		out.Disks = append(out.Disks, &computev1alpha1.MachineDisk{Disk: d.GetDisk()})
	}
	return out, nil
}

// reportVirtualMachine writes a VM's status, conditional on vm as read:
// from a reconcile that is stuck, or else from its machine m and that
// machine's node.
func (s *Server) reportVirtualMachine(ctx context.Context, vm *computev1alpha1.VirtualMachine, m *computev1alpha1.Machine, node *computev1alpha1.Node, stuck *blocked) error {
	gen := vm.GetMetadata().GetGeneration()
	var ready, stopped *metav1.Condition
	if stuck != nil {
		ready = &metav1.Condition{Type: VMReady, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: stuck.reason, Message: stuck.message}
		stopped = &metav1.Condition{Type: VMStopped, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: stuck.reason, Message: stuck.message}
	} else {
		ready = machineReadiness(m, node)
		stopped = machineStopped(vm.GetSpec().GetRunState() == computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_STOPPED, "virtual machine", m, node)
	}
	ready.ObservedGeneration, stopped.ObservedGeneration = gen, gen
	status := &computev1alpha1.VirtualMachineStatus{}
	if vm.GetStatus() != nil {
		status = proto.Clone(vm.GetStatus()).(*computev1alpha1.VirtualMachineStatus)
	}
	status.ObservedGeneration = gen
	status.Conditions = conditions.Set(status.GetConditions(), ready, s.clock.Now())
	status.Conditions = conditions.Set(status.GetConditions(), stopped, s.clock.Now())
	status.RootDisk = vmRootDisk(vm)
	status.Interfaces = nil
	if ownedBy(m.GetMetadata(), vm.GetMetadata().GetUid()) {
		for _, nic := range m.GetSpec().GetInterfaces() {
			status.Interfaces = append(status.Interfaces, &computev1alpha1.VirtualMachineInterfaceStatus{
				Name: nic.GetName(), Network: nic.GetNetwork(), Ipv4: nic.GetIpv4(), Ipv6: nic.GetIpv6(),
			})
		}
	}
	if proto.Equal(status, vm.GetStatus()) {
		return nil
	}
	rv := vm.GetMetadata().GetResourceVersion()
	key := resource.Key{Project: vm.GetMetadata().GetProject(), Name: vm.GetMetadata().GetName()}
	_, err := s.backend(registry.VirtualMachine).UpdateStatus(s.system(ctx), key, status, &rv)
	return err
}

func decodeVM(o store.Object) (*computev1alpha1.VirtualMachine, bool) {
	var vm computev1alpha1.VirtualMachine
	return &vm, proto.Unmarshal(o.Data, &vm) == nil
}

// listVMs lists the VMs of a project, or of every project for "". A
// failure is logged, and the next resync maps everything again.
func (s *Server) listVMs(ctx context.Context, project string) []store.Object {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: registry.VirtualMachine.Kind, Project: project})
	if err != nil {
		s.log.Error("listing virtual machines failed", "error", err)
		return nil
	}
	return objs
}

func keysOf(objs []store.Object) []controller.Key {
	keys := make([]controller.Key, 0, len(objs))
	for _, o := range objs {
		keys = append(keys, controller.Key{Project: o.Project, Name: o.Name})
	}
	return keys
}

// vmsInProject maps an object to every VM in its project: a network
// appearing, or a disk changing, may unblock any of them.
func (s *Server) vmsInProject(ctx context.Context, o store.Object) []controller.Key {
	return keysOf(s.listVMs(ctx, o.Project))
}

// vmsOfImage maps an image to the VMs whose root disk is made from it.
func (s *Server) vmsOfImage(ctx context.Context, o store.Object) []controller.Key {
	var keys []controller.Key
	for _, vo := range s.listVMs(ctx, "") {
		if vm, ok := decodeVM(vo); ok && vm.GetSpec().GetRootDisk().GetFromImage().GetImage().GetName() == o.Name {
			keys = append(keys, controller.Key{Project: vo.Project, Name: vo.Name})
		}
	}
	return keys
}

// vmsOnNode maps a node to the VMs whose machines it runs.
func (s *Server) vmsOnNode(ctx context.Context, n store.Object) []controller.Key {
	var keys []controller.Key
	for _, o := range s.listMachines(ctx) {
		if m, ok := decodeMachine(o); ok && m.GetSpec().GetNode() == n.Name {
			keys = append(keys, ownerOf(o.Project, m, registry.VirtualMachine)...)
		}
	}
	return keys
}

// vmOfMachineAndNeighbours maps a machine to its VM, to the VMs on its node,
// which it changing may make room for, and to the VMs of its project, an
// address it frees may be waited on.
func (s *Server) vmOfMachineAndNeighbours(ctx context.Context, o store.Object) []controller.Key {
	m, ok := decodeMachine(o)
	if !ok {
		return nil
	}
	keys := ownerOf(o.Project, m, registry.VirtualMachine)
	if node := m.GetSpec().GetNode(); node != "" {
		keys = append(keys, s.vmsOnNode(ctx, store.Object{Key: store.Key{Name: node}})...)
	}
	return append(keys, s.vmsInProject(ctx, o)...)
}

// ownerOf is the key of m's controlling owner, if it is of kind r.
func ownerOf(project string, m *computev1alpha1.Machine, r registry.Resource) []controller.Key {
	for _, ref := range m.GetMetadata().GetOwnerReferences() {
		if ref.GetController() && ref.GetKind() == r.Kind {
			return []controller.Key{{Project: project, Name: ref.GetName()}}
		}
	}
	return nil
}
