package e2e

import (
	"cmp"
	"context"
	"fmt"
	"net/netip"
	"slices"
	"testing"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

var sharedIPv4 = netip.MustParseAddr("198.51.100.3")

// applyForward forwards a public port of protocol to container ctr's eth0,
// port port, labelled with labels.
func (w *world) applyForward(name, ctr string, protocol networkv1alpha1.Protocol, port int32, labels map[string]string) {
	w.t.Helper()
	if _, err := w.root.Apply(context.Background(), registry.PortForward, &networkv1alpha1.PortForward{
		Metadata: &metav1.ObjectMeta{Project: project, Name: name, Labels: labels},
		Spec: &networkv1alpha1.PortForwardSpec{
			Target:   &networkv1alpha1.PortForwardTarget{Object: &networkv1alpha1.PortForwardTarget_Container{Container: ctr}},
			Protocol: protocol, TargetPort: port,
		},
	}); err != nil {
		w.t.Fatal(err)
	}
}

// waitForward waits until PortForward name's Ready condition has reason
// and returns it.
func (w *world) waitForward(name, reason string) *networkv1alpha1.PortForward {
	w.t.Helper()
	var pf *networkv1alpha1.PortForward
	testutil.Eventually(w.t, "port forward "+name+" "+reason, func() (bool, string) {
		pf, _ = get[*networkv1alpha1.PortForward](w, registry.PortForward, name)
		return conditions.Find(pf.GetStatus().GetConditions(), "Ready").GetReason() == reason, fmt.Sprint(pf.GetStatus())
	})
	return pf
}

// waitForwards waits until the host forwards exactly want.
func (w *world) waitForwards(what string, want ...driver.Forward) {
	w.t.Helper()
	slices.SortFunc(want, func(a, b driver.Forward) int {
		return cmp.Or(cmp.Compare(a.Protocol, b.Protocol), cmp.Compare(a.Port, b.Port))
	})
	testutil.Eventually(w.t, what, func() (bool, string) {
		got := w.host.Forwards()
		return slices.Equal(got, want), fmt.Sprint(got)
	})
}

// ipv4Of is the address of a container's machine's eth0.
func (w *world) ipv4Of(ctr string) netip.Addr {
	w.t.Helper()
	p, err := netip.ParsePrefix(w.machine(ctr).GetSpec().GetInterfaces()[0].GetIpv4())
	if err != nil {
		w.t.Fatal(err)
	}
	return p.Addr()
}

// TestPortForwardsReachTheHost: a PortForward's port at the host's shared
// address leads to its container's interface on the host, follows it to
// another, and closes once deleted, which then lets the PortForward go; a
// set ensured again as it is is not written again.
func TestPortForwardsReachTheHost(t *testing.T) {
	w := newIdleWorld(t)
	w.sharedIPv4 = sharedIPv4
	w.startAgent()
	w.apply(container("api", "nginx"))
	w.apply(container("dns", "nginx"))
	w.waitReason("api", "Running")
	w.waitReason("dns", "Running")
	netUID := w.networkUID("default")

	w.applyForward("web", "api", networkv1alpha1.Protocol_PROTOCOL_TCP, 80, nil)
	w.applyForward("udp", "dns", networkv1alpha1.Protocol_PROTOCOL_UDP, 53, nil)
	web := w.waitForward("web", "Forwarding")
	udp := w.waitForward("udp", "Forwarding")
	if web.GetStatus().GetIpv4() != sharedIPv4.String() || web.GetStatus().GetNode() == "" {
		t.Errorf("the forward is at %q on node %q, want %s", web.GetStatus().GetIpv4(), web.GetStatus().GetNode(), sharedIPv4)
	}
	tcpTo := func(ctr string) driver.Forward {
		return driver.Forward{Protocol: driver.TCP, Port: uint16(web.GetStatus().GetPort()), NetworkUID: netUID, To: netip.AddrPortFrom(w.ipv4Of(ctr), 80)}
	}
	udpTo := driver.Forward{Protocol: driver.UDP, Port: uint16(udp.GetStatus().GetPort()), NetworkUID: netUID, To: netip.AddrPortFrom(w.ipv4Of("dns"), 53)}
	w.waitForwards("both ports forwarded", tcpTo("api"), udpTo)

	// Applied again unchanged but for a label, nothing is written.
	w.waitSettled()
	writes := w.host.ForwardWrites()
	w.applyForward("web", "api", networkv1alpha1.Protocol_PROTOCOL_TCP, 80, map[string]string{"tier": "front"})
	w.waitSettled()
	if got := w.host.ForwardWrites(); got != writes {
		t.Errorf("the same forwards were written %d more times", got-writes)
	}

	// Moved to dns, the port leads there.
	w.applyForward("web", "dns", networkv1alpha1.Protocol_PROTOCOL_TCP, 80, nil)
	w.waitForwards("the TCP port moved to dns", tcpTo("dns"), udpTo)

	if _, err := w.root.Delete(context.Background(), registry.PortForward, client.Key{Project: project, Name: "web"}); err != nil {
		t.Fatal(err)
	}
	w.waitForwards("the TCP port closed", udpTo)
	testutil.Eventually(w.t, "the deleted forward gone", func() (bool, string) {
		pf, ok := get[*networkv1alpha1.PortForward](w, registry.PortForward, "web")
		return !ok, fmt.Sprint(pf.GetStatus())
	})

	// Its target deleted, the forward waits and its port closes.
	w.delete("dns")
	w.waitGone("dns")
	w.waitForwards("the UDP port closed with its target")
	w.waitForward("udp", "TargetNotFound")
}

// TestWithoutASharedAddressNothingIsForwarded: a host without a shared
// address reports none, its forwards wait for one, and one deleted still
// goes.
func TestWithoutASharedAddressNothingIsForwarded(t *testing.T) {
	w := newWorld(t)
	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")
	w.applyForward("web", "api", networkv1alpha1.Protocol_PROTOCOL_TCP, 80, nil)
	pf := w.waitForward("web", "NoSharedIPv4")
	if pf.GetStatus().GetIpv4() != "" {
		t.Errorf("the forward is at %q with no shared address", pf.GetStatus().GetIpv4())
	}
	w.waitSettled()
	if got := w.host.Forwards(); len(got) > 0 {
		t.Errorf("a host without a shared address forwards %v", got)
	}
	if _, err := w.root.Delete(context.Background(), registry.PortForward, client.Key{Project: project, Name: "web"}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(w.t, "the deleted forward gone", func() (bool, string) {
		pf, ok := get[*networkv1alpha1.PortForward](w, registry.PortForward, "web")
		return !ok, fmt.Sprint(pf.GetStatus())
	})
}
