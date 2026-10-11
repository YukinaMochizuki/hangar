package auth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"slices"

	"golang.org/x/sys/unix"
)

type peerCredKey struct{}

// ConnContext records the peer credentials of unix socket connections. Use it
// as the http.Server's ConnContext on the node listener.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return ctx
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return ctx
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || credErr != nil {
		return ctx
	}
	return context.WithValue(ctx, peerCredKey{}, cred)
}

// PeerCred authenticates processes on the node's unix socket by the uid the
// kernel reports for the peer. The socket is local to one host, so whoever
// passes is that host's node agent.
type PeerCred struct {
	// NodeUIDs are the uids accepted as the node agent.
	NodeUIDs []uint32
	// NodeName is the Node the agent acts as; it becomes the principal's ID.
	NodeName string
}

func (p PeerCred) Authenticate(ctx context.Context, _ http.Header) (Principal, bool, error) {
	cred, ok := ctx.Value(peerCredKey{}).(*unix.Ucred)
	if !ok {
		return Principal{}, false, nil
	}
	if !slices.Contains(p.NodeUIDs, cred.Uid) {
		return Principal{}, false, errors.New("peer uid is not a node agent")
	}
	if p.NodeName == "" {
		return Principal{}, false, errors.New("the node socket has no node name configured")
	}
	return Principal{Type: Node, ID: p.NodeName}, true, nil
}
