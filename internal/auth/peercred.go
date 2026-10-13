package auth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"

	"golang.org/x/sys/unix"
)

// Each listener records its peers' credentials under its own key, so an
// authenticator only ever sees the credentials of the listener it was meant
// for, whatever chain it is put in.
type (
	nodePeerKey  struct{}
	adminPeerKey struct{}
)

// ConnContext records the peer credentials of unix socket connections. Use it
// as the http.Server's ConnContext on the node listener.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	return withPeer(ctx, c, nodePeerKey{})
}

// AdminConnContext records the peer credentials of unix socket connections
// for LocalRoot. Use it as the http.Server's ConnContext on the admin
// listener, and on no other.
func AdminConnContext(ctx context.Context, c net.Conn) context.Context {
	return withPeer(ctx, c, adminPeerKey{})
}

// AdminPeer returns the credentials AdminConnContext recorded for the
// request's connection.
func AdminPeer(ctx context.Context) (*unix.Ucred, bool) {
	cred, ok := ctx.Value(adminPeerKey{}).(*unix.Ucred)
	return cred, ok
}

func withPeer(ctx context.Context, c net.Conn, key any) context.Context {
	cred, err := PeerOf(c)
	if err != nil {
		return ctx
	}
	return context.WithValue(ctx, key, cred)
}

// PeerOf returns the credentials the kernel reports for the process at the
// other end of a unix socket connection.
func PeerOf(c net.Conn) (*unix.Ucred, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return nil, errors.New("not a unix socket connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return nil, err
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return nil, err
	}
	return cred, credErr
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
	cred, ok := ctx.Value(nodePeerKey{}).(*unix.Ucred)
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

// LocalRoot authenticates root on the host's admin socket, the break-glass
// way in that works whatever the public listener's auth does. It reads only
// what AdminConnContext recorded, and no header, so no other listener can
// produce its principal. See docs/decisions/authentication.md.
type LocalRoot struct {
	// Host names the host in the principal, such as root@<node>.
	Host string
	// UID is the uid accepted; zero, root, everywhere but in tests.
	UID uint32
}

func (l LocalRoot) Authenticate(ctx context.Context, _ http.Header) (Principal, bool, error) {
	cred, ok := AdminPeer(ctx)
	if !ok {
		return Principal{}, false, nil
	}
	if cred.Uid != l.UID {
		return Principal{}, false, fmt.Errorf("peer uid %d is not root; the admin socket is root's only", cred.Uid)
	}
	if l.Host == "" {
		return Principal{}, false, errors.New("the admin socket has no host name configured")
	}
	return Principal{Type: Local, ID: "root@" + l.Host}, true, nil
}
