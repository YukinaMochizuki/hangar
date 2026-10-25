package chvm

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chguest"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chvm/serialmux"
)

// A firmware VM's console helper is Cloud Hypervisor's one serial client,
// as the VM's user; see docs/decisions/serial-console.md.
const (
	// consoleFD and consoleLogFD are the helper's fds: the console socket,
	// listening, and the pipe the guest's output goes to the launcher on.
	consoleFD, consoleLogFD = 3, 4
)

// ConsoleMain is hangar-node vmm-console --serial PATH, the console helper,
// returning its exit code: it serves the console on consoleFD until it is
// signalled, dialling the serial socket at PATH.
func ConsoleMain(args []string) int {
	fs := flag.NewFlagSet("vmm-console", flag.ContinueOnError)
	serial := fs.String("serial", "", "Cloud Hypervisor's serial socket")
	if err := fs.Parse(args); err != nil || *serial == "" || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: hangar-node vmm-console --serial PATH")
		return ExitFailed
	}
	// A few KB/s need one CPU, and its threads are the unit's tasks.
	runtime.GOMAXPROCS(1)
	l := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ln, err := net.FileListener(os.NewFile(consoleFD, "console"))
	if err != nil {
		l.Error("vmm-console: the console socket", "error", err)
		return ExitFailed
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	runConsole(ctx, l, ln, os.NewFile(consoleLogFD, "console-log"), func(ctx context.Context) (net.Conn, error) {
		// Cloud Hypervisor makes the socket in the one directory it can
		// write in; it is dialled as its other sockets are.
		return chguest.DialSocket(ctx, *serial)
	})
	return 0
}

// AttachConsole dials the machine's console socket, as the launcher made it:
// never through a link, which only the launcher could have put there. No
// socket, or one no helper serves, as after the VM ended, is ErrNoConsole.
func (h *Hypervisor) AttachConsole(ctx context.Context, uid string) (net.Conn, error) {
	if err := checkUID(uid); err != nil {
		return nil, err
	}
	c, err := chguest.DialSocket(ctx, filepath.Join(h.dir(uid), consoleSocket))
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return nil, driver.ErrNoConsole
	}
	return c, err
}

// runConsole serves the console until ctx ends.
func runConsole(ctx context.Context, l *slog.Logger, ln net.Listener, log *os.File, dial func(context.Context) (net.Conn, error)) {
	hub := serialmux.New(log, l)
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					l.Error("vmm-console: accepting", "error", err)
				}
				return
			}
			go hub.Serve(c)
		}
	}()
	hub.Pump(ctx, dial)
	_ = log.Close()
}
