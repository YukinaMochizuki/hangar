package main

import (
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

// Names systemd gives the sockets it passes (FileDescriptorName=), set in
// deploy/host/steps/80-services.sh.
const (
	nodeSocketName  = "node"
	adminSocketName = "admin"
)

// sockets are the unix sockets the apiserver serves: those systemd passed it,
// by name, or else ones it makes itself.
type sockets struct {
	inherited map[string]net.Listener
	activated bool
}

// listenFDsStart is the first descriptor systemd passes (SD_LISTEN_FDS_START).
const listenFDsStart = 3

// inheritSockets takes the sockets systemd passed through socket activation,
// as sd_listen_fds_with_names does, and clears the variables naming them.
func inheritSockets() (*sockets, error) {
	s := &sockets{inherited: map[string]net.Listener{}}
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
			return nil, fmt.Errorf("systemd passed two sockets named %q", name)
		}
		s.inherited[name] = ln
	}
	return s, nil
}

// listen returns the socket named name at path: the one systemd passed, or,
// when the apiserver was not socket-activated, one it makes with mode.
func (s *sockets) listen(name, path string, mode fs.FileMode) (net.Listener, error) {
	if ln, ok := s.inherited[name]; ok {
		delete(s.inherited, name)
		if got := ln.Addr().String(); got != path {
			_ = ln.Close()
			return nil, fmt.Errorf("the %s socket systemd passed is %s, not %s", name, got, path)
		}
		return ln, nil
	}
	// Under systemd, a socket it did not pass is a unit that lost its
	// Sockets=; one made here would have the apiserver's owner, not the
	// unit's.
	if s.activated {
		return nil, fmt.Errorf("systemd passed no socket named %q for %s", name, path)
	}
	return listenSocket(path, mode)
}

// unused fails on sockets systemd passed that no flag asked for, which would
// otherwise sit open and never be served.
func (s *sockets) unused() error {
	if len(s.inherited) == 0 {
		return nil
	}
	return fmt.Errorf("systemd passed sockets no flag serves: %s", strings.Join(slices.Sorted(maps.Keys(s.inherited)), ", "))
}

func (s *sockets) close() {
	for _, ln := range s.inherited {
		_ = ln.Close()
	}
}

func listenSocket(path string, mode fs.FileMode) (net.Listener, error) {
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
