package apiserver

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

const testPrefix = "2001:db8:0:1::/64"

// nodeReporting reports a node ready with room and the given IPv6 prefix,
// none if empty.
func nodeReporting(t *testing.T, s *Server, node, prefix string) {
	t.Helper()
	status := &computev1alpha1.NodeStatus{
		Capacity: &computev1alpha1.NodeResources{Cpu: "8", Memory: "4096Mi"},
		Conditions: []*metav1.Condition{{
			Type: NodeReady, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "AgentReporting",
		}},
		Ipv6Prefix: prefix,
	}
	if _, err := s.backend(registry.Node).UpdateStatus(asNode(t.Context(), node), resource.Key{Name: node}, status, nil); err != nil {
		t.Fatal(err)
	}
}

func ipv6Of(m *computev1alpha1.Machine) string { return m.GetSpec().GetInterfaces()[0].GetIpv6() }

// checkIPv6 fails t unless a is a /128 of the prefix drawn from where
// machines' addresses come from.
func checkIPv6(t *testing.T, a string) netip.Addr {
	t.Helper()
	p, err := netip.ParsePrefix(a)
	if err != nil || p.Bits() != 128 || !netip.MustParsePrefix(testPrefix).Contains(p.Addr()) {
		t.Fatalf("%q is not a /128 of %s", a, testPrefix)
	}
	b := p.Addr().As16()
	if binary.BigEndian.Uint32(b[8:12]) == 0 {
		t.Errorf("%s has an interface ID below 2^32, which is the host's", p.Addr())
	}
	return p.Addr()
}

func TestMachinesOnANodeWithAPrefixGetIPv6TheyKeep(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeReporting(t, s, "n1", testPrefix)
	seen := map[netip.Addr]string{}
	for _, name := range []string{"a", "b", "c"} {
		applyCtr(t, s, user, ctr(name))
		m := waitMachine(t, s, system, "ct-"+name, "given IPv6 on its node", func(m *computev1alpha1.Machine) bool {
			return m.GetSpec().GetNode() == "n1" && ipv6Of(m) != ""
		})
		a := checkIPv6(t, ipv6Of(m))
		if other, dup := seen[a]; dup {
			t.Errorf("%s and %s share %s", name, other, a)
		}
		seen[a] = name
	}

	// A change to the container keeps the address.
	a := getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-a"})
	changed := ctr("a")
	changed.Spec.Image = "nginx:2"
	applyCtr(t, s, user, changed)
	a2 := waitMachine(t, s, system, "ct-a", "updated", func(m *computev1alpha1.Machine) bool {
		return m.GetSpec().GetContainer().GetImage() == "nginx:2"
	})
	if ipv6Of(a2) != ipv6Of(a) {
		t.Errorf("an update moved the address from %s to %s", ipv6Of(a), ipv6Of(a2))
	}

	// A machine being deleted keeps its address until its node releases it,
	// and so do others theirs.
	if _, err := s.backend(registry.Container).Delete(user, resource.Key{Project: "web", Name: "b"}, nil); err != nil {
		t.Fatal(err)
	}
	b := waitMachine(t, s, system, "ct-b", "marked", func(m *computev1alpha1.Machine) bool { return m.GetMetadata().GetDeleteTime() != nil })
	if ipv6Of(b) == "" {
		t.Errorf("a machine being deleted lost its address")
	}
}

// TestAMachineIsPlacedWithItsIPv6: the scheduler writes the node and the
// IPv6 address together, so the node never runs the machine without it
// and then again with it.
func TestAMachineIsPlacedWithItsIPv6(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeReporting(t, s, "n1", testPrefix)
	applyCtr(t, s, user, ctr("a"))
	m := waitMachine(t, s, system, "ct-a", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	if ipv6Of(m) == "" {
		t.Fatalf("machine placed without IPv6 at generation %d", m.GetMetadata().GetGeneration())
	}
	testutil.Unchanged(t, "the placed machine's generation", 200*time.Millisecond, func() (string, error) {
		return fmt.Sprint(getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-a"}).GetMetadata().GetGeneration()), nil
	})
	if again := getMachine(t, s, system, resource.Key{Project: "web", Name: "ct-a"}); again.GetMetadata().GetGeneration() != m.GetMetadata().GetGeneration() {
		t.Errorf("the machine's spec changed after placement, generation %d to %d: %v", m.GetMetadata().GetGeneration(), again.GetMetadata().GetGeneration(), again.GetSpec())
	}
}

func TestANodesPrefixComesAndGoes(t *testing.T) {
	s, system, user := controlled(t, "n1")
	nodeReporting(t, s, "n1", "")
	applyCtr(t, s, user, ctr("a"))
	m := waitMachine(t, s, system, "ct-a", "placed", func(m *computev1alpha1.Machine) bool { return m.GetSpec().GetNode() == "n1" })
	if ipv6Of(m) != "" {
		t.Fatalf("a machine on a node without a prefix got %s", ipv6Of(m))
	}

	nodeReporting(t, s, "n1", testPrefix)
	m = waitMachine(t, s, system, "ct-a", "given IPv6 once its node has a prefix", func(m *computev1alpha1.Machine) bool { return ipv6Of(m) != "" })
	checkIPv6(t, ipv6Of(m))

	other := "2001:db8:0:2::/64"
	nodeReporting(t, s, "n1", other)
	waitMachine(t, s, system, "ct-a", "moved to the node's new prefix", func(m *computev1alpha1.Machine) bool {
		p, err := netip.ParsePrefix(ipv6Of(m))
		return err == nil && netip.MustParsePrefix(other).Contains(p.Addr())
	})

	nodeReporting(t, s, "n1", "")
	waitMachine(t, s, system, "ct-a", "without IPv6 once its node has no prefix", func(m *computev1alpha1.Machine) bool { return ipv6Of(m) == "" })
}

// reader returns the ids in turn, as rand.Read would.
func reader(ids ...uint64) func([]byte) (int, error) {
	return func(b []byte) (int, error) {
		if len(ids) == 0 {
			return 0, fmt.Errorf("out of ids")
		}
		binary.BigEndian.PutUint64(b, ids[0])
		ids = ids[1:]
		return len(b), nil
	}
}

func TestFreeIPv6SkipsReservedAndTaken(t *testing.T) {
	p := netip.MustParsePrefix(testPrefix)
	taken := map[netip.Addr]bool{netip.MustParseAddr("2001:db8:0:1:1234:5678:9abc:def0"): true}
	got, err := freeIPv6(p, taken, reader(0, 1, 0xffffffff, 0xfdffffffffffff80, 0xffffffffffffffff, 0x123456789abcdef0, 0x0000000100000000))
	if err != nil || got != netip.MustParseAddr("2001:db8:0:1:0:1::") {
		t.Errorf("got %s, %v; want the first id neither the host's, reserved anycast nor taken", got, err)
	}
	if _, err := freeIPv6(p, taken, reader(0)); err == nil {
		t.Errorf("no id left to draw gave no error")
	}
}

func TestFreeIPv6IsNotSequential(t *testing.T) {
	p := netip.MustParsePrefix(testPrefix)
	taken := map[netip.Addr]bool{}
	var last []byte
	for range 8 {
		a, err := freeIPv6(p, taken, rand.Read)
		if err != nil {
			t.Fatal(err)
		}
		b := a.As16()
		if last != nil && (bytes.Equal(b[8:], last) || binary.BigEndian.Uint64(b[8:])-binary.BigEndian.Uint64(last) == 1) {
			t.Fatalf("%s follows the address before", a)
		}
		last = b[8:]
		taken[a] = true
	}
}

func TestParseIPv6Prefix(t *testing.T) {
	if p, err := computeapi.ParseIPv6Prefix(testPrefix); err != nil || p.String() != testPrefix {
		t.Errorf("%s: %v %v", testPrefix, p, err)
	}
	for _, bad := range []string{"", "2001:db8::/48", "2001:db8::1/64", "10.0.0.0/8", "fe80::/64", "::ffff:0:0/96", "ff02::/64", "nonsense"} {
		if _, err := computeapi.ParseIPv6Prefix(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestMachineIPv6Admission(t *testing.T) {
	s, system, _ := running(t)
	b := s.backend(registry.Machine)
	ok := machine("ct-ok", "n1")
	ok.Spec.Interfaces[0].Ipv6 = "2001:db8:0:1:1:2:3:4/128"
	if _, err := b.Apply(system, ok); err != nil {
		t.Fatal(err)
	}
	for name, a := range map[string]string{
		"not /128":          "2001:db8:0:1:1:2:3:4/64",
		"IPv4":              "10.0.0.5/32",
		"link-local":        "fe80::1/128",
		"IPv4-mapped":       "::ffff:10.0.0.5/128",
		"not an address":    "nonsense",
		"with a zone":       "2001:db8::1%eth0/128",
		"the multicast one": "ff02::1/128",
	} {
		m := machine("ct-bad", "n1")
		m.Spec.Interfaces[0].Ipv6 = a
		_, err := b.Apply(system, m)
		wantCodeOf(t, err, connect.CodeInvalidArgument, name)
	}
	unplaced := machine("ct-unplaced", "")
	unplaced.Spec.Interfaces[0].Ipv6 = "2001:db8:0:1:1:2:3:4/128"
	_, err := b.Apply(system, unplaced)
	wantCodeOf(t, err, connect.CodeInvalidArgument, "IPv6 on a machine on no node")
}
