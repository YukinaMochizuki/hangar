package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/mdlayher/vsock"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1/guestv1connect"
	"github.com/YukinaMochizuki/hangar/internal/guest/supervise"
)

// peer is the agent's end of a connection, from the vsock address given.
type peer struct {
	net.Conn
	remote net.Addr
}

func (p peer) RemoteAddr() net.Addr { return p.remote }

// pipeListener accepts the agent's ends of in-memory connections.
type pipeListener struct {
	conns chan net.Conn
	once  sync.Once
	done  chan struct{}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *pipeListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *pipeListener) Addr() net.Addr { return &vsock.Addr{ContextID: 3, Port: 1024} }

// dial connects to the agent as a peer at cid.
func (l *pipeListener) dial(cid uint32) net.Conn {
	ours, theirs := net.Pipe()
	l.conns <- peer{Conn: theirs, remote: &vsock.Addr{ContextID: cid, Port: 50000}}
	return ours
}

// ping asks the agent over one connection from a peer at cid.
func (l *pipeListener) ping(cid uint32) error {
	conn := l.dial(cid)
	defer func() { _ = conn.Close() }()
	protocols := &http.Protocols{}
	protocols.SetUnencryptedHTTP2(true)
	tr := &http.Transport{Protocols: protocols, DialContext: func(context.Context, string, string) (net.Conn, error) { return conn, nil }}
	defer tr.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := guestv1connect.NewGuestAgentServiceClient(&http.Client{Transport: tr}, "http://guest").
		Ping(ctx, connect.NewRequest(&guestv1.PingRequest{}))
	return err
}

// The agent hangar-init serves answers the host and refuses a peer in the
// guest, through the same serveAgent the guest runs.
func TestTheAgentServesTheHostAlone(t *testing.T) {
	ln := &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
	defer func() { _ = ln.Close() }()
	var mu sync.Mutex
	var logged []string
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	serveAgent(ln, supervise.NewReaper(), &supervise.Stopper{}, nil, nil, time.Now(), logf)

	for _, cid := range []uint32{vsock.Local, 3} {
		if err := ln.ping(cid); err == nil {
			t.Errorf("a peer at CID %d was answered", cid)
		}
	}
	if err := ln.ping(vsock.Host); err != nil {
		t.Errorf("the host was not answered: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logged) == 0 || !strings.Contains(logged[0], "refused") {
		t.Errorf("hangar-init logged %q, want a refusal", logged)
	}
}
