package node

import (
	"maps"
	"testing"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

func TestAMachinesShareIsReadFromItsSpec(t *testing.T) {
	t.Parallel()
	m := &computev1alpha1.Machine{
		Metadata: &metav1.ObjectMeta{Project: "p", Name: "m"},
		Spec: &computev1alpha1.MachineSpec{
			Resources:  &computev1alpha1.Resources{Cpu: "1", Memory: "2Gi", MemoryReserved: "1Gi"},
			MemoryPool: &computev1alpha1.MemoryPool{Name: "acme", Size: "8Gi"},
		},
	}
	s := wantedShare(m)
	if s == nil || s.pool == nil || *s.pool != (driver.MemoryPool{Name: "acme", SizeMiB: 8192}) || s.memoryMiB != 2048 || s.reservedMiB != 1024 || s.specReserved() != 1024 {
		t.Errorf("pooled with a reservation: %+v", s)
	}
	m.Spec.Resources.MemoryReserved = ""
	if s := wantedShare(m); s == nil || s.reservedMiB != 2048 || s.specReserved() != 0 {
		t.Errorf("pooled with no reservation: %+v, want all of its memory", s)
	}
	// Outside a pool a reservation means nothing.
	m.Spec.Resources.MemoryReserved, m.Spec.MemoryPool = "1Gi", nil
	if s := wantedShare(m); s == nil || s.pool != nil || s.reservedMiB != 2048 || s.specReserved() != 0 {
		t.Errorf("in no pool: %+v, want all of its memory", s)
	}
	m.Spec.RunState = computev1alpha1.MachineRunState_MACHINE_RUN_STATE_STOPPED
	if s := wantedShare(m); s != nil {
		t.Errorf("stopped: %+v, want none", s)
	}
}

// TestAMachinesCPUPoolIsReadFromItsSpec: a machine's CPU pool comes with
// its share and into its VM's spec, apart from its memory pool; a cpu that
// is no quantity refuses its VM.
func TestAMachinesCPUPoolIsReadFromItsSpec(t *testing.T) {
	t.Parallel()
	m := &computev1alpha1.Machine{
		Metadata: &metav1.ObjectMeta{Project: "p", Name: "m"},
		Spec: &computev1alpha1.MachineSpec{
			Resources: &computev1alpha1.Resources{Cpu: "2", Memory: "2Gi"},
			CpuPool:   &computev1alpha1.CPUPool{Name: "acme", Cpu: "2500m"},
		},
	}
	s := wantedShare(m)
	if s == nil || s.pool != nil || s.cpuPool == nil || *s.cpuPool != (driver.CPUPool{Name: "acme", MilliCPU: 2500}) {
		t.Errorf("in a CPU pool alone: %+v", s)
	}
	spec, err := withMemoryShare(driver.VMSpec{MemoryMiB: 2048}, m)
	if err != nil || spec.CPUPool == nil || spec.CPUPool.MilliCPU != 2500 || spec.MemoryPool != nil || spec.Pool() != "acme" {
		t.Errorf("its VM's spec: %+v, %v", spec, err)
	}
	m.Spec.CpuPool.Cpu = "lots"
	if _, err := withMemoryShare(driver.VMSpec{MemoryMiB: 2048}, m); err == nil {
		t.Error("a CPU pool of no quantity gave a VM spec")
	}
}

// TestCPUPoolsAreHeldToTheirSpecs: each pool's CPUs are what its machines'
// specs give it, the most while they disagree, as a plan changes; a pool
// none gives any is held to none.
func TestCPUPoolsAreHeldToTheirSpecs(t *testing.T) {
	t.Parallel()
	a, h := newPoolAgent(t)
	with := func(s memoryShare, milli int64) memoryShare {
		s.cpuPool = &driver.CPUPool{Name: "acme", MilliCPU: milli}
		return s
	}
	a.memoryShares = map[string]memoryShare{"a": with(share("a", "acme", 4096, 4096, 1024), 2000), "s": with(share("s", "acme", 4096, 4096, 1024), 4000)}
	a.squeeze(t.Context(), false)
	if got, want := h.cpus, map[string]int64{"acme": 4000}; !maps.Equal(got, want) {
		t.Errorf("CPU pools %v, want %v", got, want)
	}
	a.memoryShares = map[string]memoryShare{"a": share("a", "acme", 4096, 4096, 1024), "s": share("s", "acme", 4096, 4096, 1024)}
	a.sharesRead++
	a.squeeze(t.Context(), false)
	if len(h.cpus) != 0 || !h.read["a"] || !h.read["s"] {
		t.Errorf("with no spec giving a CPU pool, CPU pools %v of machines read %v, want none, of a and s", h.cpus, h.read)
	}

	// An agent just started has read no machine: the driver is told so,
	// and keeps each pool as its VMs were launched.
	a, h = newPoolAgent(t)
	a.squeeze(t.Context(), false)
	if len(h.cpus) != 0 || len(h.read) != 0 {
		t.Errorf("before any read, CPU pools %v of machines read %v, want none of none", h.cpus, h.read)
	}
}

// TestAnOlderReadNeverSetsTheCPUPools: limits set from a later read of the
// specs, as a reconcile's, are not overwritten by an earlier one, as the
// squeeze's taken before it.
func TestAnOlderReadNeverSetsTheCPUPools(t *testing.T) {
	t.Parallel()
	a, h := newPoolAgent(t)
	a.host.Lock()
	old := a.readCPUPools()
	s := share("a", "acme", 4096, 4096, 1024)
	s.cpuPool = &driver.CPUPool{Name: "acme", MilliCPU: 3000}
	a.trackMemory("a", &s)
	a.applyCPUPools(t.Context())
	a.host.Unlock()
	a.setCPUPools(t.Context(), old)
	if got := h.cpus["acme"]; got != 3000 {
		t.Errorf("the pool is held to %d millicores, want 3000, the later read's", got)
	}
}
