// Package sdsocket takes the sockets systemd passes a service through socket
// activation, by the names their units give them (FileDescriptorName=), as
// sd_listen_fds_with_names does. A service that was not socket-activated
// makes its own instead. The apiserver's unix sockets and the edge's :443
// and :80 come this way (deploy/host/steps/80-services.sh, 85-edge.sh).
package sdsocket

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// Sockets are the sockets systemd passed a service, by name, and say whether
// it was socket-activated at all.
type Sockets struct {
	inherited map[string]net.Listener
	activated bool
}

// listenFDsStart is the first descriptor systemd passes (SD_LISTEN_FDS_START).
const listenFDsStart = 3

// Inherit takes the sockets systemd passed and clears the variables naming
// them, so no child takes them too.
func Inherit() (*Sockets, error) {
	s := &Sockets{inherited: map[string]net.Listener{}}
	pid, fds, names := os.Getenv("LISTEN_PID"), os.Getenv("LISTEN_FDS"), os.Getenv("LISTEN_FDNAMES")
	for _, v := range []string{"LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES"} {
		_ = os.Unsetenv(v)
	}
	if pid == "" || pid != strconv.Itoa(os.Getpid()) {
		return s, nil
	}
	n, err := strconv.Atoi(fds)
	if err != nil || n < 0 {
		return nil, fmt.Errorf("LISTEN_FDS=%q is not a count of sockets", fds)
	}
	var named []string
	if names != "" {
		named = strings.Split(names, ":")
	}
	if len(named) != n {
		return nil, fmt.Errorf("systemd passed %d sockets but named %d (LISTEN_FDNAMES=%q)", n, len(named), names)
	}
	s.activated = true
	for i, name := range named {
		fd := listenFDsStart + i
		syscall.CloseOnExec(fd)
		f := os.NewFile(uintptr(fd), name)
		ln, err := net.FileListener(f)
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("socket %q systemd passed: %w", name, err)
		}
		if _, dup := s.inherited[name]; dup {
			_ = ln.Close()
			return nil, fmt.Errorf("systemd passed two sockets named %q", name)
		}
		s.inherited[name] = ln
	}
	return s, nil
}

// Unix returns the unix socket named name at path: the one systemd passed,
// or, when the service was not socket-activated, one it makes with mode.
func (s *Sockets) Unix(name, path string, mode fs.FileMode) (net.Listener, error) {
	if ln, ok := s.inherited[name]; ok {
		delete(s.inherited, name)
		if got := ln.Addr().String(); got != path {
			_ = ln.Close()
			return nil, fmt.Errorf("the %s socket systemd passed is %s, not %s", name, got, path)
		}
		return ln, nil
	}
	// Under systemd, a socket it did not pass is a unit that lost its
	// Sockets=; one made here would have the service's owner, not the
	// unit's.
	if s.activated {
		return nil, fmt.Errorf("systemd passed no socket named %q for %s", name, path)
	}
	return listenUnix(path, mode)
}

// TCP returns the TCP socket named name: the one systemd passed, or, when
// the service was not socket-activated, one it opens on addr with lc. Under
// systemd the unit says the address; addr is only for running without it.
func (s *Sockets) TCP(ctx context.Context, name, addr string, lc *net.ListenConfig) (net.Listener, error) {
	if ln, ok := s.inherited[name]; ok {
		delete(s.inherited, name)
		if _, tcp := ln.(*net.TCPListener); !tcp {
			_ = ln.Close()
			return nil, fmt.Errorf("the %s socket systemd passed is %s, not a TCP socket", name, ln.Addr())
		}
		return ln, nil
	}
	if s.activated {
		return nil, fmt.Errorf("systemd passed no socket named %q", name)
	}
	if addr == "" {
		return nil, fmt.Errorf("no %s socket: systemd passed none and no address is given", name)
	}
	if lc == nil {
		lc = &net.ListenConfig{}
	}
	return lc.Listen(ctx, "tcp", addr)
}

// Unused fails on sockets systemd passed that nothing took, which would
// otherwise sit open and never be served.
func (s *Sockets) Unused() error {
	if len(s.inherited) == 0 {
		return nil
	}
	return fmt.Errorf("systemd passed sockets no flag serves: %s", strings.Join(slices.Sorted(maps.Keys(s.inherited)), ", "))
}

// Close closes the sockets nothing took.
func (s *Sockets) Close() {
	for _, ln := range s.inherited {
		_ = ln.Close()
	}
}

func listenUnix(path string, mode fs.FileMode) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, mode); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}
