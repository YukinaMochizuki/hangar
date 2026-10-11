package chguest_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1/guestv1connect"
	"github.com/YukinaMochizuki/hangar/internal/guest/agent"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chguest"
)

func openFDs(t *testing.T) int {
	t.Helper()
	e, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(e)
}

// TestReadyLeavesNothingOpen: the node pings every guest over and over; each
// ping must leave no connection behind, on either side.
func TestReadyLeavesNothingOpen(t *testing.T) {
	g := guestOn(fakeCH(t, agent.Port))
	ctx := t.Context()
	if err := g.Ready(ctx, "m1"); err != nil {
		t.Fatal(err)
	}
	fds, goroutines := openFDs(t), runtime.NumGoroutine()
	for range 50 {
		if err := g.Ready(ctx, "m1"); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for openFDs(t) > fds+2 || runtime.NumGoroutine() > goroutines+5 {
		if time.Now().After(deadline) {
			t.Fatalf("after 50 pings: %d fds (from %d), %d goroutines (from %d)", openFDs(t), fds, runtime.NumGoroutine(), goroutines)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// hostile answers a ping with more than any guest should send.
type hostile struct {
	guestv1connect.UnimplementedGuestAgentServiceHandler
}

func (hostile) Ping(context.Context, *connect.Request[guestv1.PingRequest]) (*connect.Response[guestv1.PingResponse], error) {
	return connect.NewResponse(&guestv1.PingResponse{Version: strings.Repeat("x", 2*agent.MaxMessage)}), nil
}

func TestAGuestCannotSendTheNodeTooMuch(t *testing.T) {
	g := guestOn(serveCH(t, agent.Port, hostile{}, func() {}))
	err := g.Ready(t.Context(), "m1")
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Errorf("a ping answered with %d bytes: %v", 2*agent.MaxMessage, err)
	}
}

func TestPingSaysHowLongTheGuestHasBeenUp(t *testing.T) {
	c, done := guestOn(fakeCH(t, agent.Port)).Client("m1")
	defer done()
	time.Sleep(10 * time.Millisecond)
	res, err := c.Ping(t.Context(), connect.NewRequest(&guestv1.PingRequest{}))
	if err != nil || res.Msg.GetVersion() != "test" || res.Msg.GetUptime().AsDuration() < 10*time.Millisecond {
		t.Errorf("ping %v %v", res, err)
	}
}

// TestDialEndsWithItsContext: a guest that never answers the handshake
// holds up no one whose context ends, deadline or not.
func TestDialEndsWithItsContext(t *testing.T) {
	socket := answering(t, "") // never answers
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)
	dialed := make(chan error, 1)
	go func() {
		_, err := chguest.Dial(ctx, socket, agent.Port)
		dialed <- err
	}()
	select {
	case err := <-dialed:
		if err == nil {
			t.Error("dialed a guest that never answered")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dial still blocked long after its context ended")
	}
}

// answering listens where Cloud Hypervisor would, and answers every
// connection with answer, if any, and nothing more.
func answering(t *testing.T, answer string) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "vsock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu     sync.Mutex
		held   []net.Conn
		closed bool
	)
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		closed = true
		for _, c := range held {
			_ = c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte(answer))
			mu.Lock()
			if closed {
				_ = c.Close()
			} else {
				held = append(held, c)
			}
			mu.Unlock()
		}
	}()
	return socket
}
