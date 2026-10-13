//go:build !linux

package execws

import "syscall"

// Control leaves the socket as it is where TCP has no user timeout to set;
// KeepAlive and the WebSocket liveness still notice a dead peer.
func Control(_, _ string, _ syscall.RawConn) error { return nil }
