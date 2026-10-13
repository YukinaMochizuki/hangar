package chguest_test

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver/chguest"
)

// hybridVsock listens at path as Cloud Hypervisor's hybrid vsock does,
// answering each CONNECT with OK, and counts the connections it took.
func hybridVsock(t *testing.T, path string) *atomic.Int32 {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	n := new(atomic.Int32)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			go func() {
				defer func() { _ = c.Close() }()
				if _, err := bufio.NewReader(c).ReadString('\n'); err == nil {
					_, _ = c.Write([]byte("OK 1073741824\n"))
				}
				time.Sleep(time.Second)
			}()
		}
	}()
	return n
}

// TestDialTakesOnlyASocket: Dial connects to a socket at its path, and to
// nothing planted there instead: not through a link to another socket, as
// Cloud Hypervisor, which can write where its socket is, could make one;
// nor to a FIFO or a file, which it refuses as no socket.
func TestDialTakesOnlyASocket(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	own := filepath.Join(dir, "vsock")
	hybridVsock(t, own)
	c, err := chguest.Dial(ctx, own, 1024)
	if err != nil {
		t.Fatalf("dialling a socket: %v", err)
	}
	_ = c.Close()

	other := filepath.Join(dir, "other")
	taken := hybridVsock(t, other)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	if c, err := chguest.Dial(ctx, link, 1024); err == nil {
		_ = c.Close()
		t.Error("dialled another socket through a link")
	} else if !errors.Is(err, chguest.ErrNotSocket) {
		t.Errorf("a link: %v, want ErrNotSocket: the link itself, not followed", err)
	}
	if taken.Load() != 0 {
		t.Errorf("the other socket took %d connections", taken.Load())
	}

	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{fifo, file} {
		if c, err := chguest.Dial(ctx, p, 1024); err == nil {
			_ = c.Close()
			t.Errorf("dialled %s", p)
		} else if !errors.Is(err, chguest.ErrNotSocket) {
			t.Errorf("%s: %v, want ErrNotSocket", p, err)
		}
	}
}
