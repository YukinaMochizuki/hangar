// Package sshws is the wire of hangar ssh-proxy to the apiserver: a WebSocket
// of the SSH connection's bytes, on a machine capability's route and failing
// as an exec's does (internal/execws). See docs/decisions/vm-ssh.md.
package sshws

import "github.com/YukinaMochizuki/hangar/internal/execws"

// Subprotocol is the WebSocket subprotocol of the tunnel; the server refuses
// a client that does not ask for it.
const Subprotocol = "hangar.ssh.v1alpha1"

// MaxMessage bounds a message either way.
const MaxMessage = 64 << 10

// Action is the suffix of an SSH tunnel's path.
const Action = "ssh"

// Path is the SSH endpoint of the object of the kind plural, such as
// virtualmachines, named name.
func Path(project, plural, name string) string {
	return execws.CapabilityPath(project, plural, name, Action)
}
