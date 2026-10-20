// Package computeapi is what the apiserver and the node agent must read
// alike in Machines, Nodes and Disks: what their status says, of which each
// writes a part and reads the other's, and which Disks a Machine's spec
// names.
package computeapi

import (
	"fmt"
	"net/netip"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
)

// VirtioDevice is how a Linux guest names the virtio disk the hypervisor
// attaches n-th, from 0: vda to vdz, then vdaa on.
func VirtioDevice(n int) string {
	name := ""
	for n++; n > 0; n = (n - 1) / 26 {
		name = string(rune('a'+(n-1)%26)) + name
	}
	return "vd" + name
}

// FirstContainerDisk is where a container machine's disks start among its
// virtio disks: after its root disk, vda, and its boot disk, vdb.
const FirstContainerDisk = 2

// MachineDisks names the Disks a machine has, in the order its node
// attaches them: the disk a firmware boot boots from, then spec.disks.
func MachineDisks(spec *computev1alpha1.MachineSpec) []string {
	var out []string
	if root := spec.GetFirmware().GetRootDisk(); root != "" {
		out = append(out, root)
	}
	for _, d := range spec.GetDisks() {
		out = append(out, d.GetDisk())
	}
	return out
}

// DiskInUseFinalizer keeps a deleted Disk until no machine lists it; the
// apiserver removes it then. A node removes a disk's volume only once it is
// gone, and reports DiskReleased.
const DiskInUseFinalizer = registry.Domain + "/disk-in-use"

// DependentsFinalizer keeps an object that cascades in place, marked as
// being deleted, until everything it owns is gone: a tenant's projects, a
// FileShare's server. A node lets go of a deleted share only once it is
// gone, and reports FileShareReleased.
const DependentsFinalizer = registry.Domain + "/dependents"

// Condition types. Each has one writer.
const (
	// NodeReady says a Node's agent is reporting. The agent sets it true with
	// every report; the apiserver turns it Unknown when the reports stop.
	NodeReady = "Ready"
	// MachineScheduled is the scheduler's: whether the machine has a node.
	// The scheduler also sets phase Pending on a machine no node has
	// reported on; the rest of a machine's status is its node's.
	MachineScheduled = "Scheduled"
	// MachineReady is the node's: whether the machine runs its current spec
	// with its guest agent answering. When false, its reason and message say
	// what stands in the way, for the machine's owner to pass on.
	MachineReady = "Ready"
	// MachineReleased is the node's: a deleted machine's VM, disks and
	// interfaces are gone from the host.
	MachineReleased = "Released"
	// DiskAllocated is the apiserver's: whether the disk's node holds room
	// for its spec.size, which status.size then says. The apiserver also
	// writes a disk's node, size, machine, attachment and phase.
	DiskAllocated = "Allocated"
	// DiskReady is the node's: the disk exists on the host at status.size.
	DiskReady = "Ready"
	// DiskReleased is the node's: a deleted disk is gone from the host.
	DiskReleased = "Released"
	// FileShareReleased is the node's: a deleted share's node holds nothing
	// of it any more.
	FileShareReleased = "Released"
	// ImageVerified is any node's: True once a node has fetched the image,
	// found it matches its sha256 and made a disk image of it; False, with
	// one of the Image reasons, when the image at that generation can never
	// be used. Nodes then try it again only once its spec changes.
	ImageVerified = "Verified"
)

// Reasons an image can never be used, for ImageVerified.
const (
	ReasonHashMismatch  = "HashMismatch"
	ReasonInvalidImage  = "InvalidImage"
	ReasonImageTooLarge = "ImageTooLarge"
)

// Reasons the node gives for MachineReady.
const (
	ReasonRunning         = "Running"
	ReasonCreating        = "Creating"
	ReasonPulling         = "Pulling"
	ReasonBooting         = "Booting"
	ReasonStopped         = "Stopped"
	ReasonImagePullFailed = "ImagePullFailed"
	// ReasonImagePullUnauthorized means the registry refused the machine's
	// login, or asked for one it has none of.
	ReasonImagePullUnauthorized = "ImagePullUnauthorized"
	ReasonDiskFailed            = "DiskFailed"
	// ReasonDiskTooSmall means the machine asks for a root disk smaller than
	// its image.
	ReasonDiskTooSmall  = "DiskTooSmall"
	ReasonNetworkFailed = "NetworkFailed"
	ReasonBootFailed    = "BootFailed"
	ReasonExited        = "Exited"
	// ReasonPoweredOff means a firmware machine's guest powered itself
	// off, which keeps it off until it is stopped and started; a firmware
	// machine that ended otherwise is Exited, and boots again.
	ReasonPoweredOff      = "PoweredOff"
	ReasonNetworkNotReady = "NetworkNotReady"
	// ReasonDiskNotReady means an attached disk is not on the node yet.
	ReasonDiskNotReady = "DiskNotReady"
)

// ParseIPv6Prefix reads the IPv6 prefix a Node reports: a global unicast
// /64 without host bits.
func ParseIPv6Prefix(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil || !p.Addr().Is6() || p.Addr().Is4In6() || p.Addr().Zone() != "" || p.Bits() != 64 || p != p.Masked() || !p.Addr().IsGlobalUnicast() {
		return netip.Prefix{}, fmt.Errorf("%q is not a global IPv6 /64 such as 2001:db8:0:1::/64", s)
	}
	return p, nil
}
