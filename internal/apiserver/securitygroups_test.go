package apiserver

import (
	"testing"

	"connectrpc.com/connect"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
)

// elsewhere is a group of another network than the default one.
func elsewhere() *networkv1alpha1.SecurityGroup {
	g := group("elsewhere", https())
	g.Spec.Network = "backend"
	return g
}

func group(name string, ingress ...*networkv1alpha1.IngressRule) *networkv1alpha1.SecurityGroup {
	return &networkv1alpha1.SecurityGroup{
		Metadata: &metav1.ObjectMeta{Project: "web", Name: name},
		Spec:     &networkv1alpha1.SecurityGroupSpec{Network: DefaultNetwork, Ingress: ingress},
	}
}

func https() *networkv1alpha1.IngressRule {
	return &networkv1alpha1.IngressRule{
		Protocol: networkv1alpha1.SecurityGroupProtocol_SECURITY_GROUP_PROTOCOL_TCP, Ports: "443",
		From: &networkv1alpha1.SecurityGroupPeer{Peer: &networkv1alpha1.SecurityGroupPeer_Cidr{Cidr: "0.0.0.0/0"}},
	}
}

func TestSecurityGroupAdmission(t *testing.T) {
	s, _, user := running(t)
	b := s.backend(registry.SecurityGroup)
	if _, err := b.Apply(user, group("web-in", https())); err != nil {
		t.Fatalf("a valid group: %v", err)
	}
	if _, err := b.Apply(user, elsewhere()); err != nil {
		t.Fatalf("a group of another network: %v", err)
	}
	peer := func(name, of string) *networkv1alpha1.SecurityGroup {
		return group(name, &networkv1alpha1.IngressRule{
			Protocol: networkv1alpha1.SecurityGroupProtocol_SECURITY_GROUP_PROTOCOL_ALL,
			From:     &networkv1alpha1.SecurityGroupPeer{Peer: &networkv1alpha1.SecurityGroupPeer_Group{Group: of}},
		})
	}
	for _, of := range []string{"web-in", "default", "not-made-yet", "itself"} {
		if _, err := b.Apply(user, peer("itself", of)); err != nil {
			t.Errorf("a peer group %q: %v", of, err)
		}
	}
	bad := map[string]*networkv1alpha1.SecurityGroup{
		"a peer group of another network": peer("crossing", "elsewhere"),
		"the implicit group's name":       group("default", https()),
		"no network": func() *networkv1alpha1.SecurityGroup {
			g := group("nonet", https())
			g.Spec.Network = ""
			return g
		}(),
		"a rule without a peer": group("nopeer", &networkv1alpha1.IngressRule{Protocol: networkv1alpha1.SecurityGroupProtocol_SECURITY_GROUP_PROTOCOL_TCP}),
		"a group peer not a name": group("badpeer", &networkv1alpha1.IngressRule{
			Protocol: networkv1alpha1.SecurityGroupProtocol_SECURITY_GROUP_PROTOCOL_ALL,
			From:     &networkv1alpha1.SecurityGroupPeer{Peer: &networkv1alpha1.SecurityGroupPeer_Group{Group: "Not_A_Name"}},
		}),
	}
	for name, g := range bad {
		if _, err := b.Apply(user, g); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	moved := group("web-in", https())
	moved.Spec.Network = "backend"
	if _, err := b.Apply(user, moved); err == nil {
		t.Error("a group moved to another network")
	}
}

func TestInterfaceGroupAdmission(t *testing.T) {
	s, _, user := running(t)
	if _, err := s.backend(registry.SecurityGroup).Apply(user, elsewhere()); err != nil {
		t.Fatal(err)
	}
	b := s.backend(registry.Container)
	for name, tc := range map[string]struct {
		groups []string
		ok     bool
	}{
		"default and one":   {[]string{"default", "web-in"}, true},
		"one not made yet":  {[]string{"later"}, true},
		"a group twice":     {[]string{"web-in", "web-in"}, false},
		"not a name":        {[]string{"Not_A_Name"}, false},
		"more than five":    {[]string{"a", "b", "c", "d", "e", "f"}, false},
		"another network's": {[]string{"default", "elsewhere"}, false},
	} {
		c := ctr("api")
		c.Spec.Interfaces = []*computev1alpha1.NetworkInterface{{Network: DefaultNetwork, SecurityGroups: tc.groups}}
		_, err := b.Apply(user, c)
		if got := err == nil; got != tc.ok {
			t.Errorf("%s: accepted %t, want %t (%v)", name, got, tc.ok, err)
		}
	}
}

// TestAGroupInUseStays: a group an interface is in cannot be deleted; once
// no interface is in it, it can.
func TestAGroupInUseStays(t *testing.T) {
	s, system, user := controlled(t)
	if _, err := s.backend(registry.SecurityGroup).Apply(user, group("web-in", https())); err != nil {
		t.Fatal(err)
	}
	c := ctr("api")
	c.Spec.Interfaces = []*computev1alpha1.NetworkInterface{{Network: DefaultNetwork, SecurityGroups: []string{"default", "web-in"}}}
	applyCtr(t, s, user, c)
	waitMachine(t, s, system, "ct-api", "in both groups", func(m *computev1alpha1.Machine) bool {
		nics := m.GetSpec().GetInterfaces()
		return len(nics) == 1 && len(nics[0].GetSecurityGroups()) == 2
	})
	key := resource.Key{Project: "web", Name: "web-in"}
	if _, err := s.backend(registry.SecurityGroup).Delete(user, key, nil); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("deleting a group in use: %v", err)
	}
	applyCtr(t, s, user, ctr("api"))
	waitMachine(t, s, system, "ct-api", "back in default alone", func(m *computev1alpha1.Machine) bool {
		nics := m.GetSpec().GetInterfaces()
		return len(nics) == 1 && len(nics[0].GetSecurityGroups()) == 1 && nics[0].GetSecurityGroups()[0] == "default"
	})
	if _, err := s.backend(registry.SecurityGroup).Delete(user, key, nil); err != nil {
		t.Errorf("deleting a group no longer in use: %v", err)
	}
}
