// Package computeapi names what the status of Machines, Nodes and Disks
// says, for the apiserver and the node agent, which write different parts of
// it and read each other's.
package computeapi

import "github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"

// DiskInUseFinalizer keeps a deleted Disk until no machine lists it; the
// apiserver removes it then. A node removes a disk's volume only once it is
// gone, and reports DiskReleased.
const DiskInUseFinalizer = registry.Domain + "/disk-in-use"

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
	// writes a disk's node, size, machine and phase.
	DiskAllocated = "Allocated"
	// DiskReady is the node's: the disk exists on the host at status.size.
	DiskReady = "Ready"
	// DiskReleased is the node's: a deleted disk is gone from the host.
	DiskReleased = "Released"
)

// Reasons the node gives for MachineReady.
const (
	ReasonRunning         = "Running"
	ReasonCreating        = "Creating"
	ReasonPulling         = "Pulling"
	ReasonBooting         = "Booting"
	ReasonStopped         = "Stopped"
	ReasonImagePullFailed = "ImagePullFailed"
	ReasonDiskFailed      = "DiskFailed"
	// ReasonDiskTooSmall means the machine asks for a root disk smaller than
	// its image.
	ReasonDiskTooSmall    = "DiskTooSmall"
	ReasonNetworkFailed   = "NetworkFailed"
	ReasonBootFailed      = "BootFailed"
	ReasonExited          = "Exited"
	ReasonNetworkNotReady = "NetworkNotReady"
	// ReasonDiskNotReady means an attached disk is not on the node yet.
	ReasonDiskNotReady = "DiskNotReady"
)
