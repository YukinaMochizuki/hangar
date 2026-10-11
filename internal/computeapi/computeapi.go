// Package computeapi names what the status of Machines and Nodes says, for
// the apiserver and the node agent, which write different parts of it and
// read each other's.
package computeapi

// Condition types. Each has one writer.
const (
	// NodeReady says a Node's agent is reporting. The agent sets it true with
	// every report; the apiserver turns it Unknown when the reports stop.
	NodeReady = "Ready"
)
