package computeapi

// TailnetJoined is the condition a container machine with a tailnet
// carries, and its container with it: True once its guest is a node of the
// tailnet, False with one of the reasons below otherwise. It never holds
// the container back (docs/decisions/container-tailscale.md).
const TailnetJoined = "TailnetJoined"

// Reasons of TailnetJoined.
const (
	// ReasonTailnetJoined is a guest that is a node of its tailnet.
	ReasonTailnetJoined = "Joined"
	// ReasonTailnetKeyRejected is a guest whose auth key the tailnet
	// refused.
	ReasonTailnetKeyRejected = "KeyRejected"
	// ReasonTailnetNoKey is a guest that needs the auth key, whose Secret,
	// or its authkey, is missing.
	ReasonTailnetNoKey = "NoKey"
	// ReasonTailnetStarting is a container not running yet, a tailscaled
	// starting, or a join under way.
	ReasonTailnetStarting = "Starting"
	// ReasonTailnetFailed is a join, or tailscaled, failed otherwise.
	ReasonTailnetFailed = "Failed"
)
