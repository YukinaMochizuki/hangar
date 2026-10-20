package apiserver

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/controller"
)

// tooBig is a share size no test node has room for: its server is not
// placed until it shrinks.
const tooBig = "500Gi"

func waitShare(t *testing.T, s *Server, system context.Context, name, what string, ok func(*storagev1alpha1.FileShare) bool) *storagev1alpha1.FileShare {
	t.Helper()
	return waitObject(t, s, system, registry.FileShare, resource.Key{Project: "web", Name: name}, "file share "+name+" "+what, ok)
}

// letShareGo removes a deleted share's finalizers, as the garbage collector
// and its controller do, where neither runs.
func letShareGo(t *testing.T, s *Server, system context.Context, name string) {
	t.Helper()
	for _, f := range shareFinalizers {
		if err := s.RemoveFinalizer(system, registry.FileShare, resource.Key{Project: "web", Name: name}, f); err != nil {
			t.Fatal(err)
		}
	}
}

// reportShareCond writes c as a share's node does.
func reportShareCond(t *testing.T, s *Server, system context.Context, name, node string, c *metav1.Condition) {
	t.Helper()
	status := &storagev1alpha1.FileShareStatus{Conditions: []*metav1.Condition{c}}
	if _, err := s.backend(registry.FileShare).UpdateStatus(asNode(system, node), resource.Key{Project: "web", Name: name}, status, nil); err != nil {
		t.Fatal(err)
	}
}

func sharePhase(phase storagev1alpha1.FileSharePhase) func(*storagev1alpha1.FileShare) bool {
	return func(fs *storagev1alpha1.FileShare) bool { return fs.GetStatus().GetPhase() == phase }
}

// serverRunning reports the share's server running its current spec, as
// its node will, once it is placed.
func serverRunning(t *testing.T, s *Server, system context.Context, share string) *computev1alpha1.Machine {
	t.Helper()
	m := waitMachine(t, s, system, shareServerName(share), "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() != "" })
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_RUNNING, m.GetMetadata().GetGeneration())
	return m
}

// addrOf is the address of an interface, without its prefix length.
func addrOf(t *testing.T, nic *computev1alpha1.MachineInterface) string {
	t.Helper()
	p, err := netip.ParsePrefix(nic.GetIpv4())
	if err != nil {
		t.Fatalf("interface %s has no address: %v", nic.GetName(), err)
	}
	return p.Addr().String()
}

// TestAShareHasAServer: a share gets a server machine of its own, which it
// owns, of the platform's size, restarted always, with an interface and an
// address on its network and no public one, and which counts toward no
// quota and no capability reaches. The scheduler places it and binds the
// share there; the share is Available once the server runs.
func TestAShareHasAServer(t *testing.T) {
	s, system, user := controlled(t, "n1")
	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Machines: proto.Int32(1)})
	if _, err := s.backend(registry.Node).UpdateStatus(asNode(system, "n1"), resource.Key{Name: "n1"}, &computev1alpha1.NodeStatus{
		Capacity:   &computev1alpha1.NodeResources{Cpu: "8", Memory: "8192Mi", Disk: "100Gi"},
		Conditions: []*metav1.Condition{{Type: NodeReady, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "AgentReporting"}},
		Ipv6Prefix: testPrefix,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.FileShare).Apply(user, fileShare("data", "2Gi")); err != nil {
		t.Fatal(err)
	}
	fs := waitShare(t, s, system, "data", "on n1", func(fs *storagev1alpha1.FileShare) bool { return fs.GetStatus().GetNode() == "n1" })
	if fs.GetStatus().GetSize() != "2Gi" {
		t.Errorf("the share was bound at %q, want 2Gi", fs.GetStatus().GetSize())
	}
	m := waitMachine(t, s, system, "fs-data", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	refs := m.GetMetadata().GetOwnerReferences()
	if len(refs) != 1 || refs[0].GetKind() != registry.FileShare.Kind || refs[0].GetName() != "data" || !ownedBy(m.GetMetadata(), fs.GetMetadata().GetUid()) {
		t.Errorf("the server's owners are %v", refs)
	}
	if !slices.Contains(m.GetMetadata().GetFinalizers(), NodeCleanupFinalizer) {
		t.Errorf("the server's finalizers are %v", m.GetMetadata().GetFinalizers())
	}
	got := proto.Clone(m.GetSpec()).(*computev1alpha1.MachineSpec)
	addr := addrOf(t, got.GetInterfaces()[0])
	got.Interfaces[0].Ipv4 = ""
	want := &computev1alpha1.MachineSpec{
		Node:          "n1",
		Resources:     &computev1alpha1.Resources{Cpu: "1", Memory: "256Mi"},
		RunState:      computev1alpha1.MachineRunState_MACHINE_RUN_STATE_RUNNING,
		RestartPolicy: computev1alpha1.RestartPolicy_RESTART_POLICY_ALWAYS,
		Boot: &computev1alpha1.MachineSpec_FileShareServer{FileShareServer: &computev1alpha1.FileShareServerBoot{
			FileShare: "data", Size: "2Gi",
		}},
		Interfaces: []*computev1alpha1.MachineInterface{{Name: "eth0", Network: "default"}},
	}
	if !proto.Equal(got, want) {
		t.Errorf("the server's spec is %v, want %v", got, want)
	}
	if !netip.MustParsePrefix(DefaultNetworkCIDR).Contains(netip.MustParseAddr(addr)) {
		t.Errorf("the server's address %s is not of the default network", addr)
	}
	waitShare(t, s, system, "data", "giving the server's address", func(fs *storagev1alpha1.FileShare) bool {
		a := fs.GetStatus().GetAddresses()
		return len(a) == 1 && a[0].GetNetwork() == "default" && a[0].GetAddress() == addr
	})
	if fs := getShare(t, s, system, "data"); fs.GetStatus().GetPhase() != storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_PENDING ||
		shareCond(fs, FileShareServerReady).GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE {
		t.Errorf("a share whose server does not run yet: %v", fs.GetStatus())
	}

	// It counts toward no quota: the one machine the plan allows is the
	// container's, which has a public address where the server has none.
	applyCtr(t, s, user, ctr("api"))
	api := waitMachine(t, s, system, machineName("api"), "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	checkIPv6(t, ipv6Of(api))
	if a := ipv6Of(getMachine(t, s, system, resource.Key{Project: "web", Name: "fs-data"})); a != "" {
		t.Errorf("the server has the public address %s", a)
	}

	serverRunning(t, s, system, "data")
	fs = waitShare(t, s, system, "data", "available", sharePhase(storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_AVAILABLE))
	if c := shareCond(fs, FileShareServerReady); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE || c.GetObservedGeneration() != fs.GetMetadata().GetGeneration() {
		t.Errorf("%s is %v", FileShareServerReady, c)
	}

	// No capability reaches it, by the share or by its machine.
	for _, verb := range []authz.Verb{authz.Logs, authz.Exec, authz.SSH} {
		for r, name := range map[*registry.Resource]string{&registry.FileShare: "data", &registry.Machine: "fs-data"} {
			_, err := s.reach(user, *r, resource.Key{Project: "web", Name: name}, verb, nil)
			wantCodeOf(t, err, connect.CodeInvalidArgument, fmt.Sprintf("%s of %s %s", verb, r.Kind, name))
		}
	}
	wantCodeOf(t, machineLogsOf(t, s, auth.Principal{Type: auth.User, ID: "alice@example.com"}, registry.FileShare.Kind, "data"),
		connect.CodeInvalidArgument, "the share's logs")
}

// TestAServerMachineIsThePlatforms: admission takes a server's machine only
// naming its share and the whole Gi of its volume, with no disk, no share
// and no public address.
func TestAServerMachineIsThePlatforms(t *testing.T) {
	s, system, _ := running(t)
	server := func(change func(*computev1alpha1.Machine)) *computev1alpha1.Machine {
		m := machine("fs-data", "")
		m.Spec.Boot = &computev1alpha1.MachineSpec_FileShareServer{FileShareServer: &computev1alpha1.FileShareServerBoot{FileShare: "data", Size: "2Gi"}}
		change(m)
		return m
	}
	if _, err := s.backend(registry.Machine).Apply(system, server(func(*computev1alpha1.Machine) {})); err != nil {
		t.Fatalf("a server's machine: %v", err)
	}
	for what, change := range map[string]func(*computev1alpha1.Machine){
		"naming no share":   func(m *computev1alpha1.Machine) { m.Spec.GetFileShareServer().FileShare = "" },
		"of no size":        func(m *computev1alpha1.Machine) { m.Spec.GetFileShareServer().Size = "" },
		"of a part of a Gi": func(m *computev1alpha1.Machine) { m.Spec.GetFileShareServer().Size = "1500Mi" },
		"attaching a disk":  func(m *computev1alpha1.Machine) { m.Spec.Disks = []*computev1alpha1.MachineDisk{{Disk: "d"}} },
		"declaring a share": func(m *computev1alpha1.Machine) { declaring(m, "data") },
		"a public address":  func(m *computev1alpha1.Machine) { m.Spec.Node, m.Spec.Interfaces[0].Ipv6 = "n1", "2001:db8:0:1::7/128" },
	} {
		_, err := s.backend(registry.Machine).Apply(system, server(change))
		wantCodeOf(t, err, connect.CodeInvalidArgument, "a server's machine "+what)
	}
}

// TestAServerGoesWhereItsDeclarersAre: a share's server goes to the node of
// the placed machines declaring it, though another has more room, and binds
// the share there; a container declaring the share follows it, and one
// placed elsewhere is refused it.
func TestAServerGoesWhereItsDeclarersAre(t *testing.T) {
	s, system, user := controlled(t, "n1", "n2")
	nodeSized(t, s, system, "n1", 2048, 100)
	nodeSized(t, s, system, "n2", 8192, 100)
	if _, err := s.backend(registry.FileShare).Apply(user, fileShare("data", tooBig)); err != nil {
		t.Fatal(err)
	}
	waitMachine(t, s, system, "fs-data", "unschedulable", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetNode() == "" && scheduled(m).GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_FALSE
	})
	waitShare(t, s, system, "data", "waiting for its server", func(fs *storagev1alpha1.FileShare) bool {
		return shareCond(fs, FileShareAllocated).GetReason() == "WaitingForServer" && shareCond(fs, FileShareServerReady).GetReason() == "Unschedulable"
	})
	// A VM's machine, which its disk takes to n1, declares it on no node.
	if _, err := s.backend(registry.Disk).Apply(user, disk("v-root", "1Gi")); err != nil {
		t.Fatal(err)
	}
	bindDisk(t, s, system, "v-root", "n1", "1Gi")
	if _, err := s.backend(registry.Machine).Apply(system, declaring(firmwareMachine("vm-v", "", "v-root"), "data")); err != nil {
		t.Fatal(err)
	}
	waitMachine(t, s, system, "vm-v", "placed with its disk", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })

	if _, err := s.backend(registry.FileShare).Apply(user, fileShare("data", "1Gi")); err != nil {
		t.Fatal(err)
	}
	waitMachine(t, s, system, "fs-data", "placed with its declarer", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() != "" })
	if fs := getShare(t, s, system, "data"); fs.GetStatus().GetNode() != "n1" || fs.GetStatus().GetSize() != "1Gi" {
		t.Errorf("the share is on %q at %q, want n1 at 1Gi", fs.GetStatus().GetNode(), fs.GetStatus().GetSize())
	}
	if node := getMachine(t, s, system, resource.Key{Project: "web", Name: "fs-data"}).GetSpec().GetNode(); node != "n1" {
		t.Errorf("the server is on %s, its declarer on n1", node)
	}

	// A container declaring it goes there.
	c := ctr("web")
	c.Spec.Volumes = []*computev1alpha1.ContainerVolume{shareVol("data", "/srv")}
	applyCtr(t, s, user, c)
	waitMachine(t, s, system, machineName("web"), "placed on the share's node", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })

	// One placed elsewhere is refused it.
	applyCtr(t, s, user, ctr("far"))
	waitMachine(t, s, system, machineName("far"), "placed on n2", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n2" })
	far := ctr("far")
	far.Spec.Volumes = []*computev1alpha1.ContainerVolume{shareVol("data", "/srv")}
	applyCtr(t, s, user, far)
	waitContainer(t, s, system, "far", "refused the share", func(c *computev1alpha1.Container) bool {
		r := containerReady(c)
		return r.GetReason() == "MachineRefused" && strings.Contains(r.GetMessage(), `file share "data" lives on node n1`)
	})
	if declaresShare(getMachine(t, s, system, resource.Key{Project: "web", Name: machineName("far")}).GetSpec(), "data") {
		t.Error("a machine of another node declares the share")
	}
}

// TestAShareChangeRestartsItsServer: a share's networks and size are its
// server's interfaces and volume, which its node starts it again on. Each
// network keeps its address; a growth waits for room on the share's node.
func TestAShareChangeRestartsItsServer(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeSized(t, s, system, "n1", 8192, 100)
	if _, err := s.backend(registry.Network).Apply(user, network("back", "10.1.0.0/24")); err != nil {
		t.Fatal(err)
	}
	apply := func(size string, networks ...string) {
		t.Helper()
		if _, err := s.backend(registry.FileShare).Apply(user, fileShare("data", size, networks...)); err != nil {
			t.Fatal(err)
		}
	}
	apply("1Gi")
	before := serverRunning(t, s, system, "data")
	waitShare(t, s, system, "data", "available", sharePhase(storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_AVAILABLE))
	onDefault := addrOf(t, before.GetSpec().GetInterfaces()[0])

	apply("1Gi", "default", "back")
	m := waitMachine(t, s, system, "fs-data", "on both networks", func(m *computev1alpha1.Machine) bool {
		nics := m.GetSpec().GetInterfaces()
		return len(nics) == 2 && nics[1].GetIpv4() != ""
	})
	nics := m.GetSpec().GetInterfaces()
	if nics[0].GetName() != "eth0" || nics[0].GetNetwork() != "default" || addrOf(t, nics[0]) != onDefault ||
		nics[1].GetName() != "eth1" || nics[1].GetNetwork() != "back" {
		t.Errorf("the server's interfaces are %v; default had %s", nics, onDefault)
	}
	if m.GetMetadata().GetGeneration() <= before.GetMetadata().GetGeneration() {
		t.Error("the server's spec did not change")
	}
	onBack := addrOf(t, nics[1])
	fs := waitShare(t, s, system, "data", "on both networks, its server starting again", func(fs *storagev1alpha1.FileShare) bool {
		return len(fs.GetStatus().GetAddresses()) == 2
	})
	if fs.GetStatus().GetPhase() != storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_PENDING {
		t.Errorf("a share whose server has not started again is %v", fs.GetStatus().GetPhase())
	}

	// Leaving default, back keeps its interface and its address.
	apply("1Gi", "back")
	m = waitMachine(t, s, system, "fs-data", "on back alone", func(m *computev1alpha1.Machine) bool { return len(m.GetSpec().GetInterfaces()) == 1 })
	if nic := m.GetSpec().GetInterfaces()[0]; nic.GetName() != "eth1" || nic.GetNetwork() != "back" || addrOf(t, nic) != onBack {
		t.Errorf("the server's interface is %v; back had %s", nic, onBack)
	}
	waitShare(t, s, system, "data", "giving its address on back", func(fs *storagev1alpha1.FileShare) bool {
		a := fs.GetStatus().GetAddresses()
		return len(a) == 1 && a[0].GetNetwork() == "back" && a[0].GetAddress() == onBack
	})

	// Grown, the server boots on the grown volume.
	gen := m.GetMetadata().GetGeneration()
	apply("3Gi", "back")
	m = waitMachine(t, s, system, "fs-data", "on 3Gi", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetFileShareServer().GetSize() == "3Gi" })
	if m.GetMetadata().GetGeneration() <= gen {
		t.Error("growing the share did not change its server")
	}
	waitShare(t, s, system, "data", "at 3Gi", func(fs *storagev1alpha1.FileShare) bool { return fs.GetStatus().GetSize() == "3Gi" })

	// A growth its node has no room for waits, and the server keeps its
	// volume.
	apply("200Gi", "back")
	waitShare(t, s, system, "data", "waiting for room", func(fs *storagev1alpha1.FileShare) bool {
		c := shareCond(fs, FileShareAllocated)
		return c.GetReason() == "ResizeDoesNotFit" && strings.Contains(c.GetMessage(), "growing the file share to 200Gi")
	})
	waitSettled(t, s)
	if fs := getShare(t, s, system, "data"); fs.GetStatus().GetSize() != "3Gi" {
		t.Errorf("a growth that does not fit made the share %s", fs.GetStatus().GetSize())
	}
	if m := getMachine(t, s, system, resource.Key{Project: "web", Name: "fs-data"}); m.GetSpec().GetFileShareServer().GetSize() != "3Gi" {
		t.Errorf("a growth that does not fit gave the server %s", m.GetSpec().GetFileShareServer().GetSize())
	}
	// Room made on its node lets it grow.
	nodeSized(t, s, system, "n1", 8192, 300)
	waitShare(t, s, system, "data", "at 200Gi", func(fs *storagev1alpha1.FileShare) bool {
		return fs.GetStatus().GetSize() == "200Gi" && shareCond(fs, FileShareAllocated).GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_TRUE
	})
	waitMachine(t, s, system, "fs-data", "on 200Gi", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetFileShareServer().GetSize() == "200Gi" })
}

// TestAShareStatusIsDerived: a share's users are the machines declaring it,
// as their specs have it, and its phase follows its server and its users;
// its node writes its own conditions and nothing else.
func TestAShareStatusIsDerived(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeSized(t, s, system, "n1", 8192, 100)
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.Network).Apply(user, network("back", "10.1.0.0/24")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.FileShare).Apply(user, fileShare("data", "1Gi", "default", "back")); err != nil {
		t.Fatal(err)
	}
	serverRunning(t, s, system, "data")
	waitShare(t, s, system, "data", "available", sharePhase(storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_AVAILABLE))

	c := ctr("web")
	c.Spec.Volumes = []*computev1alpha1.ContainerVolume{{Source: "data", SourceKind: "FileShare", Target: "/srv", ReadOnly: true}}
	applyCtr(t, s, user, c)
	vm := vmFromImage("db", "noble")
	vm.Spec.Interfaces = []*computev1alpha1.NetworkInterface{{Network: "back"}}
	vm.Spec.FileShares = []*computev1alpha1.VirtualMachineFileShare{{Name: "data"}}
	if _, err := s.backend(registry.VirtualMachine).Apply(user, vm); err != nil {
		t.Fatal(err)
	}
	wantUsers := []*storagev1alpha1.FileShareUser{
		{Kind: "Container", Name: "web", Machine: "ct-web", Network: "default", ReadOnly: true, Path: "/srv"},
		{Kind: "VirtualMachine", Name: "db", Machine: "vm-db", Network: "back"},
	}
	usersAre := func(want ...*storagev1alpha1.FileShareUser) func(*storagev1alpha1.FileShare) bool {
		return func(fs *storagev1alpha1.FileShare) bool {
			return slices.EqualFunc(fs.GetStatus().GetUsers(), want, func(a, b *storagev1alpha1.FileShareUser) bool { return proto.Equal(a, b) })
		}
	}
	fs := waitShare(t, s, system, "data", "used by both", usersAre(wantUsers...))
	if fs.GetStatus().GetPhase() != storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_IN_USE {
		t.Errorf("a share two machines use is %v", fs.GetStatus().GetPhase())
	}

	// Its node writes its own conditions, and nothing else: not where the
	// share is, nor its size, phase, addresses or users, nor the
	// apiserver's conditions.
	waitSettled(t, s)
	before := getShare(t, s, system, "data")
	nodeSays := &metav1.Condition{Type: "Ready", Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Made"}
	if _, err := s.backend(registry.FileShare).UpdateStatus(asNode(system, "n1"), resource.Key{Project: "web", Name: "data"}, &storagev1alpha1.FileShareStatus{
		Phase: storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_AVAILABLE, Node: "n2", Size: "9Gi",
		Addresses: []*storagev1alpha1.FileShareAddress{{Network: "default", Address: "10.0.0.250"}},
		Users:     []*storagev1alpha1.FileShareUser{{Kind: "Container", Name: "intruder", Machine: "ct-intruder", Network: "default"}},
		Conditions: []*metav1.Condition{nodeSays, {
			Type: FileShareServerReady, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: "Lies",
		}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	after := getShare(t, s, system, "data")
	if c := shareCond(after, "Ready"); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE || c.GetReason() != "Made" {
		t.Errorf("the node's condition is %v", c)
	}
	was, now := proto.Clone(before.GetStatus()).(*storagev1alpha1.FileShareStatus), proto.Clone(after.GetStatus()).(*storagev1alpha1.FileShareStatus)
	now.Conditions = slices.DeleteFunc(now.Conditions, func(c *metav1.Condition) bool { return c.GetType() == "Ready" })
	if !proto.Equal(was, now) {
		t.Errorf("the node changed the share's status to %v, was %v", now, was)
	}

	// A machine being deleted no longer uses it.
	old := ctr("old")
	old.Spec.Volumes = []*computev1alpha1.ContainerVolume{shareVol("data", "/old")}
	applyCtr(t, s, user, old)
	waitShare(t, s, system, "data", "used by three", func(fs *storagev1alpha1.FileShare) bool { return len(fs.GetStatus().GetUsers()) == 3 })
	if _, err := s.backend(registry.Container).Delete(user, resource.Key{Project: "web", Name: "old"}, nil); err != nil {
		t.Fatal(err)
	}
	waitMachine(t, s, system, machineName("old"), "being deleted", func(m *computev1alpha1.Machine) bool { return m.GetMetadata().GetDeleteTime() != nil })
	waitShare(t, s, system, "data", "used by the two again", usersAre(wantUsers...))

	// A machine that stops declaring it stops using it.
	applyCtr(t, s, user, ctr("web"))
	waitShare(t, s, system, "data", "used by the VM alone", usersAre(wantUsers[1]))
	vm.Spec.FileShares = nil
	if _, err := s.backend(registry.VirtualMachine).Apply(user, vm); err != nil {
		t.Fatal(err)
	}
	fs = waitShare(t, s, system, "data", "used by none", usersAre())
	if fs.GetStatus().GetPhase() != storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_AVAILABLE {
		t.Errorf("a share no machine uses is %v", fs.GetStatus().GetPhase())
	}
}

// TestDeletingAShareWaitsForItsServerAndNode: a deleted share deletes its
// server, and goes once the server is gone and its node has removed its
// volume; one never placed goes with its server at once.
func TestDeletingAShareWaitsForItsServerAndNode(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeSized(t, s, system, "n1", 8192, 100)
	key := resource.Key{Project: "web", Name: "data"}
	if _, err := s.backend(registry.FileShare).Apply(user, fileShare("data", "1Gi")); err != nil {
		t.Fatal(err)
	}
	waitShare(t, s, system, "data", "on n1", func(fs *storagev1alpha1.FileShare) bool { return fs.GetStatus().GetNode() == "n1" })
	serverRunning(t, s, system, "data")
	if _, err := s.backend(registry.FileShare).Delete(user, key, nil); err != nil {
		t.Fatal(err)
	}
	m := waitMachine(t, s, system, "fs-data", "deleted with its share", func(m *computev1alpha1.Machine) bool {
		return m.GetMetadata().GetDeleteTime() != nil
	})
	// Its node saying the volume is gone does not let the share go before
	// its server.
	released := &metav1.Condition{Type: FileShareReleased, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Released"}
	reportShareCond(t, s, system, "data", "n1", released)
	waitSettled(t, s)
	if getShare(t, s, system, "data") == nil {
		t.Fatal("the share went before its server")
	}
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, m.GetMetadata().GetGeneration(),
		&metav1.Condition{Type: MachineReleased, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Cleaned"})
	waitGone(t, s, system, registry.Machine, "fs-data")
	waitGone(t, s, system, registry.FileShare, "data")

	// Gone before its node released the volume, it waits for that.
	if _, err := s.backend(registry.FileShare).Apply(user, fileShare("logs", "1Gi")); err != nil {
		t.Fatal(err)
	}
	waitShare(t, s, system, "logs", "on n1", func(fs *storagev1alpha1.FileShare) bool { return fs.GetStatus().GetNode() == "n1" })
	waitMachine(t, s, system, "fs-logs", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	if _, err := s.backend(registry.FileShare).Delete(user, resource.Key{Project: "web", Name: "logs"}, nil); err != nil {
		t.Fatal(err)
	}
	m = waitMachine(t, s, system, "fs-logs", "deleted with its share", func(m *computev1alpha1.Machine) bool {
		return m.GetMetadata().GetDeleteTime() != nil
	})
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, m.GetMetadata().GetGeneration(),
		&metav1.Condition{Type: MachineReleased, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Cleaned"})
	waitGone(t, s, system, registry.Machine, "fs-logs")
	waitSettled(t, s)
	fs := getShare(t, s, system, "logs")
	if fs == nil || !slices.Equal(fs.GetMetadata().GetFinalizers(), []string{NodeCleanupFinalizer}) {
		t.Fatalf("a share whose node has not released it: %v", fs.GetMetadata())
	}
	// Its server gone, it says so, and is reached nowhere.
	if st := fs.GetStatus(); len(st.GetAddresses()) != 0 || st.GetPhase() != storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_PENDING ||
		shareCond(fs, FileShareServerReady).GetReason() != "Deleted" {
		t.Errorf("a deleted share without a server: %v", st)
	}
	reportShareCond(t, s, system, "logs", "n1", released)
	waitGone(t, s, system, registry.FileShare, "logs")

	// Never placed, it has nothing to release.
	if _, err := s.backend(registry.FileShare).Apply(user, fileShare("idle", tooBig)); err != nil {
		t.Fatal(err)
	}
	waitMachine(t, s, system, "fs-idle", "unschedulable", func(m *computev1alpha1.Machine) bool { return scheduled(m) != nil })
	if _, err := s.backend(registry.FileShare).Delete(user, resource.Key{Project: "web", Name: "idle"}, nil); err != nil {
		t.Fatal(err)
	}
	waitGone(t, s, system, registry.Machine, "fs-idle")
	waitGone(t, s, system, registry.FileShare, "idle")
}

// TestFileShareControllerCatchesUpOnStart: a share stored while no
// controller ran, before shares had their finalizers, gets them and its
// server once one does.
func TestFileShareControllerCatchesUpOnStart(t *testing.T) {
	s, system, user := running(t)
	if _, err := s.backend(registry.FileShare).Apply(user, fileShare("data", "1Gi")); err != nil {
		t.Fatal(err)
	}
	letShareGo(t, s, system, "data")
	if f := getShare(t, s, system, "data").GetMetadata().GetFinalizers(); len(f) != 0 {
		t.Fatalf("finalizers %v", f)
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
	fs := waitShare(t, s, system, "data", "with its finalizers", func(fs *storagev1alpha1.FileShare) bool {
		return len(fs.GetMetadata().GetFinalizers()) == 2
	})
	for _, f := range shareFinalizers {
		if !slices.Contains(fs.GetMetadata().GetFinalizers(), f) {
			t.Errorf("finalizers %v, want %v", fs.GetMetadata().GetFinalizers(), shareFinalizers)
		}
	}
	m := waitMachine(t, s, system, "fs-data", "made", func(*computev1alpha1.Machine) bool { return true })
	if !ownedBy(m.GetMetadata(), fs.GetMetadata().GetUid()) {
		t.Errorf("the server's owners are %v", m.GetMetadata().GetOwnerReferences())
	}
	waitShare(t, s, system, "data", "saying its server has no node", func(fs *storagev1alpha1.FileShare) bool {
		return shareCond(fs, FileShareServerReady).GetReason() == "Unschedulable"
	})
}

// TestASettledShareStaysQuiet: once a share and its server match, nothing
// more is written to either.
func TestASettledShareStaysQuiet(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeSized(t, s, system, "n1", 8192, 100)
	if _, err := s.backend(registry.FileShare).Apply(user, fileShare("data", "1Gi")); err != nil {
		t.Fatal(err)
	}
	serverRunning(t, s, system, "data")
	c := ctr("web")
	c.Spec.Volumes = []*computev1alpha1.ContainerVolume{shareVol("data", "/srv")}
	applyCtr(t, s, user, c)
	waitShare(t, s, system, "data", "in use", sharePhase(storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_IN_USE))
	waitSettled(t, s)
	share := getShare(t, s, system, "data").GetMetadata().GetResourceVersion()
	server := getMachine(t, s, system, resource.Key{Project: "web", Name: "fs-data"}).GetMetadata().GetResourceVersion()
	// Another node's heartbeat, a network, a disk: each maps to shares.
	nodeSized(t, s, system, "n1", 8192, 100)
	if _, err := s.backend(registry.Network).Apply(user, network("back", "10.1.0.0/24")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.Disk).Apply(user, disk("d", "1Gi")); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, s)
	if now := getShare(t, s, system, "data").GetMetadata().GetResourceVersion(); now != share {
		t.Errorf("a settled share was written again: at %d, then at %d", share, now)
	}
	if now := getMachine(t, s, system, resource.Key{Project: "web", Name: "fs-data"}).GetMetadata().GetResourceVersion(); now != server {
		t.Errorf("a settled server was written again: at %d, then at %d", server, now)
	}
}

// TestGrowingIsOneDecision: a share and a disk growing on a node with room
// for one growth: only one gets it.
func TestGrowingIsOneDecision(t *testing.T) {
	s, system, user := runningWith(t, func(c *Config) { c.Nodes = []string{"n1"} })
	waitNode(t, s, "n1")
	for round := range 10 {
		share, d := fmt.Sprintf("s%d", round), fmt.Sprintf("d%d", round)
		if _, err := s.backend(registry.FileShare).Apply(user, fileShare(share, "1Gi")); err != nil {
			t.Fatal(err)
		}
		bindShare(t, s, system, share, "n1", "1Gi")
		if _, err := s.backend(registry.Disk).Apply(user, disk(d, "1Gi")); err != nil {
			t.Fatal(err)
		}
		bindDisk(t, s, system, d, "n1", "1Gi")
		_, used, err := s.nodeUsage(system, resource.Key{})
		if err != nil {
			t.Fatal(err)
		}
		// Room for one growth of 50Gi, not two.
		nodeSized(t, s, system, "n1", 8192, int64(used["n1"].disk>>30)+75)
		if _, err := s.backend(registry.FileShare).Apply(user, fileShare(share, "51Gi")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.backend(registry.Disk).Apply(user, disk(d, "51Gi")); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Go(func() { errs[0] = s.reconcileFileShare(system, controller.Key{Project: "web", Name: share}) })
		wg.Go(func() { errs[1] = s.reconcileDisk(system, controller.Key{Project: "web", Name: d}) })
		wg.Wait()
		if errs[0] != nil || errs[1] != nil {
			t.Fatal(errs)
		}
		grown := 0
		if getShare(t, s, system, share).GetStatus().GetSize() == "51Gi" {
			grown++
		}
		if getDisk(t, s, system, d).GetStatus().GetSize() == "51Gi" {
			grown++
		}
		if grown != 1 {
			t.Fatalf("round %d: %d growths of 50Gi on a node with room for one", round, grown)
		}
	}
}

// TestSharesTakeRoomOnTheirNode: a share's volume takes its size on its
// node, which a server is placed only with room for, and which it keeps
// until its node has released it; what waited for the room goes then.
func TestSharesTakeRoomOnTheirNode(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeSized(t, s, system, "n1", 8192, 100)
	if _, err := s.backend(registry.FileShare).Apply(user, fileShare("big", "80Gi")); err != nil {
		t.Fatal(err)
	}
	waitShare(t, s, system, "big", "on n1", func(fs *storagev1alpha1.FileShare) bool { return fs.GetStatus().GetNode() == "n1" })
	if _, err := s.backend(registry.FileShare).Apply(user, fileShare("more", "30Gi")); err != nil {
		t.Fatal(err)
	}
	waitMachine(t, s, system, "fs-more", "without room", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetNode() == "" && strings.Contains(scheduled(m).GetMessage(), "30Gi of disk")
	})
	c := ctr("api")
	c.Spec.Resources = &computev1alpha1.Resources{Disk: "30Gi"}
	applyCtr(t, s, user, c)
	waitMachine(t, s, system, machineName("api"), "without room", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetNode() == "" && scheduled(m).GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_FALSE
	})

	if _, err := s.backend(registry.FileShare).Delete(user, resource.Key{Project: "web", Name: "big"}, nil); err != nil {
		t.Fatal(err)
	}
	m := waitMachine(t, s, system, "fs-big", "deleted with its share", func(m *computev1alpha1.Machine) bool {
		return m.GetMetadata().GetDeleteTime() != nil
	})
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, m.GetMetadata().GetGeneration(),
		&metav1.Condition{Type: MachineReleased, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Cleaned"})
	waitGone(t, s, system, registry.Machine, "fs-big")
	waitSettled(t, s)
	if node := getMachine(t, s, system, resource.Key{Project: "web", Name: machineName("api")}).GetSpec().GetNode(); node != "" {
		t.Fatalf("placed on %s while the share's node keeps its volume", node)
	}
	reportShareCond(t, s, system, "big", "n1", &metav1.Condition{Type: FileShareReleased, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Released"})
	waitGone(t, s, system, registry.FileShare, "big")
	waitMachine(t, s, system, machineName("api"), "placed once the volume is gone", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	waitMachine(t, s, system, "fs-more", "placed once the volume is gone", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
}

// TestAGrowthWaitsForRoom: a share's growth its node has no room for is
// granted once a disk or another share on the node goes.
func TestAGrowthWaitsForRoom(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeSized(t, s, system, "n1", 8192, 100)
	if _, err := s.backend(registry.Disk).Apply(user, disk("d", "40Gi")); err != nil {
		t.Fatal(err)
	}
	bindDisk(t, s, system, "d", "n1", "40Gi")
	for name, size := range map[string]string{"logs": "20Gi", "data": "1Gi"} {
		if _, err := s.backend(registry.FileShare).Apply(user, fileShare(name, size)); err != nil {
			t.Fatal(err)
		}
		waitShare(t, s, system, name, "on n1", func(fs *storagev1alpha1.FileShare) bool { return fs.GetStatus().GetNode() == "n1" })
	}
	grow := func(size string) {
		t.Helper()
		if _, err := s.backend(registry.FileShare).Apply(user, fileShare("data", size)); err != nil {
			t.Fatal(err)
		}
		waitShare(t, s, system, "data", "waiting for room for "+size, func(fs *storagev1alpha1.FileShare) bool {
			return shareCond(fs, FileShareAllocated).GetReason() == "ResizeDoesNotFit" && fs.GetMetadata().GetGeneration() == shareCond(fs, FileShareAllocated).GetObservedGeneration()
		})
	}
	grown := func(size string) {
		t.Helper()
		waitShare(t, s, system, "data", "at "+size, func(fs *storagev1alpha1.FileShare) bool { return fs.GetStatus().GetSize() == size })
	}

	// A disk going makes room.
	grow("50Gi")
	if _, err := s.backend(registry.Disk).Delete(user, resource.Key{Project: "web", Name: "d"}, nil); err != nil {
		t.Fatal(err)
	}
	waitDisk(t, s, system, "d", "waiting for its node", func(d *storagev1alpha1.Disk) bool {
		return !slices.Contains(d.GetMetadata().GetFinalizers(), DiskInUseFinalizer)
	})
	reportDiskCond(t, s, system, "d", &metav1.Condition{Type: DiskReleased, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Released"})
	waitGone(t, s, system, registry.Disk, "d")
	grown("50Gi")

	// So does another share going, once its node has released its volume.
	grow("90Gi")
	if _, err := s.backend(registry.FileShare).Delete(user, resource.Key{Project: "web", Name: "logs"}, nil); err != nil {
		t.Fatal(err)
	}
	m := waitMachine(t, s, system, "fs-logs", "deleted with its share", func(m *computev1alpha1.Machine) bool {
		return m.GetMetadata().GetDeleteTime() != nil
	})
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, m.GetMetadata().GetGeneration(),
		&metav1.Condition{Type: MachineReleased, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Cleaned"})
	waitGone(t, s, system, registry.Machine, "fs-logs")
	reportShareCond(t, s, system, "logs", "n1", &metav1.Condition{Type: FileShareReleased, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Released"})
	waitGone(t, s, system, registry.FileShare, "logs")
	grown("90Gi")

	// And a machine going, with its root disk.
	c := ctr("api")
	c.Spec.Resources = &computev1alpha1.Resources{Disk: "5Gi"}
	applyCtr(t, s, user, c)
	waitMachine(t, s, system, machineName("api"), "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	grow("96Gi")
	if _, err := s.backend(registry.Container).Delete(user, resource.Key{Project: "web", Name: "api"}, nil); err != nil {
		t.Fatal(err)
	}
	m = waitMachine(t, s, system, machineName("api"), "deleted with its container", func(m *computev1alpha1.Machine) bool {
		return m.GetMetadata().GetDeleteTime() != nil
	})
	reportMachine(t, s, system, m, computev1alpha1.MachinePhase_MACHINE_PHASE_STOPPED, m.GetMetadata().GetGeneration(),
		&metav1.Condition{Type: MachineReleased, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Cleaned"})
	waitGone(t, s, system, registry.Machine, machineName("api"))
	grown("96Gi")
}

// TestAServerWaitsForAnAddress: a server on a network not made yet, or
// full, says so, and gets its address once the network is made, or a
// machine on it goes.
func TestAServerWaitsForAnAddress(t *testing.T) {
	s, system, user := controlled(t)
	if _, err := s.backend(registry.FileShare).Apply(user, fileShare("data", "1Gi", "later")); err != nil {
		t.Fatal(err)
	}
	waitShare(t, s, system, "data", "waiting for its network", func(fs *storagev1alpha1.FileShare) bool {
		return shareCond(fs, FileShareServerReady).GetReason() == ReasonNetworkNotReady
	})
	if _, err := s.backend(registry.Network).Apply(user, network("later", "10.1.0.0/24")); err != nil {
		t.Fatal(err)
	}
	waitMachine(t, s, system, "fs-data", "on its network", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetInterfaces()[0].GetIpv4() != ""
	})

	// A /28 holds 13 machines.
	if _, err := s.backend(registry.Network).Apply(user, network("tiny", "10.2.0.0/28")); err != nil {
		t.Fatal(err)
	}
	for i := range 13 {
		m := machineOn(machine(fmt.Sprintf("ct-%d", i), ""), "tiny")
		m.Spec.Interfaces[0].Ipv4 = fmt.Sprintf("10.2.0.%d/28", i+2)
		if _, err := s.backend(registry.Machine).Apply(system, m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.backend(registry.FileShare).Apply(user, fileShare("full", "1Gi", "tiny")); err != nil {
		t.Fatal(err)
	}
	waitShare(t, s, system, "full", "told its network is full", func(fs *storagev1alpha1.FileShare) bool {
		return shareCond(fs, FileShareServerReady).GetReason() == ReasonNoAddressLeft
	})
	waitSettled(t, s)
	if _, err := s.backend(registry.Machine).Delete(system, resource.Key{Project: "web", Name: "ct-7"}, nil); err != nil {
		t.Fatal(err)
	}
	m := waitMachine(t, s, system, "fs-full", "given the freed address", func(*computev1alpha1.Machine) bool { return true })
	if a := m.GetSpec().GetInterfaces()[0].GetIpv4(); a != "10.2.0.9/28" {
		t.Errorf("the server's address is %s, want the one freed, 10.2.0.9/28", a)
	}
}

// TestAStaleReportKeepsTheBind: the controller's status write is
// conditional on the share as it read it, so a report from a read made
// before the scheduler bound the share is refused, and the bind stays.
func TestAStaleReportKeepsTheBind(t *testing.T) {
	s, system, user := runningWith(t, func(c *Config) { c.Nodes = []string{"n1"} })
	waitNode(t, s, "n1")
	if _, err := s.backend(registry.FileShare).Apply(user, fileShare("data", "1Gi")); err != nil {
		t.Fatal(err)
	}
	stale := getShare(t, s, system, "data")
	bindShare(t, s, system, "data", "n1", "1Gi")
	err := s.reportFileShare(system, resource.Key{Project: "web", Name: "data"}, stale, nil, nil, nil, nil)
	wantCodeOf(t, err, connect.CodeAborted, "a report from a read before the bind")
	if fs := getShare(t, s, system, "data"); fs.GetStatus().GetNode() != "n1" || fs.GetStatus().GetSize() != "1Gi" {
		t.Errorf("the bind was undone: %v", fs.GetStatus())
	}
}
