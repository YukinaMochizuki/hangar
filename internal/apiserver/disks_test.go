package apiserver

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

func disk(name, size string) *storagev1alpha1.Disk {
	return &storagev1alpha1.Disk{
		Metadata: &metav1.ObjectMeta{Project: "web", Name: name},
		Spec:     &storagev1alpha1.DiskSpec{Size: size},
	}
}

func getDisk(t *testing.T, s *Server, system context.Context, name string) *storagev1alpha1.Disk {
	t.Helper()
	obj, err := s.get(system, registry.Disk, resource.Key{Project: "web", Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if obj == nil {
		return nil
	}
	return obj.(*storagev1alpha1.Disk)
}

func waitDisk(t *testing.T, s *Server, system context.Context, name, what string, ok func(*storagev1alpha1.Disk) bool) *storagev1alpha1.Disk {
	t.Helper()
	return waitObject(t, s, system, registry.Disk, resource.Key{Project: "web", Name: name}, "disk "+name+" "+what, ok)
}

// bindDisk binds a disk to node at size, as the scheduler does.
func bindDisk(t *testing.T, s *Server, system context.Context, name, node, size string) {
	t.Helper()
	d := getDisk(t, s, system, name)
	status := proto.Clone(d.GetStatus()).(*storagev1alpha1.DiskStatus)
	if status == nil {
		status = &storagev1alpha1.DiskStatus{}
	}
	status.Node, status.Size = node, size
	if _, err := s.backend(registry.Disk).UpdateStatus(system, resource.Key{Project: "web", Name: name}, status, nil); err != nil {
		t.Fatal(err)
	}
}

// reportDiskCond sets a node's condition on a disk, as the node does.
func reportDiskCond(t *testing.T, s *Server, system context.Context, name string, c *metav1.Condition) {
	t.Helper()
	d := getDisk(t, s, system, name)
	status := proto.Clone(d.GetStatus()).(*storagev1alpha1.DiskStatus)
	status.Conditions = conditions.Set(status.GetConditions(), c, time.Now())
	if _, err := s.backend(registry.Disk).UpdateStatus(asNode(system, d.GetStatus().GetNode()), resource.Key{Project: "web", Name: name}, status, nil); err != nil {
		t.Fatal(err)
	}
}

func withDisks(m *computev1alpha1.Machine, disks ...string) *computev1alpha1.Machine {
	for _, d := range disks {
		m.Spec.Disks = append(m.Spec.Disks, &computev1alpha1.MachineDisk{Disk: d})
	}
	return m
}

// nodeSized reports a node ready with memMiB of memory and diskGiB of
// disk.
func nodeSized(t *testing.T, s *Server, system context.Context, node string, memMiB, diskGiB int64) {
	t.Helper()
	status := &computev1alpha1.NodeStatus{
		Capacity: &computev1alpha1.NodeResources{Cpu: "8", Memory: fmt.Sprintf("%dMi", memMiB), Disk: fmt.Sprintf("%dGi", diskGiB)},
		Conditions: []*metav1.Condition{{
			Type: NodeReady, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "AgentReporting",
		}},
	}
	if _, err := s.backend(registry.Node).UpdateStatus(asNode(system, node), resource.Key{Name: node}, status, nil); err != nil {
		t.Fatal(err)
	}
}

func TestDiskAdmission(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	b := s.backend(registry.Disk)
	for _, tt := range []struct {
		size string
		ok   bool
	}{
		{"10Gi", true},
		{"1Gi", true},
		{"1500M", true},
		{"64Ti", true},
		{"", false},
		{"1023Mi", false},
		{"65Ti", false},
		{"-1Gi", false},
		{"lots", false},
	} {
		_, err := b.Apply(user, disk("d", tt.size))
		if got := err == nil; got != tt.ok {
			t.Errorf("size %q: accepted %t, want %t (%v)", tt.size, got, tt.ok, err)
		}
	}

	// A disk not yet made can shrink.
	if _, err := b.Apply(user, disk("data", "20Gi")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Apply(user, disk("data", "10Gi")); err != nil {
		t.Errorf("shrinking a disk no node has made: %v", err)
	}
	// Made at 10Gi, it grows but never shrinks; a growth that never took
	// effect can be withdrawn.
	bindDisk(t, s, system, "data", "n1", "10Gi")
	_, err := b.Apply(user, disk("data", "9Gi"))
	wantCodeOf(t, err, connect.CodeInvalidArgument, "shrinking a made disk")
	for _, size := range []string{"20Gi", "10Gi", "10240Mi", "15Gi"} {
		if _, err := b.Apply(user, disk("data", size)); err != nil {
			t.Errorf("resizing a made disk to %s: %v", size, err)
		}
	}
}

func TestOnlyTheDisksNodeWritesItsStatus(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	root := asRoot(t, s, system)
	b := s.backend(registry.Disk)
	key := resource.Key{Project: "web", Name: "data"}
	status := &storagev1alpha1.DiskStatus{Node: "n1", Size: "10Gi"}

	// The project's owner makes and deletes disks; only the system and the
	// disk's node write their status, whatever the role.
	if _, err := b.Apply(user, disk("data", "10Gi")); err != nil {
		t.Fatal(err)
	}
	for who, ctx := range map[string]context.Context{"owner": user, "root": root, "node": asNode(system, "n1")} {
		_, err := b.UpdateStatus(ctx, key, status, nil)
		wantCodeOf(t, err, connect.CodePermissionDenied, who+" writing an unbound disk's status")
	}
	bindDisk(t, s, system, "data", "n1", "10Gi")
	if _, err := b.UpdateStatus(asNode(system, "n1"), key, status, nil); err != nil {
		t.Errorf("the disk's node reporting: %v", err)
	}
	for who, ctx := range map[string]context.Context{"owner": user, "root": root, "another node": asNode(system, "n2")} {
		_, err := b.UpdateStatus(ctx, key, status, nil)
		wantCodeOf(t, err, connect.CodePermissionDenied, who+" writing a bound disk's status")
	}
	if _, err := b.Delete(user, key, nil); err != nil {
		t.Errorf("the owner deleting a disk: %v", err)
	}
}

func TestADiskIsAttachedToOneMachine(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	disks, machines := s.backend(registry.Disk), s.backend(registry.Machine)
	for _, name := range []string{"data", "logs"} {
		if _, err := disks.Apply(user, disk(name, "10Gi")); err != nil {
			t.Fatal(err)
		}
	}

	_, err := machines.Apply(system, withDisks(machine("ct-a", ""), "nope"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "attaching a disk that does not exist")
	_, err = machines.Apply(system, withDisks(machine("ct-a", ""), "data", "data"))
	wantCodeOf(t, err, connect.CodeInvalidArgument, "attaching a disk twice")
	_, err = machines.Apply(system, withDisks(machine("ct-a", ""), "Bad_Name"))
	wantCodeOf(t, err, connect.CodeInvalidArgument, "attaching a disk by an invalid name")

	if _, err := machines.Apply(system, withDisks(machine("ct-a", ""), "data")); err != nil {
		t.Fatal(err)
	}
	_, err = machines.Apply(system, withDisks(machine("ct-b", ""), "data"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "attaching a disk another machine has")
	// The machine that has it keeps it as it changes, and takes another.
	if _, err := machines.Apply(system, withDisks(machine("ct-a", "n1"), "data", "logs")); err != nil {
		t.Errorf("a machine keeping its disk: %v", err)
	}

	// An attached disk cannot be deleted; detached, it can, and then no
	// machine takes it.
	_, err = disks.Delete(user, resource.Key{Project: "web", Name: "data"}, nil)
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "deleting an attached disk")
	if _, err := machines.Apply(system, withDisks(machine("ct-a", "n1"), "logs")); err != nil {
		t.Fatal(err)
	}
	if _, err := disks.Delete(user, resource.Key{Project: "web", Name: "data"}, nil); err != nil {
		t.Fatalf("deleting a detached disk: %v", err)
	}
	if d := getDisk(t, s, system, "data"); d == nil || d.GetMetadata().GetDeleteTime() == nil {
		t.Fatalf("the deleted disk, which waits for its finalizers: %v", d)
	}
	_, err = machines.Apply(system, withDisks(machine("ct-b", ""), "data"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "attaching a disk being deleted")
}

// TestAttachingIsOneDecision: machines racing for one disk, only one gets it.
func TestAttachingIsOneDecision(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	if _, err := s.backend(registry.Disk).Apply(user, disk("data", "10Gi")); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.backend(registry.Machine).Apply(system, withDisks(machine(fmt.Sprintf("ct-%d", i), ""), "data"))
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		if err == nil {
			won++
		}
	}
	if users, err := s.machinesUsing(system, "web", "data"); err != nil || won != 1 || len(users) != 1 {
		t.Errorf("%d machines got the disk, %v list it (%v); errors %v", won, users, err, errs)
	}
}

func TestADiskFollowsItsMachines(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeSized(t, s, system, "n1", 4096, 100)
	if _, err := s.backend(registry.Disk).Apply(user, disk("data", "20Gi")); err != nil {
		t.Fatal(err)
	}
	waitDisk(t, s, system, "data", "pending its first machine", func(d *storagev1alpha1.Disk) bool {
		c := conditions.Find(d.GetStatus().GetConditions(), DiskAllocated)
		return d.GetStatus().GetPhase() == storagev1alpha1.DiskPhase_DISK_PHASE_PENDING && c.GetReason() == "WaitingForFirstMachine"
	})

	// Its first machine binds it to the machine's node, at its size.
	if _, err := s.backend(registry.Machine).Apply(system, withDisks(machine("ct-a", ""), "data")); err != nil {
		t.Fatal(err)
	}
	waitMachine(t, s, system, "ct-a", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	d := waitDisk(t, s, system, "data", "bound and attached", func(d *storagev1alpha1.Disk) bool {
		return d.GetStatus().GetNode() == "n1" && d.GetStatus().GetMachine() == "ct-a" &&
			conditions.IsTrue(d.GetStatus().GetConditions(), DiskAllocated)
	})
	if d.GetStatus().GetSize() != "20Gi" || d.GetStatus().GetPhase() != storagev1alpha1.DiskPhase_DISK_PHASE_PENDING {
		t.Errorf("before its node made it: %v", d.GetStatus())
	}
	reportDiskCond(t, s, system, "data", &metav1.Condition{Type: DiskReady, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Created"})
	waitDisk(t, s, system, "data", "in use", func(d *storagev1alpha1.Disk) bool {
		return d.GetStatus().GetPhase() == storagev1alpha1.DiskPhase_DISK_PHASE_IN_USE
	})

	// Detached, it is available, and still on its node; attached again,
	// it is in use until its machine goes.
	available := func(d *storagev1alpha1.Disk) bool {
		return d.GetStatus().GetPhase() == storagev1alpha1.DiskPhase_DISK_PHASE_AVAILABLE && d.GetStatus().GetMachine() == "" &&
			d.GetStatus().GetNode() == "n1"
	}
	if _, err := s.backend(registry.Machine).Apply(system, machine("ct-a", "n1")); err != nil {
		t.Fatal(err)
	}
	waitDisk(t, s, system, "data", "available once detached", available)
	if _, err := s.backend(registry.Machine).Apply(system, withDisks(machine("ct-a", "n1"), "data")); err != nil {
		t.Fatal(err)
	}
	waitDisk(t, s, system, "data", "in use again", func(d *storagev1alpha1.Disk) bool {
		return d.GetStatus().GetPhase() == storagev1alpha1.DiskPhase_DISK_PHASE_IN_USE
	})
	if _, err := s.backend(registry.Machine).Delete(system, resource.Key{Project: "web", Name: "ct-a"}, nil); err != nil {
		t.Fatal(err)
	}
	waitGone(t, s, system, registry.Machine, "ct-a")
	waitDisk(t, s, system, "data", "available once its machine is gone", available)

	// Deleted, it waits for its node to remove it.
	if _, err := s.backend(registry.Disk).Delete(user, resource.Key{Project: "web", Name: "data"}, nil); err != nil {
		t.Fatal(err)
	}
	waitDisk(t, s, system, "data", "no longer held in use", func(d *storagev1alpha1.Disk) bool {
		return len(d.GetMetadata().GetFinalizers()) == 1 && d.GetMetadata().GetFinalizers()[0] == NodeCleanupFinalizer
	})
	testutil.Unchanged(t, "disk data, before its node released it", 100*time.Millisecond,
		versionOf(s, system, registry.Disk, resource.Key{Project: "web", Name: "data"}))
	reportDiskCond(t, s, system, "data", &metav1.Condition{Type: DiskReleased, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Cleaned"})
	waitGone(t, s, system, registry.Disk, "data")

	// A disk no node ever made has nothing to release.
	if _, err := s.backend(registry.Disk).Apply(user, disk("spare", "1Gi")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.Disk).Delete(user, resource.Key{Project: "web", Name: "spare"}, nil); err != nil {
		t.Fatal(err)
	}
	waitGone(t, s, system, registry.Disk, "spare")
}

func TestAMachineGoesWhereItsDisksAre(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "big", "small")
	nodeSized(t, s, system, "big", 8192, 100)
	nodeSized(t, s, system, "small", 1024, 100)
	for _, name := range []string{"on-small", "on-big", "new"} {
		if _, err := s.backend(registry.Disk).Apply(user, disk(name, "10Gi")); err != nil {
			t.Fatal(err)
		}
	}
	bindDisk(t, s, system, "on-small", "small", "10Gi")
	bindDisk(t, s, system, "on-big", "big", "10Gi")

	// The node with more memory would win, but the disk is elsewhere; a disk
	// on no node goes with it.
	if _, err := s.backend(registry.Machine).Apply(system, withDisks(machine("ct-a", ""), "on-small", "new")); err != nil {
		t.Fatal(err)
	}
	waitMachine(t, s, system, "ct-a", "placed with its disk", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "small" })
	waitDisk(t, s, system, "new", "bound with it", func(d *storagev1alpha1.Disk) bool { return d.GetStatus().GetNode() == "small" })

	// Disks on two nodes can never be had together.
	if _, err := s.backend(registry.Disk).Apply(user, disk("small-2", "10Gi")); err != nil {
		t.Fatal(err)
	}
	bindDisk(t, s, system, "small-2", "small", "10Gi")
	if _, err := s.backend(registry.Machine).Apply(system, withDisks(machine("ct-b", ""), "on-big", "small-2")); err != nil {
		t.Fatal(err)
	}
	waitMachine(t, s, system, "ct-b", "refused", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetNode() == "" && strings.Contains(scheduled(m).GetMessage(), "lives on node big and disk small-2 on node small")
	})
}

func TestDisksTakeRoomOnTheirNode(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	// 30Gi of disk: a bound 10Gi disk leaves 20, and the machine's own
	// 10Gi root disk plus an unbound 15Gi disk do not fit in it.
	nodeSized(t, s, system, "n1", 4096, 30)
	for name, size := range map[string]string{"old": "10Gi", "big": "15Gi", "small": "5Gi"} {
		if _, err := s.backend(registry.Disk).Apply(user, disk(name, size)); err != nil {
			t.Fatal(err)
		}
	}
	bindDisk(t, s, system, "old", "n1", "10Gi")
	m := machine("ct-a", "")
	m.Spec.Resources = &computev1alpha1.Resources{Memory: "512Mi", Disk: "10Gi"}
	if _, err := s.backend(registry.Machine).Apply(system, withDisks(m, "big")); err != nil {
		t.Fatal(err)
	}
	waitMachine(t, s, system, "ct-a", "refused for lack of disk", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetNode() == "" && scheduled(m).GetMessage() == "no ready node has 25Gi of disk free"
	})
	if getDisk(t, s, system, "big").GetStatus().GetNode() != "" {
		t.Error("the disk of a machine that did not fit was bound")
	}
	// The old disk going makes the room.
	if _, err := s.backend(registry.Disk).Delete(user, resource.Key{Project: "web", Name: "old"}, nil); err != nil {
		t.Fatal(err)
	}
	reportDiskCond(t, s, system, "old", &metav1.Condition{Type: DiskReleased, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Cleaned"})
	waitMachine(t, s, system, "ct-a", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })

	// 25Gi of 30 are held: the bound disk grows only into what is left, and
	// gets its growth once there is room.
	if _, err := s.backend(registry.Disk).Apply(user, disk("big", "20Gi")); err != nil {
		t.Fatal(err)
	}
	waitDisk(t, s, system, "big", "grown", func(d *storagev1alpha1.Disk) bool { return d.GetStatus().GetSize() == "20Gi" })
	if _, err := s.backend(registry.Disk).Apply(user, disk("big", "30Gi")); err != nil {
		t.Fatal(err)
	}
	d := waitDisk(t, s, system, "big", "told it does not fit", func(d *storagev1alpha1.Disk) bool {
		return conditions.Find(d.GetStatus().GetConditions(), DiskAllocated).GetReason() == "ResizeDoesNotFit"
	})
	if d.GetStatus().GetSize() != "20Gi" {
		t.Errorf("a growth that does not fit changed the size to %s", d.GetStatus().GetSize())
	}
	nodeSized(t, s, system, "n1", 4096, 100)
	waitDisk(t, s, system, "big", "grown once there is room", func(d *storagev1alpha1.Disk) bool {
		return d.GetStatus().GetSize() == "30Gi" && conditions.IsTrue(d.GetStatus().GetConditions(), DiskAllocated)
	})
}

func TestDiskControllerCatchesUpOnStart(t *testing.T) {
	t.Parallel()
	s, system, user := running(t)
	if _, err := s.backend(registry.Disk).Apply(user, disk("data", "10Gi")); err != nil {
		t.Fatal(err)
	}
	cfg := s.cfg
	cfg.DisableControllers = false
	next := New(cfg)
	ctx, cancel := context.WithCancel(system)
	done := make(chan error, 1)
	go func() { done <- next.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	waitDisk(t, s, system, "data", "reported", func(d *storagev1alpha1.Disk) bool {
		return d.GetStatus().GetPhase() == storagev1alpha1.DiskPhase_DISK_PHASE_PENDING
	})
}

// TestASettledDiskStaysQuiet: once a disk's status matches, nothing more is
// written to it.
func TestASettledDiskStaysQuiet(t *testing.T) {
	t.Parallel()
	s, system, user := controlled(t, "n1")
	nodeSized(t, s, system, "n1", 4096, 100)
	if _, err := s.backend(registry.Disk).Apply(user, disk("data", "10Gi")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.Machine).Apply(system, withDisks(machine("ct-a", ""), "data")); err != nil {
		t.Fatal(err)
	}
	// The scheduler binds the disk before it writes the machine, so the disk
	// can settle while the machine is still being placed; those writes may
	// wake the disk controller, which must then find nothing to write.
	waitMachine(t, s, system, "ct-a", "scheduled", func(m *computev1alpha1.Machine) bool {
		return conditions.IsTrue(m.GetStatus().GetConditions(), MachineScheduled)
	})
	waitDisk(t, s, system, "data", "bound, attached and allocated", func(d *storagev1alpha1.Disk) bool {
		return d.GetStatus().GetMachine() == "ct-a" && d.GetStatus().GetNode() == "n1" &&
			conditions.IsTrue(d.GetStatus().GetConditions(), DiskAllocated)
	})
	reportDiskCond(t, s, system, "data", &metav1.Condition{Type: DiskReady, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Created"})
	settled := waitDisk(t, s, system, "data", "in use", func(d *storagev1alpha1.Disk) bool {
		return d.GetStatus().GetPhase() == storagev1alpha1.DiskPhase_DISK_PHASE_IN_USE
	})
	waitSettled(t, s)
	if now := getDisk(t, s, system, "data").GetMetadata().GetResourceVersion(); now != settled.GetMetadata().GetResourceVersion() {
		t.Errorf("a settled disk was written again: at %d, then at %d", settled.GetMetadata().GetResourceVersion(), now)
	}
}
