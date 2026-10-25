// Package consolews is the wire of a virtual machine's serial console
// between a client and the apiserver: a WebSocket of proto messages,
// MachineConsoleRequest and MachineConsoleResponse, on a machine
// capability's route and failing as an exec's does (internal/execws). See
// docs/decisions/serial-console.md.
package consolews

import "github.com/YukinaMochizuki/hangar/internal/execws"

// Subprotocol is the WebSocket subprotocol of the console; the server
// refuses a client that does not ask for it.
const Subprotocol = "hangar.console.v1alpha1"

// MaxMessage bounds a message either way. Output comes in pieces of at
// most 32 KiB; typing, in what a terminal sends at once.
const MaxMessage = 64 << 10

// Action is the suffix of a console's path.
const Action = "console"

// Path is the console endpoint of the object of the kind plural, such as
// virtualmachines, named name.
func Path(project, plural, name string) string {
	return execws.CapabilityPath(project, plural, name, Action)
}
