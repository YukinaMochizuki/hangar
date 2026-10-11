package chguest_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1/guestv1connect"
	"github.com/YukinaMochizuki/hangar/internal/guest/agent"
	"github.com/YukinaMochizuki/hangar/internal/guest/supervise"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chguest"
)

// fakeCH plays Cloud Hypervisor's side of a hybrid vsock socket, bridging
// connections to port to an agent served in this process.
func fakeCH(t *testing.T, port uint32) string {
	t.Helper()
	return serveCH(t, port, &agent.Server{Reaper: reaper(), Root: "/", Version: "test", Started: time.Now()}, func() {})
}

// reaper is the test binary's one reaper, shared by every fake guest.
var reaper = sync.OnceValue(supervise.NewReaper)

// serveCH is fakeCH serving h.
func serveCH(t *testing.T, port uint32, h guestv1connect.GuestAgentServiceHandler, cleanup func()) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ch")
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "vsock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	_, handler := guestv1connect.NewGuestAgentServiceHandler(h, connect.WithReadMaxBytes(agent.MaxMessage))
	protocols := &http.Protocols{}
	protocols.SetUnencryptedHTTP2(true)
	guest := &chanListener{conns: make(chan net.Conn), done: make(chan struct{}), addr: ln.Addr()}
	srv := &http.Server{Handler: handler, Protocols: protocols}
	go func() { _ = srv.Serve(guest) }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			line := make([]byte, 0, 32)
			b := make([]byte, 1)
			for {
				if _, err := c.Read(b); err != nil || b[0] == '\n' {
					break
				}
				line = append(line, b[0])
			}
			if string(line) != fmt.Sprintf("CONNECT %d", port) {
				_ = c.Close() // as Cloud Hypervisor does when no one listens there
				continue
			}
			_, _ = c.Write([]byte("OK 1073741824\n"))
			guest.conns <- c
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		_ = srv.Close()
		cleanup()
		_ = os.RemoveAll(dir)
	})
	return socket
}

type chanListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
	addr  net.Addr
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return l.addr }

func guestOn(socket string) chguest.Guest {
	return chguest.Guest{Socket: func(string) string { return socket }}
}

func TestReadyPingsTheAgent(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second) // a hang fails here, by name
	defer cancel()
	if err := guestOn(fakeCH(t, agent.Port)).Ready(ctx, "m1"); err != nil {
		t.Errorf("a guest whose agent answers: %v", err)
	}
	if err := guestOn(fakeCH(t, agent.Port+1)).Ready(ctx, "m1"); err == nil {
		t.Error("ready with nothing listening on the agent's port")
	}
	if err := guestOn(filepath.Join(t.TempDir(), "none")).Ready(ctx, "m1"); err == nil {
		t.Error("ready with no VM")
	}
	if err := guestOn(answering(t, "ERR no such port\n")).Ready(ctx, "m1"); err == nil || !strings.Contains(err.Error(), "ERR no such port") {
		t.Errorf("a socket that answers something other than OK: %v", err)
	}
}

// TestASignalIsNotHeldUpByStdin: Ctrl-C reaches a process that is not
// reading what was pasted into it.
func TestASignalIsNotHeldUpByStdin(t *testing.T) {
	_, _, exit, err := run(t, t.Context(), &guestv1.ExecStart{Process: process("sleep", "30")},
		func(s *connect.BidiStreamForClient[guestv1.ExecRequest, guestv1.ExecResponse]) {
			chunk := make([]byte, 32<<10)
			for range 32 { // 1 MiB, far past what a pipe holds
				_ = s.Send(&guestv1.ExecRequest{Message: &guestv1.ExecRequest_Stdin{Stdin: chunk}})
			}
			_ = s.Send(&guestv1.ExecRequest{Message: &guestv1.ExecRequest_Signal{Signal: 15}})
		})
	if err != nil || exit.GetCode() != 143 {
		t.Errorf("exit %v err %v", exit, err)
	}
}

func TestAProgramThatCannotRunEndsAsInAShell(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "not-executable"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		program string
		code    int32
	}{
		{"no-such-program", 127},
		{filepath.Join(dir, "nowhere"), 127},
		{filepath.Join(dir, "not-executable"), 126},
	} {
		_, stderr, exit, err := run(t, t.Context(), &guestv1.ExecStart{Process: process(tt.program)}, nil)
		if err != nil || exit.GetCode() != tt.code || !strings.Contains(stderr, filepath.Base(tt.program)) {
			t.Errorf("%s: stderr %q exit %v err %v", tt.program, stderr, exit, err)
		}
	}
}

func TestATerminalCanBeResized(t *testing.T) {
	stdout, _, exit, err := run(t, t.Context(), &guestv1.ExecStart{
		Process: process("sh", "-c", "read x; stty size"), Tty: true, Size: &guestv1.WindowSize{Rows: 40, Cols: 100},
	}, func(s *connect.BidiStreamForClient[guestv1.ExecRequest, guestv1.ExecResponse]) {
		_ = s.Send(&guestv1.ExecRequest{Message: &guestv1.ExecRequest_Resize{Resize: &guestv1.WindowSize{Rows: 50, Cols: 120}}})
		_ = s.Send(&guestv1.ExecRequest{Message: &guestv1.ExecRequest_Stdin{Stdin: []byte("\n")}})
	})
	if err != nil || exit.GetCode() != 0 || !strings.Contains(stdout, "50 120") {
		t.Errorf("stdout %q exit %v err %v", stdout, exit, err)
	}
}

// run execs p and collects what comes back; send is called with the
// stream once it has started.
func run(t *testing.T, ctx context.Context, start *guestv1.ExecStart, send func(*connect.BidiStreamForClient[guestv1.ExecRequest, guestv1.ExecResponse])) (stdout, stderr string, exit *guestv1.ExecExit, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second) // a hang fails here, by name
	defer cancel()
	c, done := guestOn(fakeCH(t, agent.Port)).Client("m1")
	defer done()
	stream := c.Exec(ctx)
	if err := stream.Send(&guestv1.ExecRequest{Message: &guestv1.ExecRequest_Start{Start: start}}); err != nil {
		return "", "", nil, err
	}
	if send != nil {
		send(stream)
	}
	var out, errb strings.Builder
	for {
		res, err := stream.Receive()
		if err != nil {
			return out.String(), errb.String(), exit, err
		}
		switch m := res.GetMessage().(type) {
		case *guestv1.ExecResponse_Stdout:
			out.Write(m.Stdout)
		case *guestv1.ExecResponse_Stderr:
			errb.Write(m.Stderr)
		case *guestv1.ExecResponse_Exit:
			exit = m.Exit
			_ = stream.CloseRequest()
			_ = stream.CloseResponse()
			return out.String(), errb.String(), exit, nil
		}
	}
}

func process(args ...string) *guestv1.Process { return &guestv1.Process{Args: args} }

func TestExecRunsAProcess(t *testing.T) {
	stdout, stderr, exit, err := run(t, t.Context(),
		&guestv1.ExecStart{Process: process("sh", "-c", `read x; echo "got $x"; echo oops >&2; exit 7`)},
		func(s *connect.BidiStreamForClient[guestv1.ExecRequest, guestv1.ExecResponse]) {
			_ = s.Send(&guestv1.ExecRequest{Message: &guestv1.ExecRequest_Stdin{Stdin: []byte("hi\n")}})
		})
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "got hi\n" || stderr != "oops\n" || exit.GetCode() != 7 {
		t.Errorf("stdout %q stderr %q exit %v", stdout, stderr, exit)
	}
}

func TestStdinCanBeClosed(t *testing.T) {
	stdout, _, exit, err := run(t, t.Context(), &guestv1.ExecStart{Process: process("cat")},
		func(s *connect.BidiStreamForClient[guestv1.ExecRequest, guestv1.ExecResponse]) {
			_ = s.Send(&guestv1.ExecRequest{Message: &guestv1.ExecRequest_Stdin{Stdin: []byte("abc")}})
			_ = s.Send(&guestv1.ExecRequest{Message: &guestv1.ExecRequest_StdinClose{StdinClose: &guestv1.StdinClose{}}})
		})
	if err != nil || stdout != "abc" || exit.GetCode() != 0 {
		t.Errorf("stdout %q exit %v err %v", stdout, exit, err)
	}
}

func TestExecOnATerminal(t *testing.T) {
	stdout, _, exit, err := run(t, t.Context(), &guestv1.ExecStart{
		Process: process("sh", "-c", "tty; stty size"), Tty: true, Size: &guestv1.WindowSize{Rows: 40, Cols: 100},
	}, nil)
	if err != nil || exit.GetCode() != 0 {
		t.Fatalf("exit %v err %v", exit, err)
	}
	if !strings.Contains(stdout, "/dev/pts/") || !strings.Contains(stdout, "40 100") {
		t.Errorf("on a terminal: %q", stdout)
	}
}

func TestASignalReachesTheProcess(t *testing.T) {
	_, _, exit, err := run(t, t.Context(), &guestv1.ExecStart{Process: process("sleep", "30")},
		func(s *connect.BidiStreamForClient[guestv1.ExecRequest, guestv1.ExecResponse]) {
			time.Sleep(100 * time.Millisecond)
			_ = s.Send(&guestv1.ExecRequest{Message: &guestv1.ExecRequest_Signal{Signal: 15}})
		})
	if err != nil || exit.GetCode() != 143 || exit.GetSignal() != 15 {
		t.Errorf("exit %v err %v", exit, err)
	}
}

// TestAChildLeftBehindHoldsUpNothing: a process that exits leaving a child
// holding its output still ends the call, soon.
func TestAChildLeftBehindHoldsUpNothing(t *testing.T) {
	began := time.Now()
	stdout, _, exit, err := run(t, t.Context(), &guestv1.ExecStart{Process: process("sh", "-c", "sleep 30 & echo started")}, nil)
	if err != nil || exit.GetCode() != 0 || stdout != "started\n" {
		t.Errorf("stdout %q exit %v err %v", stdout, exit, err)
	}
	if took := time.Since(began); took > 10*time.Second {
		t.Errorf("the call took %s", took)
	}
}

// TestACallerGoingAwayKillsTheProcess: an exec whose caller is gone leaves
// nothing running.
func TestACallerGoingAwayKillsTheProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	c, done := guestOn(fakeCH(t, agent.Port)).Client("m1")
	defer done()
	stream := c.Exec(ctx)
	if err := stream.Send(&guestv1.ExecRequest{Message: &guestv1.ExecRequest_Start{Start: &guestv1.ExecStart{
		Process: process("sh", "-c", "echo $$; exec sleep 30"),
	}}}); err != nil {
		t.Fatal(err)
	}
	res, err := stream.Receive()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(res.GetStdout())))
	if err != nil {
		t.Fatalf("pid %q: %v", res.GetStdout(), err)
	}
	cancel() // and nothing else: the client closes what HTTP/2 needs closed
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); errors.Is(err, os.ErrNotExist) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d still there after its caller went away", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
