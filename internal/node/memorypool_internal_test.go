package node

import (
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
			Resources:  &computev1alpha1.Resources{Cpu: "1", Memory: "2Gi", MemoryGuaranteed: "1Gi"},
			MemoryPool: &computev1alpha1.MemoryPool{Name: "acme", Size: "8Gi"},
		},
	}
	s := wantedShare(m)
	if s == nil || s.pool == nil || *s.pool != (driver.MemoryPool{Name: "acme", SizeMiB: 8192}) || s.memoryMiB != 2048 || s.guaranteedMiB != 1024 || s.specGuaranteed() != 1024 {
		t.Errorf("pooled with a guarantee: %+v", s)
	}
	m.Spec.Resources.MemoryGuaranteed = ""
	if s := wantedShare(m); s == nil || s.guaranteedMiB != 2048 || s.specGuaranteed() != 0 {
		t.Errorf("pooled with no guarantee: %+v, want all of its memory", s)
	}
	// Outside a pool a guarantee means nothing.
	m.Spec.Resources.MemoryGuaranteed, m.Spec.MemoryPool = "1Gi", nil
	if s := wantedShare(m); s == nil || s.pool != nil || s.guaranteedMiB != 2048 || s.specGuaranteed() != 0 {
		t.Errorf("in no pool: %+v, want all of its memory", s)
	}
	m.Spec.RunState = computev1alpha1.MachineRunState_MACHINE_RUN_STATE_STOPPED
	if s := wantedShare(m); s != nil {
		t.Errorf("stopped: %+v, want none", s)
	}
}
