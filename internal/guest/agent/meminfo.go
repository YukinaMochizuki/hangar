package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"connectrpc.com/connect"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
)

// Memory is how much memory the guest's kernel has and how much of it is
// available, in bytes, as MemoryUsage says it.
type Memory struct {
	Total, Available uint64
}

// MemoryUsage says how much memory the guest has and how much is available.
func (s *Server) MemoryUsage(context.Context, *connect.Request[guestv1.MemoryUsageRequest]) (*connect.Response[guestv1.MemoryUsageResponse], error) {
	if s.StatMemory == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("this guest does not say its memory"))
	}
	m, err := s.StatMemory()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&guestv1.MemoryUsageResponse{Total: m.Total, Available: m.Available}), nil
}

// ReadMeminfo reads MemTotal and MemAvailable from the meminfo file at
// path, /proc/meminfo in a guest.
func ReadMeminfo(path string) (Memory, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Memory{}, err
	}
	return ParseMeminfo(b)
}

// ParseMeminfo reads MemTotal and MemAvailable from what /proc/meminfo
// holds. Both must be there, in kB, and available cannot exceed total.
func ParseMeminfo(b []byte) (Memory, error) {
	var m Memory
	var haveTotal, haveAvailable bool
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok || (name != "MemTotal" && name != "MemAvailable") {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) != 2 || fields[1] != "kB" {
			return Memory{}, fmt.Errorf("meminfo's %s is %q, not a number of kB", name, strings.TrimSpace(rest))
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil || kb > ^uint64(0)>>10 {
			return Memory{}, fmt.Errorf("meminfo's %s is %q, not a number of kB", name, strings.TrimSpace(rest))
		}
		if name == "MemTotal" {
			m.Total, haveTotal = kb<<10, true
		} else {
			m.Available, haveAvailable = kb<<10, true
		}
	}
	if err := sc.Err(); err != nil {
		return Memory{}, err
	}
	if !haveTotal || !haveAvailable {
		return Memory{}, errors.New("meminfo lacks MemTotal or MemAvailable")
	}
	if m.Available > m.Total {
		return Memory{}, fmt.Errorf("meminfo has %d bytes available of %d", m.Available, m.Total)
	}
	return m, nil
}
