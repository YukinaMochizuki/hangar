package chvm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	sd "github.com/coreos/go-systemd/v22/dbus"
	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chguest"
)

// poolSlicePrefix starts the name of each memory pool's slice, inside
// machinesSlice: a dash past it would make systemd nest the slice a level
// deeper, so the pool's name, a DNS label, has its dashes as underscores,
// which no DNS label holds.
const poolSlicePrefix = "hangar-pool_"

// poolNamePattern is what names a pool: a DNS label.
var poolNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// poolSlice is the name of a pool's slice.
func poolSlice(name string) string {
	return poolSlicePrefix + strings.ReplaceAll(name, "-", "_") + ".slice"
}

// sliceOf is the slice a VM's unit runs in: its pool's, or machinesSlice.
func sliceOf(spec driver.VMSpec) string {
	if spec.MemoryPool != nil {
		return poolSlice(spec.MemoryPool.Name)
	}
	return machinesSlice
}

// poolSliceDescription is a pool's slice's description.
func poolSliceDescription(name string) string {
	return fmt.Sprintf("Hangar's memory pool %s", name)
}

// checkPool refuses a pool whose name cannot name a slice, or of no size.
func checkPool(p driver.MemoryPool) error {
	if !poolNamePattern.MatchString(p.Name) {
		return fmt.Errorf("memory pool %q cannot name a slice", p.Name)
	}
	if p.SizeMiB <= 0 {
		return fmt.Errorf("memory pool %s has a size of %d MiB", p.Name, p.SizeMiB)
	}
	return nil
}

// unitOverhead is what a VM's unit may hold past its guest's memory: its
// VMM's, its launcher's and a TPM VM's swtpm's (unitMemoryMax).
func unitOverhead(spec driver.VMSpec) uint64 {
	return unitMemoryMax(spec) - uint64(spec.MemoryMiB)<<20
}

// cgroupDir is the cgroup of a machine's VM's unit, in the slice its spec
// file says it was started in.
func (h *Hypervisor) cgroupDir(uid string) (string, error) {
	ln, err := readLaunch(h.dir(uid))
	if err != nil {
		return "", err
	}
	return h.unitCgroup(ln.Spec, uid), nil
}

// unitCgroup is the cgroup of the unit of a VM started with spec.
func (h *Hypervisor) unitCgroup(spec driver.VMSpec, uid string) string {
	dir := filepath.Join(h.cfg.cgroupRoot, machinesSlice)
	if spec.MemoryPool != nil {
		dir = filepath.Join(dir, poolSlice(spec.MemoryPool.Name))
	}
	return filepath.Join(dir, unitName(uid))
}

// poolMember is a VM whose unit runs, or is about to, in a pool's slice.
type poolMember struct {
	uid  string
	spec driver.VMSpec
}

// poolMembers lists the VMs whose units run, or wait to restart, in the
// pool's slice, by their spec files.
func (h *Hypervisor) poolMembers(ctx context.Context, name string) ([]poolMember, error) {
	units, err := h.sd.list(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing units: %w", err)
	}
	var out []poolMember
	for _, u := range units {
		uid, ok := uidOfUnit(u.Name)
		if !ok || !running(u) {
			continue
		}
		ln, err := readLaunch(h.dir(uid))
		if err != nil || ln.Spec.MemoryPool == nil || ln.Spec.MemoryPool.Name != name {
			continue
		}
		out = append(out, poolMember{uid: uid, spec: ln.Spec})
	}
	return out, nil
}

// poolProps are a pool's slice's properties, for its members and one more
// about to start, if any: a MemoryMax of its size and each member's unit's
// overhead, the backstop; and a CPUWeight of its members' together, so
// each has the share of the CPUs it would have in machinesSlice itself,
// beside the VMs in no pool. A member's weight is its cgroup's, which
// SetMemoryShare may have changed since it started.
func (h *Hypervisor) poolProps(pool driver.MemoryPool, members []poolMember, starting *driver.VMSpec) []sd.Property {
	memory := uint64(pool.SizeMiB) << 20
	var weight uint64
	for _, m := range members {
		memory += unitOverhead(m.spec)
		w, err := readUint(filepath.Join(h.unitCgroup(m.spec, m.uid), "cpu.weight"))
		if err != nil {
			w = cpuWeight(m.spec)
		}
		weight += w
	}
	if starting != nil {
		memory += unitOverhead(*starting)
		weight += cpuWeight(*starting)
	}
	return []sd.Property{
		property("MemoryMax", memory),
		noSwap(),
		property("CPUWeight", clampWeight(weight)),
	}
}

// startPool makes or updates the slice of the pool of a VM about to start,
// for it as well as the members running already. h.pools is held.
func (h *Hypervisor) startPool(ctx context.Context, spec driver.VMSpec) error {
	pool := *spec.MemoryPool
	members, err := h.poolMembers(ctx, pool.Name)
	if err != nil {
		return err
	}
	if err := h.sd.slice(ctx, poolSlice(pool.Name), poolSliceDescription(pool.Name), h.poolProps(pool, members, &spec)); err != nil {
		return fmt.Errorf("the memory pool's slice %s: %w", poolSlice(pool.Name), err)
	}
	return nil
}

func (h *Hypervisor) SetMemoryPool(ctx context.Context, pool driver.MemoryPool) error {
	if err := checkPool(pool); err != nil {
		return err
	}
	h.pools.Lock()
	defer h.pools.Unlock()
	members, err := h.poolMembers(ctx, pool.Name)
	if err != nil || len(members) == 0 {
		return err
	}
	if err := h.sd.slice(ctx, poolSlice(pool.Name), poolSliceDescription(pool.Name), h.poolProps(pool, members, nil)); err != nil {
		return fmt.Errorf("the memory pool's slice %s: %w", poolSlice(pool.Name), err)
	}
	h.watchEvents(filepath.Join(h.cfg.cgroupRoot, machinesSlice, poolSlice(pool.Name), "memory.events"))
	return nil
}

// SweepMemoryPools stops each pool's slice with no member and nothing in
// its cgroup: stopping a slice would stop what runs in it.
func (h *Hypervisor) SweepMemoryPools(ctx context.Context) error {
	h.pools.Lock()
	defer h.pools.Unlock()
	slices, err := h.sd.listSlices(ctx)
	if err != nil {
		return fmt.Errorf("listing slices: %w", err)
	}
	var errs []error
	for _, s := range slices {
		name, ok := strings.CutPrefix(strings.TrimSuffix(s, ".slice"), poolSlicePrefix)
		if !ok || !strings.HasSuffix(s, ".slice") {
			continue
		}
		name = strings.ReplaceAll(name, "_", "-")
		members, err := h.poolMembers(ctx, name)
		if err != nil {
			return err
		}
		if len(members) > 0 || hasChildCgroup(filepath.Join(h.cfg.cgroupRoot, machinesSlice, s)) {
			continue
		}
		if err := h.sd.stop(ctx, s); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s, err))
		}
	}
	return errors.Join(errs...)
}

// hasChildCgroup reports whether the cgroup dir has a cgroup in it, or
// cannot be read.
func hasChildCgroup(dir string) bool {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	for _, e := range entries {
		if e.IsDir() {
			return true
		}
	}
	return false
}

// SetMemoryShare sets a running VM's unit's CPUWeight, and its processes'
// OOM score, as its unit would have them started with the pool and the
// guarantee, where they differ. Its unit's memory.oom.group is set again,
// as systemd clears it whenever it sets a property of a unit started
// without OOMPolicy=kill.
func (h *Hypervisor) SetMemoryShare(ctx context.Context, uid string, pool *driver.MemoryPool, guaranteedMiB int64) error {
	if err := checkUID(uid); err != nil {
		return err
	}
	ln, err := readLaunch(h.dir(uid))
	if err != nil {
		return fmt.Errorf("machine %s has no running VM: %w", uid, err)
	}
	spec := ln.Spec
	spec.MemoryPool, spec.GuaranteedMiB = pool, guaranteedMiB
	cg := h.unitCgroup(ln.Spec, uid)
	have, err := readUint(filepath.Join(cg, "cpu.weight"))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("machine %s has no running VM: %w", uid, err)
	}
	if err != nil {
		return err
	}
	if want := cpuWeight(spec); have != want {
		if err := h.sd.setProps(ctx, unitName(uid), []sd.Property{property("CPUWeight", want)}); err != nil {
			return err
		}
		if err := setOOMGroup(cg); err != nil {
			return err
		}
	}
	return setOOMScores(cg, oomScore(spec))
}

// setOOMGroup sets memory.oom.group in the cgroup dir: the kernel's OOM
// killer then takes all of its processes or none.
func setOOMGroup(dir string) error {
	err := os.WriteFile(filepath.Join(dir, "memory.oom.group"), []byte("1"), 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// setOOMScores sets the oom_score_adj of each process in the cgroup dir
// whose differs. A process gone meanwhile is passed by.
func setOOMScores(dir string, score int32) error {
	b, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	want := strconv.Itoa(int(score))
	for pid := range strings.FieldsSeq(string(b)) {
		path := filepath.Join("/proc", pid, "oom_score_adj")
		have, err := os.ReadFile(path)
		if err != nil || strings.TrimSpace(string(have)) == want {
			continue
		}
		if err := os.WriteFile(path, []byte(want), 0); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.ESRCH) {
			return fmt.Errorf("the OOM score of process %s: %w", pid, err)
		}
	}
	return nil
}

// balloonStats is Cloud Hypervisor's vm.balloon-stats: what the balloon
// holds, and the statistics its guest last reported, each one it reported
// and Cloud Hypervisor knows.
type balloonStats struct {
	BalloonActual uint64 `json:"balloon_actual"`
	// LastUpdate is when the guest last reported, in milliseconds since
	// the epoch; zero when it never has.
	LastUpdate int64 `json:"last_update"`
	Stats      struct {
		TotalMemory     *uint64 `json:"total_memory"`
		AvailableMemory *uint64 `json:"available_memory"`
		AllocStalls     *uint64 `json:"alloc_stalls"`
		DirectReclaims  *uint64 `json:"direct_reclaims"`
		AsyncReclaims   *uint64 `json:"async_reclaims"`
		OOMKills        *uint64 `json:"oom_kills"`
	} `json:"stats"`
}

func (h *Hypervisor) Balloon(ctx context.Context, uid string) (driver.Balloon, error) {
	if err := checkUID(uid); err != nil {
		return driver.Balloon{}, err
	}
	var st balloonStats
	if err := h.chAPI(ctx, uid, http.MethodGet, "vm.balloon-stats", nil, &st); err != nil {
		// A guest whose balloon driver takes no statistics, or a Cloud
		// Hypervisor from before them, says only what its balloon holds:
		// what the VM's memory lacks of its size.
		ln, lerr := readLaunch(h.dir(uid))
		if lerr != nil {
			return driver.Balloon{}, err
		}
		var info struct {
			MemoryActualSize uint64 `json:"memory_actual_size"`
		}
		if ierr := h.chAPI(ctx, uid, http.MethodGet, "vm.info", nil, &info); ierr != nil {
			return driver.Balloon{}, errors.Join(err, ierr)
		}
		memory := uint64(ln.Spec.MemoryMiB) << 20
		return driver.Balloon{Actual: memory - min(info.MemoryActualSize, memory)}, nil
	}
	b := driver.Balloon{Actual: st.BalloonActual}
	if total, avail := st.Stats.TotalMemory, st.Stats.AvailableMemory; st.LastUpdate > 0 && total != nil && avail != nil {
		b.Total, b.Available, b.Updated = *total, *avail, time.UnixMilli(st.LastUpdate)
		for _, n := range []*uint64{st.Stats.AllocStalls, st.Stats.DirectReclaims, st.Stats.AsyncReclaims} {
			if n != nil {
				b.Reclaims, b.HasReclaims = b.Reclaims+*n, true
			}
		}
		if n := st.Stats.OOMKills; n != nil {
			b.OOMKills, b.HasOOMKills = *n, true
		}
	}
	return b, nil
}

func (h *Hypervisor) SetBalloon(ctx context.Context, uid string, size uint64) error {
	if err := checkUID(uid); err != nil {
		return err
	}
	ln, err := readLaunch(h.dir(uid))
	if err != nil {
		return fmt.Errorf("machine %s has no running VM: %w", uid, err)
	}
	if size > uint64(ln.Spec.MemoryMiB)<<20 {
		return fmt.Errorf("machine %s: a balloon of %d bytes is past its memory, %d MiB", uid, size, ln.Spec.MemoryMiB)
	}
	return h.chAPI(ctx, uid, http.MethodPut, "vm.resize", map[string]uint64{"desired_balloon": size}, nil)
}

// chAPI calls the API of a machine's VM's Cloud Hypervisor, sending in as
// JSON if not nil, and reading what it answers into out if not nil. The
// socket is dialled as pressPowerButton dials it, never through a link.
func (h *Hypervisor) chAPI(ctx context.Context, uid, method, path string, in, out any) error {
	socket := filepath.Join(h.dir(uid), apiFile)
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	c := http.Client{Transport: &http.Transport{
		// Cloud Hypervisor's API keeps few connections open.
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return chguest.DialSocket(ctx, socket)
		},
	}}
	defer c.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost/api/v1/"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("machine %s: %s: %w", uid, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return fmt.Errorf("machine %s: %s: %s: %s", uid, path, resp.Status, bytes.TrimSpace(b))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(out); err != nil {
		return fmt.Errorf("machine %s: %s: %w", uid, path, err)
	}
	return nil
}

// OOMGroupKills reads machinesSlice's memory.events, which counts what
// happens in every cgroup inside it.
func (h *Hypervisor) OOMGroupKills(context.Context) (uint64, error) {
	b, err := os.ReadFile(filepath.Join(h.cfg.cgroupRoot, machinesSlice, "memory.events"))
	if err != nil {
		return 0, err
	}
	return keyedValue(b, "oom_group_kill")
}

// keyedValue is the value of key in a cgroup's flat keyed file.
func keyedValue(b []byte, key string) (uint64, error) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), " ")
		if ok && k == key {
			return strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		}
	}
	return 0, fmt.Errorf("no %s", key)
}

// readUint reads a file holding one unsigned number.
func readUint(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}
