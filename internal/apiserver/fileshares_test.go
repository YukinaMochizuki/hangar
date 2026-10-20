package apiserver

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/authz"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

func fileShare(name, size string, networks ...string) *storagev1alpha1.FileShare {
	return &storagev1alpha1.FileShare{
		Metadata: &metav1.ObjectMeta{Project: "web", Name: name},
		Spec:     &storagev1alpha1.FileShareSpec{Size: size, Networks: networks},
	}
}

func getShare(t *testing.T, s *Server, system context.Context, name string) *storagev1alpha1.FileShare {
	t.Helper()
	obj, err := s.get(system, registry.FileShare, resource.Key{Project: "web", Name: name})
	if err != nil {
		t.Fatal(err)
	}
	fs, _ := obj.(*storagev1alpha1.FileShare)
	return fs
}

// bindShare puts a share on node at size, as placing its server will.
func bindShare(t *testing.T, s *Server, system context.Context, name, node, size string) {
	t.Helper()
	status := &storagev1alpha1.FileShareStatus{Node: node, Size: size}
	if _, err := s.backend(registry.FileShare).UpdateStatus(system, resource.Key{Project: "web", Name: name}, status, nil); err != nil {
		t.Fatal(err)
	}
}

// declaring is m declaring each of shares, read-write.
func declaring(m *computev1alpha1.Machine, shares ...string) *computev1alpha1.Machine {
	for _, name := range shares {
		m.Spec.FileShares = append(m.Spec.FileShares, &computev1alpha1.MachineFileShare{FileShare: name})
	}
	return m
}

// machineOn is m with one interface on each of networks, in order.
func machineOn(m *computev1alpha1.Machine, networks ...string) *computev1alpha1.Machine {
	m.Spec.Interfaces = nil
	for i, n := range networks {
		m.Spec.Interfaces = append(m.Spec.Interfaces, &computev1alpha1.MachineInterface{Name: fmt.Sprintf("eth%d", i), Network: n})
	}
	return m
}

func shareVol(share, target string) *computev1alpha1.ContainerVolume {
	return &computev1alpha1.ContainerVolume{Source: share, SourceKind: "FileShare", Target: target}
}

func TestFileShareAdmission(t *testing.T) {
	s, system, user := running(t)
	b := s.backend(registry.FileShare)
	nine := make([]string, 9)
	for i := range nine {
		nine[i] = fmt.Sprintf("n%d", i)
	}
	squashed := func(mode storagev1alpha1.FileShareSquash, uid, gid *uint32) *storagev1alpha1.FileShare {
		fs := fileShare("fs", "10Gi")
		fs.Spec.Squash, fs.Spec.AnonUid, fs.Spec.AnonGid = mode, uid, gid
		return fs
	}
	for name, tt := range map[string]struct {
		fs *storagev1alpha1.FileShare
		ok bool
	}{
		"10Gi on default":            {fileShare("fs", "10Gi"), true},
		"1Gi":                        {fileShare("fs", "1Gi"), true},
		"not whole Gi":               {fileShare("fs", "1500M"), true},
		"64Ti":                       {fileShare("fs", "64Ti"), true},
		"no size":                    {fileShare("fs", ""), false},
		"below 1Gi":                  {fileShare("fs", "1023Mi"), false},
		"above 64Ti":                 {fileShare("fs", "65Ti"), false},
		"a size not a quantity":      {fileShare("fs", "lots"), false},
		"eight networks":             {fileShare("fs", "10Gi", nine[:8]...), true},
		"nine networks":              {fileShare("fs", "10Gi", nine...), false},
		"a network twice":            {fileShare("fs", "10Gi", "default", "default"), false},
		"a network not a name":       {fileShare("fs", "10Gi", "Back_End"), false},
		"no squash, named":           {squashed(storagev1alpha1.FileShareSquash_FILE_SHARE_SQUASH_NONE, nil, nil), true},
		"root squashed":              {squashed(storagev1alpha1.FileShareSquash_FILE_SHARE_SQUASH_ROOT, nil, nil), true},
		"all squashed to 1000":       {squashed(storagev1alpha1.FileShareSquash_FILE_SHARE_SQUASH_ALL, proto.Uint32(1000), proto.Uint32(1000)), true},
		"all squashed to root":       {squashed(storagev1alpha1.FileShareSquash_FILE_SHARE_SQUASH_ALL, proto.Uint32(0), proto.Uint32(0)), true},
		"an unknown squash":          {squashed(storagev1alpha1.FileShareSquash(7), nil, nil), false},
		"an anonymous user unsquash": {squashed(storagev1alpha1.FileShareSquash_FILE_SHARE_SQUASH_UNSPECIFIED, proto.Uint32(1000), nil), false},
		"an anonymous group, NONE":   {squashed(storagev1alpha1.FileShareSquash_FILE_SHARE_SQUASH_NONE, nil, proto.Uint32(1000)), false},
		"uid -1":                     {squashed(storagev1alpha1.FileShareSquash_FILE_SHARE_SQUASH_ALL, proto.Uint32(noUID), nil), false},
		"gid -1":                     {squashed(storagev1alpha1.FileShareSquash_FILE_SHARE_SQUASH_ROOT, nil, proto.Uint32(noUID)), false},
	} {
		_, err := b.Apply(user, tt.fs)
		if got := err == nil; got != tt.ok {
			t.Errorf("%s: accepted %t, want %t (%v)", name, got, tt.ok, err)
		}
		if err == nil {
			if _, err := b.Delete(user, resource.Key{Project: "web", Name: "fs"}, nil); err != nil {
				t.Fatal(err)
			}
		}
	}

	// A share not yet made can shrink; made, it grows but never shrinks, and
	// a growth that never took effect can be withdrawn.
	if _, err := b.Apply(user, fileShare("data", "20Gi")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Apply(user, fileShare("data", "10Gi")); err != nil {
		t.Errorf("shrinking a share no node has made: %v", err)
	}
	bindShare(t, s, system, "data", "n1", "10Gi")
	_, err := b.Apply(user, fileShare("data", "9Gi"))
	wantCodeOf(t, err, connect.CodeInvalidArgument, "shrinking a made share")
	for _, size := range []string{"20Gi", "10Gi", "10240Mi", "15Gi"} {
		if _, err := b.Apply(user, fileShare("data", size)); err != nil {
			t.Errorf("resizing a made share to %s: %v", size, err)
		}
	}
	// Only a change of size is checked: an apply that leaves it as it was
	// passes, even when the share has more.
	bindShare(t, s, system, "data", "n1", "30Gi")
	if _, err := b.Apply(user, fileShare("data", "15Gi")); err != nil {
		t.Errorf("applying a share again at the size it asks for: %v", err)
	}
}

// bindProjectRole binds email the role on project web, as its owner does.
func bindProjectRole(t *testing.T, s *Server, system context.Context, email, role string) context.Context {
	t.Helper()
	addMember(t, s, email)
	if _, err := s.backend(registry.RoleBinding).Apply(system, &iamv1alpha1.RoleBinding{
		Metadata: &metav1.ObjectMeta{Name: usernameOf(email) + "-web"},
		Spec: &iamv1alpha1.RoleBindingSpec{
			Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: email}},
			Role:    role,
			Scope:   &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Project{Project: "web"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return auth.WithPrincipal(system, auth.Principal{Type: auth.User, ID: email})
}

// TestFileSharePermissions: storage.fileshares.* are the kind's
// permissions, held by the project's roles as a Disk's are; only the system
// and the share's node write its status, whatever the role.
func TestFileSharePermissions(t *testing.T) {
	for _, v := range []authz.Verb{authz.Get, authz.List, authz.Watch, authz.Apply, authz.Delete, authz.UpdateStatus} {
		perm := "storage.fileshares." + string(v)
		if r, verb, ok := authz.LookupPermission(perm); !ok || r.Kind != registry.FileShare.Kind || verb != v {
			t.Errorf("%s is %v %v %t, want FileShare %s", perm, r.Kind, verb, ok, v)
		}
	}
	s, system, owner := running(t)
	root := asRoot(t, s, system)
	editor := bindProjectRole(t, s, system, "bob@example.com", "project.editor")
	viewer := bindProjectRole(t, s, system, "carol@example.com", "project.viewer")
	addMember(t, s, "dave@example.com")
	outsider := auth.WithPrincipal(system, auth.Principal{Type: auth.User, ID: "dave@example.com"})
	b := s.backend(registry.FileShare)
	key := resource.Key{Project: "web", Name: "data"}

	for who, ctx := range map[string]context.Context{"owner": owner, "editor": editor} {
		if _, err := b.Apply(ctx, fileShare("data", "10Gi")); err != nil {
			t.Errorf("the project's %s applying a share: %v", who, err)
		}
	}
	for who, ctx := range map[string]context.Context{"viewer": viewer, "outsider": outsider} {
		_, err := b.Apply(ctx, fileShare("data", "20Gi"))
		wantCodeOf(t, err, connect.CodePermissionDenied, who+" applying a share")
		_, err = b.Delete(ctx, key, nil)
		wantCodeOf(t, err, connect.CodePermissionDenied, who+" deleting a share")
	}
	if _, err := b.Get(viewer, key); err != nil {
		t.Errorf("the project's viewer reading a share: %v", err)
	}
	_, err := b.Get(outsider, key)
	wantCodeOf(t, err, connect.CodePermissionDenied, "someone outside the project reading a share")

	status := &storagev1alpha1.FileShareStatus{Node: "n1", Size: "10Gi"}
	for who, ctx := range map[string]context.Context{"owner": owner, "root": root, "node": asNode(system, "n1")} {
		_, err := b.UpdateStatus(ctx, key, status, nil)
		wantCodeOf(t, err, connect.CodePermissionDenied, who+" writing an unplaced share's status")
	}
	bindShare(t, s, system, "data", "n1", "10Gi")
	if _, err := b.UpdateStatus(asNode(system, "n1"), key, status, nil); err != nil {
		t.Errorf("the share's node reporting: %v", err)
	}
	for who, ctx := range map[string]context.Context{"owner": owner, "root": root, "another node": asNode(system, "n2")} {
		_, err := b.UpdateStatus(ctx, key, status, nil)
		wantCodeOf(t, err, connect.CodePermissionDenied, who+" writing a placed share's status")
	}
	if _, err := b.Delete(editor, key, nil); err != nil {
		t.Errorf("the project's editor deleting a share: %v", err)
	}
}

// TestAShareIsReachedFromItsNetworks: a container, a VM or a machine
// declaring a share on none of its networks is refused, told the share's
// networks; one declaring a share not made yet is let through, and its
// machine held back.
func TestAShareIsReachedFromItsNetworks(t *testing.T) {
	s, system, user := running(t)
	if _, err := s.backend(registry.Network).Apply(user, network("back", "10.1.0.0/24")); err != nil {
		t.Fatal(err)
	}
	for _, fs := range []*storagev1alpha1.FileShare{fileShare("front", "1Gi"), fileShare("back", "1Gi", "back", "store")} {
		if _, err := s.backend(registry.FileShare).Apply(user, fs); err != nil {
			t.Fatal(err)
		}
		// A container's machine takes only a share on a node.
		bindShare(t, s, system, fs.GetMetadata().GetName(), "n1", "1Gi")
	}
	refused := func(err error, what string) {
		t.Helper()
		wantCodeOf(t, err, connect.CodeInvalidArgument, what)
		if err != nil && !strings.Contains(err.Error(), "is on network back, store") {
			t.Errorf("%s: %v does not name the share's networks", what, err)
		}
	}

	// A container on default reaches a share on default, which an empty
	// spec.networks is, and not one on back.
	c := ctr("api")
	c.Spec.Volumes = []*computev1alpha1.ContainerVolume{shareVol("front", "/front")}
	applyCtr(t, s, user, c)
	c.Spec.Volumes = append(c.Spec.Volumes, shareVol("back", "/back"))
	_, err := s.backend(registry.Container).Apply(user, c)
	refused(err, "a container declaring a share on none of its networks")
	c.Spec.Interfaces = []*computev1alpha1.NetworkInterface{{Network: "default"}, {Network: "back"}}
	c.Spec.Volumes = c.Spec.Volumes[1:]
	if _, err := s.backend(registry.Container).Apply(user, c); err != nil {
		t.Errorf("a container on one of the share's networks: %v", err)
	}
	// One not made yet is the machine's to wait for.
	c.Spec.Volumes = append(c.Spec.Volumes, shareVol("later", "/later"))
	if _, err := s.backend(registry.Container).Apply(user, c); err != nil {
		t.Errorf("a container declaring a share not made yet: %v", err)
	}

	vm := vmOfDisk("db", "db-root")
	vm.Spec.FileShares = []*computev1alpha1.VirtualMachineFileShare{{Name: "back"}}
	_, err = s.backend(registry.VirtualMachine).Apply(user, vm)
	refused(err, "a VM declaring a share on none of its networks")
	vm.Spec.Interfaces = []*computev1alpha1.NetworkInterface{{Network: "back"}}
	if _, err := s.backend(registry.VirtualMachine).Apply(user, vm); err != nil {
		t.Errorf("a VM on the share's network: %v", err)
	}
	vm.Spec.FileShares = append(vm.Spec.FileShares, &computev1alpha1.VirtualMachineFileShare{Name: "back", ReadOnly: true})
	_, err = s.backend(registry.VirtualMachine).Apply(user, vm)
	wantCodeOf(t, err, connect.CodeInvalidArgument, "a VM declaring a share twice")

	machines := s.backend(registry.Machine)
	_, err = machines.Apply(system, declaring(machine("ct-a", ""), "back"))
	refused(err, "a machine declaring a share on none of its networks")
	_, err = machines.Apply(system, declaring(machine("ct-a", ""), "later"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "a machine declaring a share not made yet")
	if _, err := machines.Apply(system, declaring(machineOn(machine("ct-a", ""), "default", "store"), "back", "front")); err != nil {
		t.Errorf("a machine on a network of each share: %v", err)
	}
	// A machine leaving the network it reaches a share on is refused too.
	_, err = machines.Apply(system, declaring(machineOn(machine("ct-a", ""), "default"), "back", "front"))
	refused(err, "a machine leaving its share's networks")
}

// TestAShareKeepsTheNetworksItIsMountedFrom: a network a declaring machine
// mounts a share from, the first of its own the share is on, cannot be
// taken from the share; any other can, and networks can be added.
func TestAShareKeepsTheNetworksItIsMountedFrom(t *testing.T) {
	s, system, user := running(t)
	shares, machines := s.backend(registry.FileShare), s.backend(registry.Machine)
	if _, err := shares.Apply(user, fileShare("data", "1Gi", "a", "b", "c")); err != nil {
		t.Fatal(err)
	}
	bindShare(t, s, system, "data", "n1", "1Gi")
	if _, err := machines.Apply(system, declaring(machineOn(machine("ct-x", ""), "default", "b", "a"), "data")); err != nil {
		t.Fatal(err)
	}
	_, err := shares.Apply(user, fileShare("data", "1Gi", "a", "c"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "removing the network a machine mounts from")
	if err != nil && !strings.Contains(err.Error(), `cannot leave network "b": it is mounted from there by machine ct-x`) {
		t.Errorf("the refusal says %v", err)
	}
	if _, err := shares.Apply(user, fileShare("data", "1Gi", "b", "c", "d")); err != nil {
		t.Errorf("removing a network no machine mounts from, adding one: %v", err)
	}
	// The default network, which an empty list is, is kept as any other.
	if _, err := shares.Apply(user, fileShare("dflt", "1Gi")); err != nil {
		t.Fatal(err)
	}
	bindShare(t, s, system, "dflt", "n1", "1Gi")
	if _, err := machines.Apply(system, declaring(machineOn(machine("ct-y", ""), "default"), "dflt")); err != nil {
		t.Fatal(err)
	}
	_, err = shares.Apply(user, fileShare("dflt", "1Gi", "b"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "removing the default network a machine mounts from")
	if _, err := shares.Apply(user, fileShare("dflt", "1Gi", "default", "b")); err != nil {
		t.Errorf("naming the default network it was on: %v", err)
	}
}

// TestANetworkWithAShareStays: a network a share is on cannot be deleted,
// and a share cannot join a network being deleted.
func TestANetworkWithAShareStays(t *testing.T) {
	s, _, user := running(t)
	networks, shares := s.backend(registry.Network), s.backend(registry.FileShare)
	for _, n := range []string{"back", "old"} {
		if _, err := networks.Apply(user, network(n, "10.1.0.0/24")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := shares.Apply(user, fileShare("data", "1Gi", "back")); err != nil {
		t.Fatal(err)
	}
	_, err := networks.Delete(user, resource.Key{Project: "web", Name: "back"}, nil)
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "deleting a network a share is on")
	if err != nil && !strings.Contains(err.Error(), "file share data") {
		t.Errorf("the refusal says %v", err)
	}
	_, err = networks.Delete(user, resource.Key{Project: "web", Name: DefaultNetwork}, nil)
	if err != nil {
		t.Errorf("deleting a network no share is on: %v", err)
	}

	// Without the controller, a deleted network stays marked.
	if _, err := networks.Delete(user, resource.Key{Project: "web", Name: "old"}, nil); err != nil {
		t.Fatal(err)
	}
	_, err = shares.Apply(user, fileShare("data", "1Gi", "back", "old"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "joining a network being deleted")
	if _, err := shares.Apply(user, fileShare("data", "1Gi", "back", "later")); err != nil {
		t.Errorf("joining a network not made yet: %v", err)
	}
	if _, err := shares.Apply(user, fileShare("data", "1Gi", "later")); err != nil {
		t.Fatal(err)
	}
	if _, err := networks.Delete(user, resource.Key{Project: "web", Name: "back"}, nil); err != nil {
		t.Errorf("deleting a network the share left: %v", err)
	}
}

// TestADeclaredShareCannotBeDeleted: a share a live machine declares stays;
// once no machine but one being deleted declares it, it can go.
func TestADeclaredShareCannotBeDeleted(t *testing.T) {
	s, system, user := running(t)
	shares, machines := s.backend(registry.FileShare), s.backend(registry.Machine)
	key := resource.Key{Project: "web", Name: "data"}
	if _, err := shares.Apply(user, fileShare("data", "1Gi")); err != nil {
		t.Fatal(err)
	}
	bindShare(t, s, system, "data", "n1", "1Gi")
	// Kept, once deleted, until its node releases it, as a controller's is.
	m := declaring(machine("ct-a", ""), "data")
	m.Metadata.Finalizers = []string{NodeCleanupFinalizer}
	if _, err := machines.Apply(system, m); err != nil {
		t.Fatal(err)
	}
	_, err := shares.Delete(user, key, nil)
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "deleting a declared share")
	if err != nil && !strings.Contains(err.Error(), "declared by machine ct-a") {
		t.Errorf("the refusal says %v", err)
	}
	if _, err := machines.Delete(system, resource.Key{Project: "web", Name: "ct-a"}, nil); err != nil {
		t.Fatal(err)
	}
	if m := mustGet[*computev1alpha1.Machine](t, s, system, registry.Machine, resource.Key{Project: "web", Name: "ct-a"}); m.GetMetadata().GetDeleteTime() == nil {
		t.Fatal("the machine is not being deleted")
	}
	if _, err := shares.Delete(user, key, nil); err != nil {
		t.Errorf("deleting a share only a machine being deleted declares: %v", err)
	}
	if getShare(t, s, system, "data") != nil {
		t.Error("a deleted share is still there")
	}
	// Gone, it holds back what declares it again.
	_, err = machines.Apply(system, declaring(machine("ct-b", ""), "data"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "declaring a deleted share")

	// A project being deleted lets its shares go, declared or not.
	if _, err := shares.Apply(user, fileShare("kept", "1Gi")); err != nil {
		t.Fatal(err)
	}
	bindShare(t, s, system, "kept", "n1", "1Gi")
	if _, err := machines.Apply(system, declaring(machine("ct-c", ""), "kept")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.Project).Delete(user, resource.Key{Name: "web"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := shares.Delete(user, resource.Key{Project: "web", Name: "kept"}, nil); err != nil {
		t.Errorf("deleting a declared share of a project being deleted: %v", err)
	}
}

// TestDeletingAProjectTakesItsShares: a project's shares go with it, even
// one a machine declares.
func TestDeletingAProjectTakesItsShares(t *testing.T) {
	s, system, user := controlled(t)
	if _, err := s.backend(registry.FileShare).Apply(user, fileShare("data", "1Gi")); err != nil {
		t.Fatal(err)
	}
	bindShare(t, s, system, "data", "n1", "1Gi")
	c := ctr("api")
	c.Spec.Volumes = []*computev1alpha1.ContainerVolume{shareVol("data", "/data")}
	applyCtr(t, s, user, c)
	waitMachine(t, s, system, machineName("api"), "declaring the share", func(m *computev1alpha1.Machine) bool {
		return declaresShare(m.GetSpec(), "data")
	})
	if _, err := s.backend(registry.Project).Delete(user, resource.Key{Name: "web"}, nil); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the share to go with its project", func() bool { return getShare(t, s, system, "data") == nil })
}

// firmwareMachine is a VM's machine on node, booting rootDisk.
func firmwareMachine(name, node, rootDisk string) *computev1alpha1.Machine {
	m := machine(name, node)
	m.Spec.RestartPolicy = computev1alpha1.RestartPolicy_RESTART_POLICY_ON_FAILURE
	m.Spec.Boot = &computev1alpha1.MachineSpec_Firmware{Firmware: &computev1alpha1.FirmwareBoot{RootDisk: rootDisk}}
	return m
}

// TestAContainerWaitsForItsShare: a container declaring a share that does
// not exist yet, or is on no node yet, is held back and says which; once
// the share is on a node, the machine declares it, never to fill it from
// the image, and goes to that node. A placed container's machine is held
// back for a share on no node, and refused one of another node; a placed
// VM's takes one on no node with the rest of its change.
func TestAContainerWaitsForItsShare(t *testing.T) {
	s, system, user := controlled(t, "n1", "n2")
	nodeSized(t, s, system, "n1", 1024, 100)
	nodeSized(t, s, system, "n2", 8192, 100)
	c := ctr("web")
	c.Spec.Volumes = []*computev1alpha1.ContainerVolume{
		{Source: "assets", SourceKind: "FileShare", Target: "/srv", ReadOnly: true, Volume: &computev1alpha1.ContainerVolumeOptions{Subpath: "site"}},
	}
	applyCtr(t, s, user, c)
	heldBack := func(says string) {
		t.Helper()
		got := waitContainer(t, s, system, "web", "held back: "+says, func(c *computev1alpha1.Container) bool {
			return containerReady(c).GetReason() == "MachineHeldBack" && strings.Contains(containerReady(c).GetMessage(), says)
		})
		if m, err := s.get(system, registry.Machine, resource.Key{Project: "web", Name: machineName("web")}); err != nil || m != nil {
			t.Errorf("a held back container has a machine: %v (%v)", m, err)
		}
		if ready := containerReady(got); ready.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE {
			t.Errorf("Ready is %v", ready)
		}
	}
	heldBack(`file share "assets" does not exist`)

	// Made, on no node, it holds the container back still.
	if _, err := s.backend(registry.FileShare).Apply(user, fileShare("assets", "1Gi")); err != nil {
		t.Fatal(err)
	}
	heldBack(`file share "assets" is on no node yet`)

	// On a node, the share takes the container there, though another node
	// has more room.
	bindShare(t, s, system, "assets", "n1", "1Gi")
	m := waitMachine(t, s, system, machineName("web"), "placed on the share's node", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetNode() == "n1"
	})
	wantShares := []*computev1alpha1.MachineFileShare{{FileShare: "assets", ReadOnly: true, Network: "default"}}
	wantMounts := []*computev1alpha1.ContainerBootVolume{{FileShare: "assets", Target: "/srv", ReadOnly: true, Nocopy: true, Subpath: "site"}}
	spec := m.GetSpec()
	if len(spec.GetDisks()) != 0 || len(spec.GetFileShares()) != 1 || !proto.Equal(spec.GetFileShares()[0], wantShares[0]) ||
		len(spec.GetContainer().GetVolumes()) != 1 || !proto.Equal(spec.GetContainer().GetVolumes()[0], wantMounts[0]) {
		t.Errorf("machine spec %v", spec)
	}

	// A placed machine is refused a share of another node. A container's
	// is held back for one on no node; a VM's takes it with the rest of its
	// change, such as a stop.
	for name, node := range map[string]string{"elsewhere": "n2", "nowhere": ""} {
		if _, err := s.backend(registry.FileShare).Apply(user, fileShare(name, "1Gi")); err != nil {
			t.Fatal(err)
		}
		if node != "" {
			bindShare(t, s, system, name, node, "1Gi")
		}
	}
	if _, err := s.backend(registry.Machine).Apply(system, machine("ct-p", "n1")); err != nil {
		t.Fatal(err)
	}
	_, err := s.backend(registry.Machine).Apply(system, declaring(machine("ct-p", "n1"), "elsewhere"))
	wantCodeOf(t, err, connect.CodeInvalidArgument, "a placed machine declaring a share of another node")
	_, err = s.backend(registry.Machine).Apply(system, declaring(machine("ct-p", "n1"), "nowhere"))
	wantCodeOf(t, err, connect.CodeFailedPrecondition, "a placed container's machine declaring a share on no node")
	if _, err := s.backend(registry.Disk).Apply(user, disk("p-root", "1Gi")); err != nil {
		t.Fatal(err)
	}
	bindDisk(t, s, system, "p-root", "n1", "1Gi")
	if _, err := s.backend(registry.Machine).Apply(system, firmwareMachine("vm-p", "n1", "p-root")); err != nil {
		t.Fatal(err)
	}
	stopping := declaring(firmwareMachine("vm-p", "n1", "p-root"), "nowhere", "elsewhere", "assets")
	stopping.Spec.RunState = computev1alpha1.MachineRunState_MACHINE_RUN_STATE_STOPPED
	if _, err := s.backend(registry.Machine).Apply(system, stopping); err != nil {
		t.Errorf("a placed VM's machine stopping and declaring shares on no node and another node: %v", err)
	}

	// Disks and shares on two nodes can never be had together.
	if _, err := s.backend(registry.Disk).Apply(user, disk("on-n1", "1Gi")); err != nil {
		t.Fatal(err)
	}
	bindDisk(t, s, system, "on-n1", "n1", "1Gi")
	if _, err := s.backend(registry.Machine).Apply(system, declaring(withDisks(machine("ct-q", ""), "on-n1"), "elsewhere")); err != nil {
		t.Fatal(err)
	}
	waitMachine(t, s, system, "ct-q", "refused", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetNode() == "" && strings.Contains(scheduled(m).GetMessage(), "its disks live on node n1 and its file shares on node n2")
	})
	// A VM's goes with its disks, its share of another node unmounted.
	if _, err := s.backend(registry.Disk).Apply(user, disk("q-root", "1Gi")); err != nil {
		t.Fatal(err)
	}
	bindDisk(t, s, system, "q-root", "n1", "1Gi")
	if _, err := s.backend(registry.Machine).Apply(system, declaring(firmwareMachine("vm-q", "", "q-root"), "elsewhere")); err != nil {
		t.Fatal(err)
	}
	waitMachine(t, s, system, "vm-q", "placed with its disk", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
}

// TestOnlyAVMsMachineIsPlacedWithAShareOnNoNode: the scheduler places a
// VM's machine declaring a share on no node, and not a container's, which
// admission holds back before it gets that far.
func TestOnlyAVMsMachineIsPlacedWithAShareOnNoNode(t *testing.T) {
	s, system, user := running(t)
	if _, err := s.backend(registry.FileShare).Apply(user, fileShare("data", "1Gi")); err != nil {
		t.Fatal(err)
	}
	node, why, err := s.machineShares(system, declaring(machine("ct-a", ""), "data"))
	if err != nil || node != "" || !strings.Contains(why, "file share data is on no node yet, and a container runs only with its file shares mounted") {
		t.Errorf("a container's machine: node %q, why %q (%v)", node, why, err)
	}
	node, why, err = s.machineShares(system, declaring(firmwareMachine("vm-a", "", "a-root"), "data"))
	if err != nil || node != "" || why != "" {
		t.Errorf("a VM's machine: node %q, why %q (%v)", node, why, err)
	}
	// Of shares on two nodes, a container's machine can have none, and a
	// VM's goes to the first's.
	for name, on := range map[string]string{"on-n1": "n1", "on-n2": "n2"} {
		if _, err := s.backend(registry.FileShare).Apply(user, fileShare(name, "1Gi")); err != nil {
			t.Fatal(err)
		}
		bindShare(t, s, system, name, on, "1Gi")
	}
	node, why, err = s.machineShares(system, declaring(machine("ct-b", ""), "on-n1", "on-n2"))
	if err != nil || node != "" || !strings.Contains(why, "file share on-n1 lives on node n1 and file share on-n2 on node n2") {
		t.Errorf("a container's machine with shares on two nodes: node %q, why %q (%v)", node, why, err)
	}
	node, why, err = s.machineShares(system, declaring(firmwareMachine("vm-b", "", "b-root"), "on-n1", "on-n2"))
	if err != nil || node != "n1" || why != "" {
		t.Errorf("a VM's machine with shares on two nodes: node %q, why %q (%v)", node, why, err)
	}
}

// TestAVirtualMachineDeclaresItsShares: a VM's spec.file_shares are its
// machine's, and changing them changes nothing else of the machine.
func TestAVirtualMachineDeclaresItsShares(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeSized(t, s, system, "n1", 8192, 100)
	if _, err := s.backend(registry.Image).Apply(system, image("noble")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"data", "logs"} {
		if _, err := s.backend(registry.FileShare).Apply(user, fileShare(name, "1Gi")); err != nil {
			t.Fatal(err)
		}
	}
	bindShare(t, s, system, "data", "n1", "1Gi")
	vm := vmFromImage("web", "noble")
	vm.Spec.FileShares = []*computev1alpha1.VirtualMachineFileShare{{Name: "data"}}
	if _, err := s.backend(registry.VirtualMachine).Apply(user, vm); err != nil {
		t.Fatal(err)
	}
	before := waitMachine(t, s, system, vmMachineName("web"), "placed, declaring data", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetNode() == "n1" && declaresShare(m.GetSpec(), "data")
	})

	vm.Spec.FileShares = []*computev1alpha1.VirtualMachineFileShare{{Name: "data", ReadOnly: true}, {Name: "logs"}}
	if _, err := s.backend(registry.VirtualMachine).Apply(user, vm); err != nil {
		t.Fatal(err)
	}
	after := waitMachine(t, s, system, vmMachineName("web"), "declaring both", func(m *computev1alpha1.Machine) bool {
		return declaresShare(m.GetSpec(), "logs")
	})
	want := []*computev1alpha1.MachineFileShare{{FileShare: "data", ReadOnly: true, Network: "default"}, {FileShare: "logs", Network: "default"}}
	if got := after.GetSpec().GetFileShares(); len(got) != 2 || !proto.Equal(got[0], want[0]) || !proto.Equal(got[1], want[1]) {
		t.Errorf("the machine declares %v, want %v", got, want)
	}
	was, now := proto.Clone(before.GetSpec()).(*computev1alpha1.MachineSpec), proto.Clone(after.GetSpec()).(*computev1alpha1.MachineSpec)
	was.FileShares, now.FileShares = nil, nil
	if !proto.Equal(was, now) {
		t.Errorf("changing the VM's shares changed more of its machine: %v, was %v", now, was)
	}

	// It says which of its shares is not on its node, until it is.
	sharesOnNode := func(vm *computev1alpha1.VirtualMachine) *metav1.Condition {
		return conditions.Find(vm.GetStatus().GetConditions(), FileSharesOnNode)
	}
	waitVM(t, s, system, "web", "saying logs is not on its node", func(vm *computev1alpha1.VirtualMachine) bool {
		c := sharesOnNode(vm)
		return c.GetReason() == "FileShareNotOnNode" && strings.Contains(c.GetMessage(), "file share logs is not on node n1")
	})
	// Settled first, so only the share's change can tell the VM.
	waitSettled(t, s)
	bindShare(t, s, system, "logs", "n1", "1Gi")
	got := waitVM(t, s, system, "web", "saying its shares are on its node", func(vm *computev1alpha1.VirtualMachine) bool {
		return sharesOnNode(vm).GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_TRUE
	})
	if c := sharesOnNode(got); c.GetReason() != "OnNode" || c.GetObservedGeneration() != got.GetMetadata().GetGeneration() {
		t.Errorf("%s says %v", FileSharesOnNode, c)
	}

	// A share of another node is taken, and said to be off its node.
	if _, err := s.backend(registry.FileShare).Apply(user, fileShare("far", "1Gi")); err != nil {
		t.Fatal(err)
	}
	bindShare(t, s, system, "far", "n2", "1Gi")
	vm.Spec.FileShares = append(vm.Spec.FileShares, &computev1alpha1.VirtualMachineFileShare{Name: "far"})
	vm.Spec.RunState = computev1alpha1.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_STOPPED
	if _, err := s.backend(registry.VirtualMachine).Apply(user, vm); err != nil {
		t.Fatal(err)
	}
	waitMachine(t, s, system, vmMachineName("web"), "stopped, declaring far", func(m *computev1alpha1.Machine) bool {
		return declaresShare(m.GetSpec(), "far") && m.GetSpec().GetRunState() == computev1alpha1.MachineRunState_MACHINE_RUN_STATE_STOPPED
	})
	waitVM(t, s, system, "web", "saying far is not on its node", func(vm *computev1alpha1.VirtualMachine) bool {
		c := sharesOnNode(vm)
		return c.GetReason() == "FileShareNotOnNode" && strings.Contains(c.GetMessage(), "file share far is not on node n1")
	})

	// One whose machine is on no node says so.
	big := vmFromImage("big", "noble")
	big.Spec.Resources = &computev1alpha1.VirtualMachineResources{Memory: "64Gi"}
	big.Spec.FileShares = []*computev1alpha1.VirtualMachineFileShare{{Name: "data"}}
	if _, err := s.backend(registry.VirtualMachine).Apply(user, big); err != nil {
		t.Fatal(err)
	}
	waitVM(t, s, system, "big", "saying its machine is on no node", func(vm *computev1alpha1.VirtualMachine) bool {
		c := sharesOnNode(vm)
		return c.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_FALSE && c.GetReason() == "MachineNotPlaced"
	})

	// Declaring none, a VM has no such condition.
	plain := vmFromImage("web", "noble")
	if _, err := s.backend(registry.VirtualMachine).Apply(user, plain); err != nil {
		t.Fatal(err)
	}
	waitVM(t, s, system, "web", "without the condition, declaring nothing", func(vm *computev1alpha1.VirtualMachine) bool {
		return sharesOnNode(vm) == nil && vmReady(vm).GetObservedGeneration() == vm.GetMetadata().GetGeneration()
	})
}

// TestFileSharesAreCounted: a share counts toward file_shares and its size
// toward disk; its server counts toward nothing.
func TestFileSharesAreCounted(t *testing.T) {
	s, system, user := running(t)
	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{Disk: proto.String("30Gi"), FileShares: proto.Int32(2), Machines: proto.Int32(1)})
	b := s.backend(registry.FileShare)
	if _, err := b.Apply(user, fileShare("a", "10Gi")); err != nil {
		t.Fatal(err)
	}
	_, err := b.Apply(user, fileShare("big", "25Gi"))
	wantExhausted(t, err, "disk limit is 30Gi", "a share past the disk limit")
	if _, err := b.Apply(user, fileShare("b", "10Gi")); err != nil {
		t.Fatal(err)
	}
	_, err = b.Apply(user, fileShare("c", "1Gi"))
	wantExhausted(t, err, "file_shares limit is 2", "a third share")
	_, err = b.Apply(user, fileShare("b", "21Gi"))
	wantExhausted(t, err, "this would bring it to 31Gi", "growing a share past the disk limit")
	// A share made bigger than it asks counts what it has.
	bindShare(t, s, system, "b", "n1", "15Gi")
	_, err = b.Apply(user, fileShare("a", "16Gi"))
	wantExhausted(t, err, "it uses 25Gi", "a share's made size counted")

	// The usage says so, and a container declaring a share is one machine.
	c := ctr("api")
	c.Spec.Resources = &computev1alpha1.Resources{Disk: "1Gi"}
	c.Spec.Volumes = []*computev1alpha1.ContainerVolume{shareVol("a", "/a")}
	applyCtr(t, s, user, c)
	tl, err := s.tally(system, "acme", nil)
	if err != nil {
		t.Fatal(err)
	}
	if u := tl.tenant.usage(); u.GetFileShares() != 2 || u.GetDisk() != "26Gi" || u.GetMachines() != 1 {
		t.Errorf("usage %v, want 2 file shares, 26Gi of disk and 1 machine", u)
	}
	if ls, err := parseLimits(&tenancyv1alpha1.Limits{FileShares: proto.Int32(3)}); err != nil || !proto.Equal(ls.proto(), &tenancyv1alpha1.Limits{FileShares: proto.Int32(3)}) {
		t.Errorf("file_shares 3 read back as %v (%v)", ls.proto(), err)
	}
	_, err = parseLimits(&tenancyv1alpha1.Limits{FileShares: proto.Int32(-1)})
	if err == nil {
		t.Error("a file_shares limit below zero was taken")
	}
}

// TestConcurrentSharesCannotOvershootTheirLimit races share creates against
// the file_shares limit: exactly as many win as it allows.
func TestConcurrentSharesCannotOvershootTheirLimit(t *testing.T) {
	s, _, user := running(t)
	setDefaultPlan(t, s, &tenancyv1alpha1.Limits{FileShares: proto.Int32(3)})
	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.backend(registry.FileShare).Apply(user, fileShare(fmt.Sprintf("s%d", i), "1Gi"))
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case connect.CodeOf(err) != connect.CodeResourceExhausted:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if won != 3 {
		t.Errorf("%d of 10 concurrent creates won against a limit of 3", won)
	}
}

// TestLeavingANetworkAndMountingFromItAreOneDecision races a share leaving a
// network with a machine coming to mount it from there, and a network
// being deleted with a share joining it: in each, exactly one wins.
func TestLeavingANetworkAndMountingFromItAreOneDecision(t *testing.T) {
	for i := range 20 {
		s, system, user := running(t)
		shares := s.backend(registry.FileShare)
		if _, err := s.backend(registry.Network).Apply(user, network("back", "10.1.0.0/24")); err != nil {
			t.Fatal(err)
		}
		if _, err := shares.Apply(user, fileShare("data", "1Gi", "default", "back")); err != nil {
			t.Fatal(err)
		}
		bindShare(t, s, system, "data", "n1", "1Gi")
		if _, err := s.backend(registry.Network).Apply(user, network("old", "10.2.0.0/24")); err != nil {
			t.Fatal(err)
		}
		if _, err := shares.Apply(user, fileShare("other", "1Gi")); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var leave, mount, del, join error
		wg.Add(4)
		go func() {
			defer wg.Done()
			_, leave = shares.Apply(user, fileShare("data", "1Gi", "default"))
		}()
		go func() {
			defer wg.Done()
			_, mount = s.backend(registry.Machine).Apply(system, declaring(machineOn(machine("ct-a", ""), "back"), "data"))
		}()
		go func() {
			defer wg.Done()
			_, del = s.backend(registry.Network).Delete(user, resource.Key{Project: "web", Name: "old"}, nil)
		}()
		go func() {
			defer wg.Done()
			_, join = shares.Apply(user, fileShare("other", "1Gi", "default", "old"))
		}()
		wg.Wait()
		if (leave == nil) == (mount == nil) {
			t.Errorf("run %d: the share leaving back: %v; a machine mounting from back: %v", i, leave, mount)
		}
		if (del == nil) == (join == nil) {
			t.Errorf("run %d: deleting network old: %v; the share joining it: %v", i, del, join)
		}
	}
}

// TestAShareIsPendingFromItsCreation: the apiserver stores a new share
// Pending, whatever status its creator sent, and an apply keeps it.
func TestAShareIsPendingFromItsCreation(t *testing.T) {
	s, system, user := running(t)
	fs := fileShare("data", "1Gi")
	fs.Status = &storagev1alpha1.FileShareStatus{Phase: storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_IN_USE, Node: "n9"}
	if _, err := s.backend(registry.FileShare).Apply(user, fs); err != nil {
		t.Fatal(err)
	}
	want := &storagev1alpha1.FileShareStatus{Phase: storagev1alpha1.FileSharePhase_FILE_SHARE_PHASE_PENDING}
	if got := getShare(t, s, system, "data").GetStatus(); !proto.Equal(got, want) {
		t.Errorf("a new share's status is %v, want %v", got, want)
	}
	if _, err := s.backend(registry.FileShare).Apply(user, fileShare("data", "2Gi")); err != nil {
		t.Fatal(err)
	}
	if got := getShare(t, s, system, "data").GetStatus(); !proto.Equal(got, want) {
		t.Errorf("an applied share's status is %v, want %v", got, want)
	}
}

// TestContainersAndVMsDeclareBeforeTheirMachines: a container or a VM
// declares a share before it has a machine, as while it is held back: the
// share cannot be deleted from under it, nor leave the network it mounts
// from, nor be made where it cannot be reached from it.
func TestContainersAndVMsDeclareBeforeTheirMachines(t *testing.T) {
	s, _, user := running(t)
	shares := s.backend(registry.FileShare)
	key := resource.Key{Project: "web", Name: "data"}
	if _, err := s.backend(registry.Network).Apply(user, network("back", "10.1.0.0/24")); err != nil {
		t.Fatal(err)
	}
	if _, err := shares.Apply(user, fileShare("data", "1Gi", "default", "back")); err != nil {
		t.Fatal(err)
	}
	c := ctr("api")
	c.Spec.Volumes = []*computev1alpha1.ContainerVolume{shareVol("data", "/data")}
	applyCtr(t, s, user, c)
	vm := vmOfDisk("db", "db-root")
	vm.Spec.Interfaces = []*computev1alpha1.NetworkInterface{{Network: "back"}}
	vm.Spec.FileShares = []*computev1alpha1.VirtualMachineFileShare{{Name: "data"}}
	if _, err := s.backend(registry.VirtualMachine).Apply(user, vm); err != nil {
		t.Fatal(err)
	}
	refused := func(err error, what, says string) {
		t.Helper()
		wantCodeOf(t, err, connect.CodeFailedPrecondition, what)
		if err != nil && !strings.Contains(err.Error(), says) {
			t.Errorf("%s: %v does not say %q", what, err, says)
		}
	}

	_, err := shares.Delete(user, key, nil)
	refused(err, "deleting a share a container and a VM declare", "declared by container api, virtual machine db")
	_, err = shares.Apply(user, fileShare("data", "1Gi", "back"))
	refused(err, "leaving the network a container mounts from", `cannot leave network "default": it is mounted from there by container api`)
	_, err = shares.Apply(user, fileShare("data", "1Gi", "default"))
	refused(err, "leaving the network a VM mounts from", `cannot leave network "back": it is mounted from there by virtual machine db`)

	// A share made after what declares it must be reachable from it.
	late := ctr("late")
	late.Spec.Volumes = []*computev1alpha1.ContainerVolume{shareVol("later", "/later")}
	applyCtr(t, s, user, late)
	_, err = shares.Apply(user, fileShare("later", "1Gi", "back"))
	refused(err, "making a share none of its declarers reach", "could not be reached by container late (on network default)")
	if _, err := shares.Apply(user, fileShare("later", "1Gi")); err != nil {
		t.Errorf("making a share its declarer reaches: %v", err)
	}

	// Declared by none, the share goes.
	applyCtr(t, s, user, ctr("api"))
	vm.Spec.FileShares = nil
	if _, err := s.backend(registry.VirtualMachine).Apply(user, vm); err != nil {
		t.Fatal(err)
	}
	if _, err := shares.Delete(user, key, nil); err != nil {
		t.Errorf("deleting a share no one declares: %v", err)
	}
}

// TestDeclaringAndMovingAShareAreOneDecision races containers and VMs
// declaring a share with the share leaving their network: either the share
// leaves and none of them declares it, or one declares it and the share
// stays.
func TestDeclaringAndMovingAShareAreOneDecision(t *testing.T) {
	const declarers = 6
	for i := range 20 {
		s, _, user := running(t)
		shares := s.backend(registry.FileShare)
		if _, err := s.backend(registry.Network).Apply(user, network("back", "10.1.0.0/24")); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"for-cts", "for-vms"} {
			if _, err := shares.Apply(user, fileShare(name, "1Gi", "default", "back")); err != nil {
				t.Fatal(err)
			}
		}
		var wg sync.WaitGroup
		var moveC, moveVM error
		declareC, declareVM := make([]error, declarers), make([]error, declarers)
		for j := range declarers {
			wg.Add(2)
			go func() {
				defer wg.Done()
				c := ctr(fmt.Sprintf("c%d", j))
				c.Spec.Volumes = []*computev1alpha1.ContainerVolume{shareVol("for-cts", "/data")}
				_, declareC[j] = s.backend(registry.Container).Apply(user, c)
			}()
			go func() {
				defer wg.Done()
				vm := vmOfDisk(fmt.Sprintf("v%d", j), fmt.Sprintf("v%d-root", j))
				vm.Spec.FileShares = []*computev1alpha1.VirtualMachineFileShare{{Name: "for-vms"}}
				_, declareVM[j] = s.backend(registry.VirtualMachine).Apply(user, vm)
			}()
		}
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, moveC = shares.Apply(user, fileShare("for-cts", "1Gi", "back"))
		}()
		go func() {
			defer wg.Done()
			_, moveVM = shares.Apply(user, fileShare("for-vms", "1Gi", "back"))
		}()
		wg.Wait()
		for what, r := range map[string]struct {
			move     error
			declares []error
		}{"containers": {moveC, declareC}, "VMs": {moveVM, declareVM}} {
			won := 0
			for _, err := range r.declares {
				if err == nil {
					won++
				}
			}
			if (r.move == nil) == (won > 0) {
				t.Errorf("run %d: %d %s declared the share, and it leaving their network: %v", i, won, what, r.move)
			}
		}
	}
}

// TestSigningUpWaitsForNoShare races people signing up, whose transaction
// writes their project's network, with FileShare and Network applies, which
// hold the lock that write takes: all of them finish.
func TestSigningUpWaitsForNoShare(t *testing.T) {
	s, _, user := running(t)
	const n = 30
	for i := range n {
		invite(t, s, fmt.Sprintf("p%d@example.com", i), "platform.member")
	}
	var wg sync.WaitGroup
	errs := make([]error, 3*n)
	for i := range n {
		wg.Add(3)
		go func() {
			defer wg.Done()
			email := fmt.Sprintf("p%d@example.com", i)
			ctx := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.User, ID: email})
			_, errs[3*i] = (account{s}).SignUp(ctx, connect.NewRequest(&iamv1alpha1.SignUpRequest{Username: usernameOf(email)}))
		}()
		go func() {
			defer wg.Done()
			_, errs[3*i+1] = s.backend(registry.FileShare).Apply(user, fileShare(fmt.Sprintf("s%d", i), "1Gi"))
		}()
		go func() {
			defer wg.Done()
			_, errs[3*i+2] = s.backend(registry.Network).Apply(user, network(fmt.Sprintf("n%d", i), "10.1.0.0/24"))
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	testutil.Eventually(t, "signing up and applying shares and networks to finish", func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})
	for _, err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

// TestAMachineKeepsTheNetworkItMountsFrom: the network a machine mounts a
// share from is chosen once and kept, so the share cannot leave it, however
// its networks are reordered; a container whose machine declares the share
// is held to the machine's network, not to the one it would choose now.
func TestAMachineKeepsTheNetworkItMountsFrom(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeSized(t, s, system, "n1", 8192, 100)
	shares := s.backend(registry.FileShare)
	if _, err := s.backend(registry.Network).Apply(user, network("back", "10.1.0.0/24")); err != nil {
		t.Fatal(err)
	}
	if _, err := shares.Apply(user, fileShare("data", "1Gi", "back")); err != nil {
		t.Fatal(err)
	}
	bindShare(t, s, system, "data", "n1", "1Gi")
	c := ctr("api")
	c.Spec.Interfaces = []*computev1alpha1.NetworkInterface{{Network: "default"}, {Network: "back"}}
	c.Spec.Volumes = []*computev1alpha1.ContainerVolume{shareVol("data", "/data")}
	applyCtr(t, s, user, c)
	waitMachine(t, s, system, machineName("api"), "placed, mounting data from back", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetNode() == "n1" && declaredShare(m.GetSpec(), "data").GetNetwork() == "back"
	})

	leave := func(what string, networks ...string) {
		t.Helper()
		_, err := shares.Apply(user, fileShare("data", "1Gi", networks...))
		wantCodeOf(t, err, connect.CodeFailedPrecondition, what)
		if err != nil && !strings.Contains(err.Error(), `cannot leave network "back": it is mounted from there by machine ct-api`) {
			t.Errorf("%s: the refusal says %v", what, err)
		}
	}
	leave("leaving back", "default")
	if _, err := shares.Apply(user, fileShare("data", "1Gi", "back", "default")); err != nil {
		t.Fatal(err)
	}
	leave("leaving back once default is added", "default")
	// The container would mount from default now; its machine does not.
	if _, err := shares.Apply(user, fileShare("data", "1Gi", "back")); err != nil {
		t.Errorf("leaving default, which no machine mounts from: %v", err)
	}

	// Its container changing keeps the machine's network, and settles.
	if _, err := shares.Apply(user, fileShare("data", "1Gi", "back", "default")); err != nil {
		t.Fatal(err)
	}
	changed := ctr("api")
	changed.Spec.Interfaces = c.Spec.Interfaces
	changed.Spec.Volumes = c.Spec.Volumes
	changed.Spec.Env = []*computev1alpha1.EnvVar{{Name: "MODE", Value: "b"}}
	applyCtr(t, s, user, changed)
	m := waitMachine(t, s, system, machineName("api"), "changed", func(m *computev1alpha1.Machine) bool {
		return len(m.GetSpec().GetContainer().GetEnv()) == 1
	})
	if n := declaredShare(m.GetSpec(), "data").GetNetwork(); n != "back" {
		t.Errorf("the machine mounts data from %q, want back", n)
	}
	waitSettled(t, s)
	settled := getMachine(t, s, system, resource.Key{Project: "web", Name: machineName("api")}).GetMetadata().GetResourceVersion()
	waitSettled(t, s)
	if now := getMachine(t, s, system, resource.Key{Project: "web", Name: machineName("api")}).GetMetadata().GetResourceVersion(); now != settled {
		t.Errorf("a settled machine was written again: at %d, then at %d", settled, now)
	}
}

// TestTheNetworkAShareIsMountedFromIsSettledOnce: admission gives a
// machine's share the first of its networks the share is on, keeps it while
// the machine is on it, whatever a writer leaves out or asks for, and
// chooses again once the machine leaves it; a network a writer names must
// be the machine's and the share's.
func TestTheNetworkAShareIsMountedFromIsSettledOnce(t *testing.T) {
	s, system, user := running(t)
	machines := s.backend(registry.Machine)
	if _, err := s.backend(registry.FileShare).Apply(user, fileShare("data", "1Gi", "a", "b")); err != nil {
		t.Fatal(err)
	}
	bindShare(t, s, system, "data", "n1", "1Gi")
	mountsFrom := func(name string) string {
		return declaredShare(getMachine(t, s, system, resource.Key{Project: "web", Name: name}).GetSpec(), "data").GetNetwork()
	}
	if _, err := machines.Apply(system, declaring(machineOn(machine("ct-x", ""), "default", "b", "a"), "data")); err != nil {
		t.Fatal(err)
	}
	if n := mountsFrom("ct-x"); n != "b" {
		t.Errorf("ct-x mounts from %q, want b, the first of its networks the share is on", n)
	}
	asking := func(m *computev1alpha1.Machine, network string) *computev1alpha1.Machine {
		m.Spec.FileShares[0].Network = network
		return m
	}
	_, err := machines.Apply(system, asking(declaring(machineOn(machine("ct-x", ""), "default", "b", "a"), "data"), "a"))
	wantCodeOf(t, err, connect.CodeInvalidArgument, "moving a share to another network the machine is on")
	if _, err := machines.Apply(system, declaring(machineOn(machine("ct-x", ""), "a", "b"), "data")); err != nil {
		t.Fatal(err)
	}
	if n := mountsFrom("ct-x"); n != "b" {
		t.Errorf("reordered, ct-x mounts from %q, want b still", n)
	}
	if _, err := machines.Apply(system, declaring(machineOn(machine("ct-x", ""), "a"), "data")); err != nil {
		t.Fatal(err)
	}
	if n := mountsFrom("ct-x"); n != "a" {
		t.Errorf("off b, ct-x mounts from %q, want a", n)
	}

	_, err = machines.Apply(system, asking(declaring(machineOn(machine("ct-y", ""), "default", "a"), "data"), "b"))
	wantCodeOf(t, err, connect.CodeInvalidArgument, "naming a network of the share's the machine is not on")
	_, err = machines.Apply(system, asking(declaring(machineOn(machine("ct-y", ""), "default", "a"), "data"), "default"))
	wantCodeOf(t, err, connect.CodeInvalidArgument, "naming a network the share is not on")
	if _, err := machines.Apply(system, asking(declaring(machineOn(machine("ct-y", ""), "default", "b", "a"), "data"), "a")); err != nil {
		t.Errorf("naming a network of both: %v", err)
	}
	if n := mountsFrom("ct-y"); n != "a" {
		t.Errorf("ct-y mounts from %q, want a, as named", n)
	}
}
