package e2e

import (
	"context"
	"fmt"
	"testing"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

const gib = int64(1) << 30

// diskMachine is a machine with disks, as a controller writes one.
func diskMachine(name string, disks ...string) *computev1alpha1.Machine {
	m := &computev1alpha1.Machine{
		Metadata: &metav1.ObjectMeta{Project: project, Name: name, Finalizers: []string{apiserver.NodeCleanupFinalizer}},
		Spec: &computev1alpha1.MachineSpec{
			Resources:     &computev1alpha1.Resources{Cpu: "1", Memory: "512Mi", Disk: "10Gi"},
			RunState:      computev1alpha1.MachineRunState_MACHINE_RUN_STATE_RUNNING,
			RestartPolicy: computev1alpha1.RestartPolicy_RESTART_POLICY_ALWAYS,
			Boot:          &computev1alpha1.MachineSpec_Container{Container: &computev1alpha1.ContainerBoot{Image: "nginx"}},
			Interfaces:    []*computev1alpha1.MachineInterface{{Name: "eth0", Network: "default", Ipv4: "10.0.0.200/24"}},
		},
	}
	for _, d := range disks {
		m.Spec.Disks = append(m.Spec.Disks, &computev1alpha1.MachineDisk{Disk: d})
	}
	return m
}

// putMachine applies m, keeping the node its machine was placed on.
func (w *world) putMachine(m *computev1alpha1.Machine) {
	w.t.Helper()
	if cur, ok := get[*computev1alpha1.Machine](w, registry.Machine, m.GetMetadata().GetName()); ok {
		m.Spec.Node = cur.GetSpec().GetNode()
	}
	if _, err := w.system.Apply(context.Background(), registry.Machine, m); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) applyDisk(name, size string) {
	w.t.Helper()
	if _, err := w.root.Apply(context.Background(), registry.Disk, &storagev1alpha1.Disk{
		Metadata: &metav1.ObjectMeta{Project: project, Name: name},
		Spec:     &storagev1alpha1.DiskSpec{Size: size},
	}); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) waitDisk(name, what string, ok func(*storagev1alpha1.Disk) bool) *storagev1alpha1.Disk {
	w.t.Helper()
	var d *storagev1alpha1.Disk
	testutil.Eventually(w.t, "disk "+name+" "+what, func() (bool, string) {
		var found bool
		d, found = get[*storagev1alpha1.Disk](w, registry.Disk, name)
		return found && ok(d), fmt.Sprintf("%v", d)
	})
	return d
}

// vmOf returns the VM of the machine on the host.
func (w *world) vmOf(machine string) (*computev1alpha1.Machine, driver.VMSpec, bool) {
	m, ok := get[*computev1alpha1.Machine](w, registry.Machine, machine)
	if !ok {
		return nil, driver.VMSpec{}, false
	}
	vm, ok := w.host.Snapshot().VMs[m.GetMetadata().GetUid()]
	return m, vm.Spec, ok && vm.State == driver.VMRunning
}

// TestADiskOnTheHost follows a disk from its first machine, through growing
// and detaching, to its deletion, and an orphan volume to the sweep.
func TestADiskOnTheHost(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.applyDisk("data", "2Gi")
	w.putMachine(diskMachine("vm-a", "data"))

	d := w.waitDisk("data", "in use", func(d *storagev1alpha1.Disk) bool {
		return d.GetStatus().GetPhase() == storagev1alpha1.DiskPhase_DISK_PHASE_IN_USE &&
			conditions.IsTrue(d.GetStatus().GetConditions(), computeapi.DiskReady)
	})
	if d.GetStatus().GetNode() == "" || d.GetStatus().GetSize() != "2Gi" || d.GetStatus().GetMachine() != "vm-a" {
		t.Errorf("disk status %v", d.GetStatus())
	}
	uid := d.GetMetadata().GetUid()
	testutil.Eventually(w.t, "the VM running with the disk", func() (bool, string) {
		_, spec, running := w.vmOf("vm-a")
		return running && len(spec.Disks) == 1 && spec.Disks[0].DiskUID == uid && spec.Disks[0].Size >= 2*gib,
			fmt.Sprintf("%+v", spec.Disks)
	})
	if v, ok := w.host.Snapshot().Volumes[uid]; !ok || v.Size < 2*gib {
		t.Errorf("volume %+v %t", v, ok)
	}

	// Grown, the volume grows, and the VM boots again to see it.
	w.applyDisk("data", "3Gi")
	testutil.Eventually(w.t, "the VM running with the grown disk", func() (bool, string) {
		_, spec, running := w.vmOf("vm-a")
		return running && len(spec.Disks) == 1 && spec.Disks[0].Size >= 3*gib, fmt.Sprintf("%+v", spec.Disks)
	})

	// Detached, the VM runs without it, and the disk stays on the host.
	w.putMachine(diskMachine("vm-a"))
	testutil.Eventually(w.t, "the VM running without the disk", func() (bool, string) {
		_, spec, running := w.vmOf("vm-a")
		return running && len(spec.Disks) == 0, fmt.Sprintf("%+v", spec.Disks)
	})
	w.waitDisk("data", "available", func(d *storagev1alpha1.Disk) bool {
		return d.GetStatus().GetPhase() == storagev1alpha1.DiskPhase_DISK_PHASE_AVAILABLE
	})
	if _, ok := w.host.Snapshot().Volumes[uid]; !ok {
		t.Error("a detached disk's volume is gone")
	}

	// Deleted, its volume goes, then the disk.
	if _, err := w.root.Delete(context.Background(), registry.Disk, client.Key{Project: project, Name: "data"}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(w.t, "the disk and its volume gone", func() (bool, string) {
		_, found := get[*storagev1alpha1.Disk](w, registry.Disk, "data")
		_, vol := w.host.Snapshot().Volumes[uid]
		return !found && !vol, fmt.Sprintf("disk left: %t, volume left: %t", found, vol)
	})

	// A volume no disk accounts for is swept.
	w.host.LeaveVolume("orphan")
	testutil.Eventually(w.t, "the orphan volume swept", func() (bool, string) {
		_, left := w.host.Snapshot().Volumes["orphan"]
		return !left, ""
	})

	if _, err := w.system.Delete(context.Background(), registry.Machine, client.Key{Project: project, Name: "vm-a"}); err != nil {
		t.Fatal(err)
	}
	w.waitHostEmpty()
}

// TestAContainerWithAVolume: a container's volume reaches its VM as a disk
// mounted at its target, the disk says it is the container's volume, and
// moving it restarts the VM to mount it there.
func TestAContainerWithAVolume(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.applyDisk("pgdata", "2Gi")
	c := container("db", "nginx")
	c.Spec.Volumes = []*computev1alpha1.ContainerVolume{{Type: "volume", Source: "pgdata", Target: "/var/lib/postgresql/data"}}
	w.apply(c)
	w.waitReason("db", "Running")

	d := w.waitDisk("pgdata", "a volume of db", func(d *storagev1alpha1.Disk) bool {
		return d.GetStatus().GetPhase() == storagev1alpha1.DiskPhase_DISK_PHASE_IN_USE &&
			d.GetStatus().GetAttachment().GetAs() == storagev1alpha1.DiskAttachmentRole_DISK_ATTACHMENT_ROLE_VOLUME
	})
	if a := d.GetStatus().GetAttachment(); a.GetKind() != "Container" || a.GetName() != "db" || a.GetDevice() != "vdc" ||
		a.GetPath() != "/var/lib/postgresql/data" {
		t.Errorf("attachment %v", a)
	}
	uid := d.GetMetadata().GetUid()
	mounted := func(target string) func() (bool, string) {
		return func() (bool, string) {
			_, spec, running := w.vmOf("ct-db")
			return running && len(spec.Disks) == 1 && spec.Disks[0].DiskUID == uid && len(spec.Mounts) == 1 &&
				spec.Mounts[0] == driver.Mount{Disk: 0, Name: "pgdata", Target: target}, fmt.Sprintf("%+v %+v", spec.Disks, spec.Mounts)
		}
	}
	testutil.Eventually(w.t, "the VM running with the volume mounted", mounted("/var/lib/postgresql/data"))

	c.Spec.Volumes[0].Target = "/data"
	w.apply(c)
	testutil.Eventually(w.t, "the VM running with the volume at /data", mounted("/data"))
	w.waitDisk("pgdata", "at /data", func(d *storagev1alpha1.Disk) bool { return d.GetStatus().GetAttachment().GetPath() == "/data" })

	w.delete("db")
	w.waitDisk("pgdata", "attached to nothing", func(d *storagev1alpha1.Disk) bool {
		return d.GetStatus().GetAttachment() == nil && d.GetStatus().GetPhase() == storagev1alpha1.DiskPhase_DISK_PHASE_AVAILABLE
	})
}

// TestADiskWaitsForItsNode: a machine whose disk the host cannot make says
// so, and runs once it can.
func TestADiskWaitsForItsNode(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.applyDisk("data", "2Gi")
	w.host.SetFull(true)
	w.putMachine(diskMachine("vm-a", "data"))
	w.waitDisk("data", "told there is no room", func(d *storagev1alpha1.Disk) bool {
		return conditions.Find(d.GetStatus().GetConditions(), computeapi.DiskReady).GetReason() == "NoSpace"
	})
	w.host.SetFull(false)
	testutil.Eventually(w.t, "the VM running with the disk", func() (bool, string) {
		_, spec, running := w.vmOf("vm-a")
		return running && len(spec.Disks) == 1, fmt.Sprintf("%+v", spec.Disks)
	})
	w.waitDisk("data", "in use", func(d *storagev1alpha1.Disk) bool {
		return d.GetStatus().GetPhase() == storagev1alpha1.DiskPhase_DISK_PHASE_IN_USE
	})
}
