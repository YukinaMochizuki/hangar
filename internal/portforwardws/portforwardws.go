// Package portforwardws is the wire of a port forward to the apiserver: a
// WebSocket of one TCP connection's bytes, on a machine capability's route
// with the port in its query, failing as an exec's does (internal/execws).
// See docs/decisions/port-forward.md.
package portforwardws

import (
	"net/url"
	"strconv"

	"github.com/YukinaMochizuki/hangar/internal/execws"
)

// Subprotocol is the WebSocket subprotocol of a port forward; the server
// refuses a client that does not ask for it.
const Subprotocol = "hangar.portforward.v1alpha1"

// MaxMessage bounds a message either way.
const MaxMessage = 64 << 10

// Action is the suffix of a port forward's path.
const Action = "portforward"

// PortParam is the query parameter naming the port.
const PortParam = "port"

// Path is the port forward to port of the object of the kind plural, such
// as containers, named name.
func Path(project, plural, name string, port uint16) string {
	return execws.CapabilityPath(project, plural, name, Action) + "?" + url.Values{PortParam: {strconv.Itoa(int(port))}}.Encode()
}
