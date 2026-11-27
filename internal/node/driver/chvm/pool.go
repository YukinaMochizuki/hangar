package chvm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	sd "github.com/coreos/go-systemd/v22/dbus"
	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chguest"
)

// poolSlicePrefix starts the name of each pool's slice, inside
// machinesSlice: a tenant's memory pool and CPU pool share one, of the
// tenant's name; a dash past it would make systemd nest the slice a level
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
	if name := spec.Pool(); name != "" {
		return poolSlice(name)
	}
	return machinesSlice
}

// poolSliceDescription is a pool's slice's description.
func poolSliceDescription(name string) string {
	return fmt.Sprintf("Hangar's pool %s", name)
}

// checkCPUPool refuses a CPU pool whose name cannot name a slice, or of no
// CPU.
func checkCPUPool(p driver.CPUPool) error {
	if !poolNamePattern.MatchString(p.Name) {
		return fmt.Errorf("CPU pool %q cannot name a slice", p.Name)
	}
	if p.MilliCPU <= 0 {
		return fmt.Errorf("CPU pool %s has %d millicores", p.Name, p.MilliCPU)
	}
	return nil
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

// sliceShare is what a VM's unit adds to its pool's slice's MemoryMax past
// the guests' memory a memory pool is held to: its overhead, for a VM in
// the memory pool, whose guest's memory the pool holds; all its unit may
// hold, for a VM in the slice for a CPU pool alone, whose memory is its own.
func sliceShare(spec driver.VMSpec) uint64 {
	if spec.MemoryPool == nil {
		return unitMemoryMax(spec)
	}
	return unitOverhead(spec)
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
	if name := spec.Pool(); name != "" {
		dir = filepath.Join(dir, poolSlice(name))
	}
	return filepath.Join(dir, unitName(uid))
}

// poolMember is a VM whose unit runs, or is about to, in a pool's slice.
type poolMember struct {
	uid     string
	spec    driver.VMSpec
	started time.Time
}

// allPoolMembers lists the VMs whose units run, or wait to restart, in a
// pool's slice, by their spec files, by pool: a memory pool's VMs and a
// CPU pool's alike.
func (h *Hypervisor) allPoolMembers(ctx context.Context) (map[string][]poolMember, error) {
	units, err := h.sd.list(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing units: %w", err)
	}
	out := map[string][]poolMember{}
	for _, u := range units {
		uid, ok := uidOfUnit(u.Name)
		if !ok || !running(u) {
			continue
		}
		ln, err := readLaunch(h.dir(uid))
		if err != nil || ln.Spec.Pool() == "" {
			continue
		}
		name := ln.Spec.Pool()
		out[name] = append(out[name], poolMember{uid: uid, spec: ln.Spec, started: u.Started})
	}
	return out, nil
}

// poolMembers lists the VMs whose units run, or wait to restart, in the
// pool's slice.
func (h *Hypervisor) poolMembers(ctx context.Context, name string) ([]poolMember, error) {
	all, err := h.allPoolMembers(ctx)
	return all[name], err
}

func (h *Hypervisor) PoolVMs(ctx context.Context) ([]driver.PoolVM, error) {
	all, err := h.allPoolMembers(ctx)
	if err != nil {
		return nil, err
	}
	var out []driver.PoolVM
	for _, members := range all {
		for _, m := range members {
			if m.spec.MemoryPool == nil {
				// In the slice for its CPU pool alone: no memory pool's.
				continue
			}
			out = append(out, driver.PoolVM{
				MachineUID: m.uid, Pool: *m.spec.MemoryPool, MemoryMiB: m.spec.MemoryMiB,
				ReservedMiB: m.spec.Reserved(), Started: m.started,
			})
		}
	}
	slices.SortFunc(out, func(a, b driver.PoolVM) int { return strings.Compare(a.MachineUID, b.MachineUID) })
	return out, nil
}

// poolProps are a pool's slice's MemoryMax and CPUWeight, for its members and starting (docs/decisions/node-agent.md "Memory pools").
func (h *Hypervisor) poolProps(guestMiB int64, members []poolMember, starting *driver.VMSpec) (memory, weight uint64) {
	memory = uint64(max(guestMiB, 0)) << 20
	for _, m := range members {
		memory += sliceShare(m.spec)
		w, err := readUint(filepath.Join(h.unitCgroup(m.spec, m.uid), "cpu.weight"))
		if err != nil {
			w = cpuWeight(m.spec)
		}
		weight += w
	}
	if starting != nil {
		memory += sliceShare(*starting)
		weight += cpuWeight(*starting)
	}
	if guestMiB <= 0 {
		memory = math.MaxUint64
	}
	return memory, clampWeight(weight)
}

// setSlice gives a pool's slice memory and weight, where they differ from
// what its cgroup has; a slice not made yet is made.
func (h *Hypervisor) setSlice(ctx context.Context, name string, memory, weight uint64) error {
	dir := filepath.Join(h.cfg.cgroupRoot, machinesSlice, poolSlice(name))
	if have, ok := cgroupMax(filepath.Join(dir, "memory.max")); ok && have == memory&^4095 {
		if w, err := readUint(filepath.Join(dir, "cpu.weight")); err == nil && w == weight {
			return nil
		}
	}
	props := []sd.Property{property("MemoryMax", memory), noSwap(), property("CPUWeight", weight)}
	if err := h.sd.slice(ctx, poolSlice(name), poolSliceDescription(name), props); err != nil {
		return fmt.Errorf("the pool's slice %s: %w", poolSlice(name), err)
	}
	h.watchEvents(filepath.Join(dir, "memory.events"))
	return nil
}

// cgroupMax reads a cgroup's limit file, "max" as math.MaxUint64&^4095.
func cgroupMax(path string) (uint64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	if s := strings.TrimSpace(string(b)); s == "max" {
		return math.MaxUint64 &^ 4095, true
	}
	n, err := readUint(path)
	return n, err == nil
}

// startPool makes or raises the slice of the pool of a VM about to start,
// and gives it the VM's CPU pool's limit (docs/decisions/node-agent.md
// "Memory pools", docs/decisions/cpu-pool.md).
func (h *Hypervisor) startPool(ctx context.Context, spec driver.VMSpec) error {
	name := spec.Pool()
	members, err := h.poolMembers(ctx, name)
	if err != nil {
		return err
	}
	// A slice for CPU alone holds no memory to a limit until a VM of a
	// memory pool starts in it, and the squeeze gives it one.
	var guestMiB int64
	if spec.MemoryPool != nil {
		guestMiB = spec.MemoryPool.SizeMiB
	}
	memory, weight := h.poolProps(guestMiB, members, &spec)
	inMemoryPool := slices.ContainsFunc(members, func(m poolMember) bool { return m.spec.MemoryPool != nil })
	if have, ok := cgroupMax(filepath.Join(h.cfg.cgroupRoot, machinesSlice, poolSlice(name), "memory.max")); ok {
		switch {
		case have == math.MaxUint64&^4095 && (inMemoryPool || spec.MemoryPool == nil):
			// A memory pool lifted, which the squeeze sets again, or a slice
			// with no memory pool's VM, kept so.
			memory = math.MaxUint64
		case have == math.MaxUint64&^4095:
			// The first of a memory pool's VMs in a slice made for a CPU
			// pool alone: held from the start, as poolProps counts it.
		case spec.MemoryPool == nil:
			// Into a memory pool's slice, for its CPU pool alone: all its
			// unit may hold comes on top, and the pool's limit stays.
			memory = have + sliceShare(spec)
		default:
			memory = max(memory, have+sliceShare(spec))
		}
	}
	if err := h.setSlice(ctx, name, memory, weight); err != nil {
		return err
	}
	if spec.CPUPool == nil {
		// A VM of the memory pool alone leaves the slice's CPU limit as it
		// is: the agent sets it from the specs of the machines in it.
		return nil
	}
	return h.setSliceCPU(ctx, name, spec.CPUPool.MilliCPU)
}

func (h *Hypervisor) SetMemoryPool(ctx context.Context, name string, guestMiB int64) error {
	if !poolNamePattern.MatchString(name) {
		return fmt.Errorf("memory pool %q cannot name a slice", name)
	}
	h.pools.Lock()
	defer h.pools.Unlock()
	members, err := h.poolMembers(ctx, name)
	if err != nil || len(members) == 0 {
		return err
	}
	memory, weight := h.poolProps(guestMiB, members, nil)
	return h.setSlice(ctx, name, memory, weight)
}

// cpuQuotaPeriod is the period of a CPU pool's limit, short for the pause a
// guest sees (docs/decisions/node-agent.md "CPU pools").
const cpuQuotaPeriod = 20 * time.Millisecond

// cpuMax reads a cgroup's cpu.max: what its processes may run each period,
// zero for no limit, and the period.
func cpuMax(path string) (quota, period time.Duration, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	q, p, found := strings.Cut(strings.TrimSpace(string(b)), " ")
	if !found {
		return 0, 0, false
	}
	pn, err := strconv.ParseUint(p, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	period = time.Duration(pn) * time.Microsecond
	if q == "max" {
		return 0, period, true
	}
	qn, err := strconv.ParseUint(q, 10, 64)
	return time.Duration(qn) * time.Microsecond, period, err == nil
}

// cpuQuota is what a pool of milliCPU runs each cpuQuotaPeriod; zero, for a
// milliCPU of zero, is no limit.
func cpuQuota(milliCPU int64) time.Duration {
	if milliCPU <= 0 {
		return 0
	}
	return time.Duration(milliCPU) * cpuQuotaPeriod / 1000
}

// setSliceCPU holds a pool's slice to milliCPU of the CPUs, or lifts its
// limit for zero, where its cgroup differs.
func (h *Hypervisor) setSliceCPU(ctx context.Context, name string, milliCPU int64) error {
	want := cpuQuota(milliCPU)
	quota, period, ok := cpuMax(filepath.Join(h.cfg.cgroupRoot, machinesSlice, poolSlice(name), "cpu.max"))
	if ok && quota == want && (want == 0 || period == cpuQuotaPeriod) {
		return nil
	}
	if err := h.sd.slice(ctx, poolSlice(name), poolSliceDescription(name), cpuLimitProps(milliCPU)); err != nil {
		return fmt.Errorf("the CPU limit of the pool's slice %s: %w", poolSlice(name), err)
	}
	return nil
}

// cpuLimitProps are the properties that hold a unit or a slice to milliCPU
// of the CPUs each cpuQuotaPeriod, or lift its limit for zero.
func cpuLimitProps(milliCPU int64) []sd.Property {
	// systemd takes the limit as CPU time per second, infinity for none.
	perSec := uint64(math.MaxUint64)
	if milliCPU > 0 {
		perSec = uint64(milliCPU) * 1000
	}
	return []sd.Property{property("CPUQuotaPerSecUSec", perSec), property("CPUQuotaPeriodUSec", usec(cpuQuotaPeriod))}
}

// SetCPULimit sets the VM's unit's CPU limit where its cgroup's differs,
// and its memory.oom.group again, as SetMemoryShare does. systemd keeps
// the limit in a drop-in under /run: the unit's own restarts keep it, a
// new unit or a reboot does not, so Start applies VMSpec.CPULimit
// (docs/decisions/cpu-credits.md).
func (h *Hypervisor) SetCPULimit(ctx context.Context, uid string, milliCPU int64) error {
	if err := checkUID(uid); err != nil {
		return err
	}
	ln, err := readLaunch(h.dir(uid))
	if err != nil {
		return fmt.Errorf("machine %s has no running VM: %w", uid, err)
	}
	cg := h.unitCgroup(ln.Spec, uid)
	want := cpuQuota(milliCPU)
	quota, period, ok := cpuMax(filepath.Join(cg, "cpu.max"))
	if !ok {
		return fmt.Errorf("machine %s has no running VM: its unit's cpu.max cannot be read", uid)
	}
	if quota == want && (want == 0 || period == cpuQuotaPeriod) {
		return nil
	}
	if err := h.sd.setProps(ctx, unitName(uid), cpuLimitProps(milliCPU)); err != nil {
		return fmt.Errorf("the CPU limit of %s: %w", unitName(uid), err)
	}
	return setOOMGroup(cg)
}

func (h *Hypervisor) SetCPUPools(ctx context.Context, want map[string]int64, read map[string]bool) error {
	h.pools.Lock()
	defer h.pools.Unlock()
	all, err := h.allPoolMembers(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(all)) {
		if err := h.setSliceCPU(ctx, name, cpuPoolLimit(name, all[name], want, read)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// cpuPoolLimit is the millicores a pool's slice is held to, zero for no
// limit: what want gives it, or more where a member whose spec is not read
// was launched in the pool with more, so an agent starting holds the pool
// as it was until it has read its machines.
func cpuPoolLimit(name string, members []poolMember, want map[string]int64, read map[string]bool) int64 {
	limit := max(want[name], 0)
	for _, m := range members {
		if p := m.spec.CPUPool; p != nil && !read[m.uid] && p.Name == name {
			limit = max(limit, p.MilliCPU)
		}
	}
	return limit
}

func (h *Hypervisor) CPUPoolStats(ctx context.Context) (map[string]driver.CPUPoolStat, error) {
	all, err := h.allPoolMembers(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]driver.CPUPoolStat{}
	for name := range all {
		b, err := os.ReadFile(filepath.Join(h.cfg.cgroupRoot, machinesSlice, poolSlice(name), "cpu.stat"))
		if err != nil {
			continue
		}
		var st driver.CPUPoolStat
		for key, v := range map[string]*uint64{"nr_periods": &st.Periods, "nr_throttled": &st.Throttled} {
			*v, _ = keyedValue(b, key)
		}
		throttled, _ := keyedValue(b, "throttled_usec")
		usage, _ := keyedValue(b, "usage_usec")
		st.ThrottledTime, st.Usage = time.Duration(throttled)*time.Microsecond, time.Duration(usage)*time.Microsecond
		out[name] = st
	}
	return out, nil
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
// reservation, where they differ. Its unit's memory.oom.group is set again,
// as systemd clears it whenever it sets a property of a unit started
// without OOMPolicy=kill.
func (h *Hypervisor) SetMemoryShare(ctx context.Context, uid string, pool *driver.MemoryPool, reservedMiB int64) error {
	if err := checkUID(uid); err != nil {
		return err
	}
	ln, err := readLaunch(h.dir(uid))
	if err != nil {
		return fmt.Errorf("machine %s has no running VM: %w", uid, err)
	}
	spec := ln.Spec
	spec.MemoryPool, spec.ReservedMiB = pool, reservedMiB
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
