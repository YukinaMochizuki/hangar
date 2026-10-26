package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/miekg/dns"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// aliased is a container named name on network, holding aliases there.
func aliased(name, network string, aliases ...string) *computev1alpha1.Container {
	c := container(name, "nginx")
	c.Spec.Interfaces = []*computev1alpha1.NetworkInterface{{Network: network, Aliases: aliases}}
	return c
}

// refused applies c and fails the test unless admission refuses it as a
// name taken, saying holder.
func (w *world) refused(c *computev1alpha1.Container, holder string) {
	w.t.Helper()
	_, err := w.root.Apply(context.Background(), registry.Container, c)
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), holder) {
		w.t.Errorf("container %s: %v, want it refused naming %s", c.GetMetadata().GetName(), err, holder)
	}
}

// TestAContainerIsResolvedByItsAliases: a network's resolver answers a
// container's aliases on it with its address there, another network's does
// not, an alias names one machine of a network, and changing aliases
// restarts nothing.
func TestAContainerIsResolvedByItsAliases(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	if _, err := w.root.Apply(context.Background(), registry.Network, &networkv1alpha1.Network{
		Metadata: &metav1.ObjectMeta{Project: project, Name: "other"},
		Spec:     &networkv1alpha1.NetworkSpec{Ipv4Cidr: "10.1.0.0/24"},
	}); err != nil {
		t.Fatal(err)
	}
	w.apply(aliased("t-1", "default", "db", "postgres"))
	w.apply(aliased("t-2", "other", "db"))
	w.apply(container("web", "nginx"))
	w.waitReason("t-1", "Running")
	w.waitReason("t-2", "Running")
	w.waitReason("web", "Running")
	one, two := addrOf(w.machine("t-1")), addrOf(w.machine("t-2"))
	zone := project + ".hangar.internal"
	w.answers("default", "10.0.0.1", "db."+zone, dns.RcodeSuccess, one)
	w.answers("default", "10.0.0.1", "postgres."+zone, dns.RcodeSuccess, one)
	w.answers("default", "10.0.0.1", "t-1."+zone, dns.RcodeSuccess, one)
	w.answers("other", "10.1.0.1", "db."+zone, dns.RcodeSuccess, two)
	w.answers("other", "10.1.0.1", "postgres."+zone, dns.RcodeNameError)

	// On one network, an alias is another's neither as an alias nor as a
	// name, and a name is no other's alias.
	w.refused(aliased("t-3", "default", "db"), "container t-1")
	w.refused(aliased("t-3", "default", "web"), "container web")
	w.refused(aliased("db", "default"), "container t-1")
	// t-2 holds no name on default.
	if _, err := w.root.Apply(context.Background(), registry.Container, aliased("t-3", "default", "t-2")); err != nil {
		t.Errorf("an alias that is the name of a machine on another network: %v", err)
	}
	// Its own name is no other's.
	if _, err := w.root.Apply(context.Background(), registry.Container, aliased("t-1", "default", "db", "postgres", "t-1")); err != nil {
		t.Errorf("a container applied again with its own name as an alias: %v", err)
	}
	if _, ok := get[*computev1alpha1.Container](w, registry.Container, "t-3"); !ok {
		t.Fatal("t-3 was not made")
	}

	// Changing aliases moves the names and restarts nothing.
	uid := w.machine("t-1").GetMetadata().GetUid()
	started := w.started(uid)
	w.apply(aliased("t-1", "default", "cache"))
	w.answers("default", "10.0.0.1", "cache."+zone, dns.RcodeSuccess, one)
	w.answers("default", "10.0.0.1", "db."+zone, dns.RcodeNameError)
	w.waitReason("t-1", "Running")
	w.checkNotRestarted(started)
	// The alias let go is free again.
	if _, err := w.root.Apply(context.Background(), registry.Container, aliased("t-3", "default", "db")); err != nil {
		t.Errorf("an alias let go: %v", err)
	}
	w.answers("default", "10.0.0.1", "db."+zone, dns.RcodeSuccess, addrOf(w.machine("t-3")))

	for _, name := range []string{"t-1", "t-2", "t-3", "web"} {
		w.delete(name)
	}
	for _, name := range []string{"t-1", "t-2", "t-3", "web"} {
		w.waitGone(name)
	}
	w.waitHostEmpty()
}

// TestAVMsAliasesAreAMachinesNames: a VM's aliases are checked against
// containers' names and aliases, and theirs against it, and are answered
// as a container's.
func TestAVMsAliasesAreAMachinesNames(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.host.AddDiskImage(cloudURL, "sha256:"+cloudSHA, 3*gib)
	w.applyImage("noble", cloudSHA)
	w.apply(aliased("api", "default", "cache"))
	vm := func(name string, aliases ...string) error {
		_, err := w.root.Apply(context.Background(), registry.VirtualMachine, &computev1alpha1.VirtualMachine{
			Metadata: &metav1.ObjectMeta{Project: project, Name: name},
			Spec: &computev1alpha1.VirtualMachineSpec{
				RootDisk: &computev1alpha1.VirtualMachineRootDisk{Source: &computev1alpha1.VirtualMachineRootDisk_FromImage{
					FromImage: &computev1alpha1.VirtualMachineImageDisk{Image: &storagev1alpha1.ImageSource{Name: "noble"}, Size: "10Gi"},
				}},
				Interfaces: []*computev1alpha1.NetworkInterface{{Network: "default", Aliases: aliases}},
			},
		})
		return err
	}
	for _, alias := range []string{"cache", "api"} {
		if err := vm("db", alias); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "container api") {
			t.Errorf("a VM aliased %s: %v, want it refused naming container api", alias, err)
		}
	}
	if err := vm("db", "postgres"); err != nil {
		t.Fatal(err)
	}
	w.refused(aliased("web", "default", "postgres"), "virtual machine db")
	w.refused(aliased("postgres", "default"), "virtual machine db")
	w.waitVMReason("db", "Running")
	zone := project + ".hangar.internal"
	w.answers("default", "10.0.0.1", "postgres."+zone, dns.RcodeSuccess, addrOf(w.machine2(vmMachineOf("db"))))
	testutil.Eventually(w.t, "the VM's machine carrying its aliases", func() (bool, string) {
		got := w.machine2(vmMachineOf("db")).GetSpec().GetInterfaces()[0].GetAliases()
		return fmt.Sprint(got) == "[postgres]", fmt.Sprint(got)
	})

	// Changing them moves the names and restarts nothing.
	started := w.started(w.machine2(vmMachineOf("db")).GetMetadata().GetUid())
	if err := vm("db", "pg"); err != nil {
		t.Fatal(err)
	}
	w.answers("default", "10.0.0.1", "pg."+zone, dns.RcodeSuccess, addrOf(w.machine2(vmMachineOf("db"))))
	w.answers("default", "10.0.0.1", "postgres."+zone, dns.RcodeNameError)
	w.waitVMReason("db", "Running")
	w.checkNotRestarted(started)
}
