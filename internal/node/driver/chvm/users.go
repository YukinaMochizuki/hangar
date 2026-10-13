package chvm

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// UserRange is a range of ids, uids and the gids of the same numbers:
// First, and Count of them in all.
type UserRange struct{ First, Count uint32 }

// DefaultVMUsers are the ids the VMs run as unless Config.VMUsers says
// otherwise. deploy/host keeps them from every user and group of the host.
var DefaultVMUsers = UserRange{First: 200000, Count: 65536}

func (r UserRange) has(id uint32) bool { return id >= r.First && id-r.First < r.Count }

// vmUsersName is the name deploy/host keeps the VMs' ids under, in
// /etc/subuid and /etc/subgid, so no user is given them as its own.
const vmUsersName = "hangar-vms"

// checkFree refuses a range a user or group in etc has an id in, or that a
// subordinate range in etc overlaps but the VMs' own: a VM could reach
// what is that user's, or a container's.
func (r UserRange) checkFree(etc string) error {
	for _, f := range []struct {
		name   string
		fields []int // the fields holding ids
	}{{"passwd", []int{2, 3}}, {"group", []int{2}}} {
		lines, err := readLines(filepath.Join(etc, f.name))
		if err != nil {
			return err
		}
		for _, l := range lines {
			parts := strings.Split(l, ":")
			for _, i := range f.fields {
				if i >= len(parts) {
					continue
				}
				if id, err := strconv.ParseUint(parts[i], 10, 32); err == nil && r.has(uint32(id)) {
					return fmt.Errorf("%s %s has id %d, one of the VMs' (%d+%d)", f.name, parts[0], id, r.First, r.Count)
				}
			}
		}
	}
	for _, name := range []string{"subuid", "subgid"} {
		lines, err := readLines(filepath.Join(etc, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, l := range lines {
			parts := strings.Split(l, ":")
			if len(parts) != 3 {
				continue
			}
			start, err1 := strconv.ParseUint(parts[1], 10, 32)
			count, err2 := strconv.ParseUint(parts[2], 10, 32)
			if err1 != nil || err2 != nil || parts[0] == vmUsersName {
				continue
			}
			if start < uint64(r.First)+uint64(r.Count) && uint64(r.First) < start+count {
				return fmt.Errorf("%s gives %s %d+%d, which overlaps the VMs' ids (%d+%d)", name, parts[0], start, count, r.First, r.Count)
			}
		}
	}
	return nil
}

// readLines is a file's lines but comments and blank ones.
func readLines(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out, nil
}

func (r UserRange) check() error {
	// Below 1000 are the host's own users; past 2^31-1 ids read as
	// negative to much of userspace.
	if r.Count == 0 || r.First < 1000 || uint64(r.First)+uint64(r.Count) > 1<<31 {
		return fmt.Errorf("VM users %d+%d: not a range of ids from 1000 to 2^31", r.First, r.Count)
	}
	return nil
}

// vmUser returns the id the machine's VM is to run as: the one its spec
// file holds, else the lowest no spec file holds and no process runs as.
// The caller holds h.users and Dir's lock until it has written the spec.
func (h *Hypervisor) vmUser(uid string) (uint32, error) {
	entries, err := os.ReadDir(h.cfg.Dir)
	if err != nil {
		return 0, err
	}
	held := map[uint32]bool{}
	var mine uint32
	for _, e := range entries {
		if !e.IsDir() || checkUID(e.Name()) != nil {
			continue
		}
		ln, err := readLaunch(filepath.Join(h.cfg.Dir, e.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			// It may hold an id: none is given out until it is read.
			return 0, err
		}
		switch {
		case ln.VMUser == 0:
		case e.Name() == uid:
			mine = ln.VMUser
		default:
			held[ln.VMUser] = true
		}
	}
	if mine != 0 && h.cfg.VMUsers.has(mine) && !held[mine] {
		return mine, nil
	}
	running, err := h.procUsers()
	if err != nil {
		return 0, fmt.Errorf("listing the ids processes run as: %w", err)
	}
	for id := range h.cfg.VMUsers.Count {
		id += h.cfg.VMUsers.First
		if !held[id] && !running[id] {
			return id, nil
		}
	}
	return 0, fmt.Errorf("all %d VM users are taken", h.cfg.VMUsers.Count)
}

// procUsers lists every uid and gid a process runs as, real, effective,
// saved or filesystem's.
func procUsers() (map[uint32]bool, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	ids := map[uint32]bool{}
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "status"))
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue // it has exited
		}
		if err != nil {
			return nil, err
		}
		if err := statusIDs(b, ids); err != nil {
			return nil, fmt.Errorf("/proc/%s/status: %w", e.Name(), err)
		}
	}
	return ids, nil
}

// statusIDs adds the ids of a /proc/PID/status's Uid: and Gid: lines.
func statusIDs(status []byte, ids map[uint32]bool) error {
	found := 0
	sc := bufio.NewScanner(bytes.NewReader(status))
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok || key != "Uid" && key != "Gid" {
			continue
		}
		found++
		for _, f := range strings.Fields(rest) {
			id, err := strconv.ParseUint(f, 10, 32)
			if err != nil {
				return err
			}
			ids[uint32(id)] = true
		}
	}
	if found != 2 {
		return errors.New("no Uid: and Gid: lines")
	}
	return sc.Err()
}
