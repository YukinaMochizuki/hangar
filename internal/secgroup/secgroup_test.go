package secgroup

import (
	"net/netip"
	"slices"
	"testing"

	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
)

func TestParsePorts(t *testing.T) {
	for in, want := range map[string][]PortRange{
		"443":            {{443, 443}},
		"8000-8080":      {{8000, 8080}},
		"80, 443,1-1024": {{80, 80}, {443, 443}, {1, 1024}},
	} {
		got, err := ParsePorts(in)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%q: %v %v, want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"0", "65536", "80-79", "a", "80-", "-80", "1,,2", "1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17"} {
		if got, err := ParsePorts(bad); err == nil {
			t.Errorf("%q accepted as %v", bad, got)
		}
	}
}

func peerCIDR(c string) *networkv1alpha1.SecurityGroupPeer {
	return &networkv1alpha1.SecurityGroupPeer{Peer: &networkv1alpha1.SecurityGroupPeer_Cidr{Cidr: c}}
}

func peerGroup(g string) *networkv1alpha1.SecurityGroupPeer {
	return &networkv1alpha1.SecurityGroupPeer{Peer: &networkv1alpha1.SecurityGroupPeer_Group{Group: g}}
}

func peerNetwork(n string) *networkv1alpha1.SecurityGroupPeer {
	return &networkv1alpha1.SecurityGroupPeer{Peer: &networkv1alpha1.SecurityGroupPeer_Network{Network: n}}
}

func TestFromSpec(t *testing.T) {
	tcp := networkv1alpha1.SecurityGroupProtocol_SECURITY_GROUP_PROTOCOL_TCP
	icmp := networkv1alpha1.SecurityGroupProtocol_SECURITY_GROUP_PROTOCOL_ICMP
	ok := &networkv1alpha1.SecurityGroupSpec{
		Ingress: []*networkv1alpha1.IngressRule{{Protocol: tcp, Ports: "443", From: peerCIDR("0.0.0.0/0")}},
		Egress:  []*networkv1alpha1.EgressRule{{Protocol: icmp, To: peerGroup("default")}},
	}
	g, err := FromSpec(ok)
	if err != nil || len(g.Ingress) != 1 || g.Ingress[0].PeerCIDR != netip.MustParsePrefix("0.0.0.0/0") || g.Egress[0].PeerGroup != "default" {
		t.Errorf("%+v %v", g, err)
	}
	v6 := &networkv1alpha1.SecurityGroupSpec{Ingress: []*networkv1alpha1.IngressRule{{Protocol: tcp, Ports: "22", From: peerCIDR("::/0")}}}
	if g, err := FromSpec(v6); err != nil || g.Ingress[0].PeerCIDR != netip.MustParsePrefix("::/0") {
		t.Errorf("an IPv6 range: %+v %v", g, err)
	}
	fromNet := &networkv1alpha1.SecurityGroupSpec{Ingress: []*networkv1alpha1.IngressRule{{Protocol: tcp, Ports: "22", From: peerNetwork("ws-a")}}}
	if g, err := FromSpec(fromNet); err != nil || g.Ingress[0].PeerNetwork != "ws-a" {
		t.Errorf("a network peer: %+v %v", g, err)
	}
	toNet := &networkv1alpha1.SecurityGroupSpec{Egress: []*networkv1alpha1.EgressRule{{Protocol: tcp, To: peerNetwork("ws-a")}}}
	if g, err := FromSpec(toNet); err == nil {
		t.Errorf("an egress rule took a network peer: %+v", g)
	}
	for name, r := range map[string]*networkv1alpha1.IngressRule{
		"no protocol":          {From: peerCIDR("0.0.0.0/0")},
		"ports on icmp":        {Protocol: icmp, Ports: "1", From: peerCIDR("0.0.0.0/0")},
		"bad ports":            {Protocol: tcp, Ports: "x", From: peerCIDR("0.0.0.0/0")},
		"no peer":              {Protocol: tcp},
		"an IPv4-mapped range": {Protocol: tcp, From: peerCIDR("::ffff:10.0.0.0/120")},
		"a zone":               {Protocol: tcp, From: peerCIDR("fe80::%eth0/64")},
		"host bits":            {Protocol: tcp, From: peerCIDR("10.0.0.1/24")},
		"IPv6 host bits":       {Protocol: tcp, From: peerCIDR("2001:db8::1/64")},
		"an empty group":       {Protocol: tcp, From: peerGroup("")},
		"an empty network":     {Protocol: tcp, From: peerNetwork("")},
		"not a range":          {Protocol: tcp, From: peerCIDR("nonsense")},
	} {
		if _, err := FromSpec(&networkv1alpha1.SecurityGroupSpec{Ingress: []*networkv1alpha1.IngressRule{r}}); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestResolve(t *testing.T) {
	a := func(s string) netip.Addr { return netip.MustParseAddr(s) }
	p := func(s string) netip.Prefix { return netip.PrefixFrom(a(s), a(s).BitLen()) }
	as := func(s ...string) []netip.Addr {
		var out []netip.Addr
		for _, x := range s {
			out = append(out, a(x))
		}
		return out
	}
	groups := map[string]Group{
		"web": {Ingress: []RuleSpec{{Protocol: TCP, Ports: []PortRange{{443, 443}}, PeerCIDR: netip.MustParsePrefix("0.0.0.0/0")}}},
		"db":  {Ingress: []RuleSpec{{Protocol: TCP, Ports: []PortRange{{5432, 5432}}, PeerGroup: "web"}}},
		"sandbox": {Egress: []RuleSpec{
			{Protocol: TCP, Ports: []PortRange{{443, 443}}, PeerCIDR: netip.MustParsePrefix("140.82.112.0/20")},
			{Protocol: UDP, PeerGroup: "nobody"},
		}},
	}
	members := []Member{
		{Addrs: as("10.0.0.2", "2001:db8::2")},                           // default
		{Addrs: as("10.0.0.3", "2001:db8::3"), Groups: []string{"web"}},  // web
		{Addrs: as("10.0.0.4"), Groups: []string{"db"}},                  // db
		{Addrs: as("10.0.0.5"), Groups: []string{Default, "web"}},        // both
		{Addrs: as("10.0.0.6"), Groups: []string{"sandbox"}},             // locked down
		{Addrs: as("10.0.0.7"), Groups: []string{"web", "not-made-yet"}}, // a group missing
		{Addrs: as("10.0.0.8"), Groups: []string{"nobody"}},              // names a group that is not there
		{Addrs: as("10.0.0.9"), Isolated: true},                          // a file share's server
		{Addrs: as("10.0.0.10"), Groups: []string{"web"}, Isolated: true},
	}
	got := Resolve(groups, members, nil)

	if d := got[0]; len(d.Ingress) != 1 || !slices.Equal(d.Ingress[0].Peers, []netip.Prefix{p("10.0.0.2"), p("2001:db8::2"), p("10.0.0.5")}) || !d.EgressAll {
		t.Errorf("default member: %+v", d)
	}
	if w := got[1]; len(w.Ingress) != 1 || w.Ingress[0].Peers[0] != netip.MustParsePrefix("0.0.0.0/0") || !w.EgressAll {
		t.Errorf("web member: %+v", w)
	}
	if db := got[2]; len(db.Ingress) != 1 || !slices.Equal(db.Ingress[0].Peers, []netip.Prefix{p("10.0.0.3"), p("2001:db8::3"), p("10.0.0.5"), p("10.0.0.7")}) {
		t.Errorf("db member: %+v", db)
	}
	if both := got[3]; len(both.Ingress) != 2 {
		t.Errorf("member of default and web: %+v", both)
	}
	sb := got[4]
	if sb.EgressAll || len(sb.Egress) != 2 || len(sb.Ingress) != 0 || len(sb.Egress[1].Peers) != 0 {
		t.Errorf("sandbox member: a rule naming a group that is not there must match nobody, even an interface naming it: %+v", sb)
	}
	if m := got[5]; m.EgressAll || len(m.Egress) != 0 || len(m.Ingress) != 1 {
		t.Errorf("a member of a group not made yet must be no more open, and send nothing: %+v", m)
	}
	// Isolated members are in no group, Default included, and are let
	// neither in nor out.
	for i, m := range got[7:] {
		if m.EgressAll || len(m.Egress) != 0 || len(m.Ingress) != 0 {
			t.Errorf("isolated member %d: %+v", i, m)
		}
	}
	for _, peer := range slices.Concat(got[0].Ingress[0].Peers, got[2].Ingress[0].Peers) {
		if peer == p("10.0.0.9") || peer == p("10.0.0.10") {
			t.Errorf("an isolated member is a peer of default or web: %v", peer)
		}
	}
}

// A network peer stands for that network's transit address alone, and for
// no one while the network is not known.
func TestResolveNetworkPeers(t *testing.T) {
	groups := map[string]Group{"git": {Ingress: []RuleSpec{
		{Protocol: TCP, Ports: []PortRange{{22, 22}}, PeerNetwork: "ws-a"},
		{Protocol: TCP, Ports: []PortRange{{22, 22}}, PeerNetwork: "not-made-yet"},
	}}}
	members := []Member{{Addrs: []netip.Addr{netip.MustParseAddr("10.0.0.2")}, Groups: []string{"git"}}}
	transits := map[string]netip.Addr{"ws-a": netip.MustParseAddr("100.64.0.1"), "ws-b": netip.MustParseAddr("100.64.0.3")}
	got := Resolve(groups, members, transits)[0]
	if len(got.Ingress) != 2 || !slices.Equal(got.Ingress[0].Peers, []netip.Prefix{netip.MustParsePrefix("100.64.0.1/32")}) || len(got.Ingress[1].Peers) != 0 {
		t.Errorf("policy %+v, want ws-a's transit address alone, then no one", got)
	}
}
