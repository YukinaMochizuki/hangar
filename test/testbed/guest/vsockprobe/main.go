//go:build testbed

// Command vsockprobe is the USER nobody container's process in
// TestOnlyTheHostReachesTheAgent: it tries the guest agent at each CID given,
// prints what each did, and waits to be stopped (docs/runbook/testbed.md).
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1/guestv1connect"
	"github.com/YukinaMochizuki/hangar/internal/guest/agent"
)

// Each argument is CID or CID:PORT, the port the agent's by default. For
// each it prints one line, "vsockprobe: cid C port P: STEP RESULT":
//
//	socket ERRNO       no vsock socket could be made
//	connect ERRNO      the connection failed
//	ping ok VERSION    the agent answered
//	ping ERROR         connected, but no answer
//	read LINE          connected to another port, which said LINE
func main() {
	fmt.Printf("vsockprobe: uid %d\n", os.Geteuid())
	for _, arg := range os.Args[1:] {
		c, p, ok := strings.Cut(arg, ":")
		if !ok {
			p = strconv.Itoa(agent.Port)
		}
		cid, err1 := strconv.ParseUint(c, 10, 32)
		port, err2 := strconv.ParseUint(p, 10, 32)
		if err1 != nil || err2 != nil {
			fmt.Printf("vsockprobe: bad target %q\n", arg)
			continue
		}
		fmt.Printf("vsockprobe: cid %d port %d: %s\n", cid, port, try(uint32(cid), uint32(port)))
	}
	fmt.Println("vsockprobe: done")
	// Until Stop's SIGTERM, which ends it by default.
	for {
		time.Sleep(time.Hour)
	}
}

func errno(err error) string {
	var e syscall.Errno
	if errors.As(err, &e) {
		if name := unix.ErrnoName(e); name != "" {
			return name
		}
	}
	return err.Error()
}

func try(cid, port uint32) string {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return "socket " + errno(err)
	}
	connected := make(chan error, 1)
	go func() { connected <- unix.Connect(fd, &unix.SockaddrVM{CID: cid, Port: port}) }()
	select {
	case err := <-connected:
		if err != nil {
			_ = unix.Close(fd)
			return "connect " + errno(err)
		}
	case <-time.After(10 * time.Second):
		// The kernel's own connect timeout is 2s; the fd is left to the
		// stuck goroutine.
		return "connect hung"
	}
	// Non-blocking, so the runtime polls it and deadlines work.
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return "connect " + errno(err)
	}
	conn := &fileConn{File: os.NewFile(uintptr(fd), "vsock"), remote: &vsock.Addr{ContextID: cid, Port: port}}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if port != agent.Port {
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			return "read failed: " + err.Error()
		}
		return "read " + strings.TrimSpace(line)
	}
	return ping(conn)
}

// ping asks the agent on conn, as the node does: Connect over unencrypted
// HTTP/2.
func ping(conn net.Conn) string {
	protocols := &http.Protocols{}
	protocols.SetUnencryptedHTTP2(true)
	used := false
	tr := &http.Transport{Protocols: protocols, DialContext: func(context.Context, string, string) (net.Conn, error) {
		if used {
			return nil, errors.New("one connection only")
		}
		used = true
		return conn, nil
	}}
	defer tr.CloseIdleConnections()
	c := guestv1connect.NewGuestAgentServiceClient(&http.Client{Transport: tr}, "http://guest")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := c.Ping(ctx, connect.NewRequest(&guestv1.PingRequest{}))
	if err != nil {
		return "ping " + err.Error()
	}
	return "ping ok " + resp.Msg.GetVersion()
}

// fileConn is a connected vsock socket as a net.Conn.
type fileConn struct {
	*os.File
	remote net.Addr
}

func (c *fileConn) LocalAddr() net.Addr  { return &vsock.Addr{} }
func (c *fileConn) RemoteAddr() net.Addr { return c.remote }
