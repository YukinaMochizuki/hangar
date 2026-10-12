// Package computeapi names what the status of Machines and Nodes says, for
// the apiserver and the node agent, which write different parts of it and
// read each other's.
package computeapi

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
)
