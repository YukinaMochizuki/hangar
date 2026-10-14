// Package sshws is the wire of hangar ssh-proxy to the apiserver: a WebSocket
// of the SSH connection's bytes, failing as an exec's does (internal/execws).
// See docs/decisions/vm-ssh.md.
package sshws

import (
	"net/http"
	"net/url"
	"strings"
)

// Subprotocol is the WebSocket subprotocol of the tunnel; the server refuses
// a client that does not ask for it.
const Subprotocol = "hangar.ssh.v1alpha1"

// MaxMessage bounds a message either way.
const MaxMessage = 64 << 10

// Path is the SSH endpoint of a virtual machine.
func Path(project, name string) string {
	return "/v1/projects/" + url.PathEscape(project) + "/virtualmachines/" + url.PathEscape(name) + ":ssh"
}

// Pattern is Path as an http.ServeMux pattern: a wildcard cannot share a
// segment with ":ssh".
const Pattern = "GET /v1/projects/{project}/virtualmachines/{nameSSH}"

// NameOf returns the VM's name in a request to Pattern, if it is the SSH
// endpoint's.
func NameOf(r *http.Request) (string, bool) {
	return strings.CutSuffix(r.PathValue("nameSSH"), ":ssh")
}
