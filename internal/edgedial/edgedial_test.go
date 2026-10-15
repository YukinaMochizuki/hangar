package edgedial_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/edgedial"
)

// echo is a guest's server: it sends back what it reads, then says bye and
// closes once its input ends.
func echo(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
				_, _ = c.Write([]byte("bye"))
			}()
		}
	}()
	return ln.Addr().String()
}

type node struct {
	sock  string
	mu    sync.Mutex
	asked []edgedial.Target
}

// serve runs a dial server accepting uids, whose Open reaches guest for
// endpoint web/site of uid "0a1b-2c" and refuses everything else.
func serve(t *testing.T, uids []uint32, guest string) *node {
	return serveWith(t, uids, guest, 0)
}

// serveWith is serve with the server's Grace.
func serveWith(t *testing.T, uids []uint32, guest string, grace time.Duration) *node {
	t.Helper()
	dir, err := os.MkdirTemp("", "edgedial")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	n := &node{sock: filepath.Join(dir, "dial.sock")}
	ln, err := net.Listen("unix", n.sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &edgedial.Server{UIDs: uids, Grace: grace, Log: slog.New(slog.DiscardHandler), Open: func(ctx context.Context, tg edgedial.Target) (net.Conn, error) {
		n.mu.Lock()
		n.asked = append(n.asked, tg)
		n.mu.Unlock()
		if tg != (edgedial.Target{Project: "web", Name: "site", UID: "0a1b-2c"}) {
			return nil, edgedial.Refuse(http.StatusNotFound, "public endpoint %s does not exist", tg)
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", guest)
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return n
}

func (n *node) dial(ctx context.Context) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", n.sock)
}

func TestADialReachesTheEndpointBothWays(t *testing.T) {
	n := serve(t, []uint32{uint32(os.Getuid())}, echo(t))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, err := edgedial.Dial(ctx, n.dial, edgedial.Target{Project: "web", Name: "site", UID: "0a1b-2c"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	// Ending what is sent reaches the guest, whose answer still comes back.
	if err := c.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(c)
	if err != nil || string(got) != "hellobye" {
		t.Fatalf("read %q, %v", got, err)
	}
}

func TestADialNamesAnEndpointAndNothingElse(t *testing.T) {
	n := serve(t, []uint32{uint32(os.Getuid())}, echo(t))
	ctx := t.Context()
	_, err := edgedial.Dial(ctx, n.dial, edgedial.Target{Project: "web", Name: "other", UID: "0a1b-2c"})
	var de *edgedial.Error
	if !errors.As(err, &de) || de.Status != http.StatusNotFound {
		t.Errorf("an endpoint that does not exist: %v", err)
	}
	// A uid that could carry another header is never sent.
	for _, uid := range []string{"", "0a1b\r\nX-Evil: 1", "ZZZ"} {
		if _, err := edgedial.Dial(ctx, n.dial, edgedial.Target{Project: "web", Name: "site", UID: uid}); err == nil {
			t.Errorf("uid %q was sent", uid)
		}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.asked) != 1 || n.asked[0] != (edgedial.Target{Project: "web", Name: "other", UID: "0a1b-2c"}) {
		t.Errorf("the node was asked for %v", n.asked)
	}
}

func TestOnlyTheEdgeDials(t *testing.T) {
	n := serve(t, []uint32{uint32(os.Getuid()) + 1}, echo(t))
	_, err := edgedial.Dial(t.Context(), n.dial, edgedial.Target{Project: "web", Name: "site", UID: "0a1b-2c"})
	var de *edgedial.Error
	if !errors.As(err, &de) || de.Status != http.StatusForbidden {
		t.Errorf("a dial from another uid: %v", err)
	}
	if len(n.asked) != 0 {
		t.Errorf("the node opened %v for another uid", n.asked)
	}
}

// TestAGuestThatNeverClosesIsClosedAfterTheGrace: once the edge's side has
// ended, a guest that never ends its own is closed after the grace.
func TestAGuestThatNeverClosesIsClosedAfterTheGrace(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	guestClosed := make(chan struct{})
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		// Reads everything, answers nothing, closes nothing.
		_, _ = io.Copy(io.Discard, c)
		_, _ = c.Read(make([]byte, 1))
		<-guestClosed
		_ = c.Close()
	}()
	t.Cleanup(func() { close(guestClosed) })
	n := serveWith(t, []uint32{uint32(os.Getuid())}, ln.Addr().String(), 300*time.Millisecond)
	c, err := edgedial.Dial(t.Context(), n.dial, edgedial.Target{Project: "web", Name: "site", UID: "0a1b-2c"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := c.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = c.Read(make([]byte, 1))
	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("the guest's side was never closed")
	}
	if took := time.Since(start); took < 200*time.Millisecond {
		t.Errorf("closed after %v, before the grace", took)
	}
}
