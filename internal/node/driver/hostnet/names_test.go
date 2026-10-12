package hostnet

import (
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/google/nftables"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

func TestLinkNames(t *testing.T) {
	for _, p := range []string{vethPrefix, tapPrefix} {
		seen := map[string]string{}
		for _, key := range []string{"m1/eth0", "m1/eth1", "m1-and-a-longer-uid-sharing-its-prefix/eth0", "0192f3c4-5d6e-7f80-9a1b-2c3d4e5f6a7b/eth0"} {
			n := linkName(p, key, 0)
			if len(n) != 15 || !strings.HasPrefix(n, p) {
				t.Errorf("name %q of %q is not 15 characters under %s", n, key, p)
			}
			if n != linkName(p, key, 0) {
				t.Errorf("name of %q differs from call to call", key)
			}
			if other, ok := seen[n]; ok {
				t.Errorf("%q and %q share the name %s", key, other, n)
			}
			seen[n] = key
		}
	}
	if linkName(tapPrefix, "m1/eth0", 0) == linkName(tapPrefix, "m1/eth0", 1) {
		t.Error("a second attempt names the same as the first")
	}
}

func TestGatewayMAC(t *testing.T) {
	a, b := gatewayMAC("n1"), gatewayMAC("n2")
	if a.String() != gatewayMAC("n1").String() || a.String() == b.String() {
		t.Errorf("gateway MACs %s and %s of two networks", a, b)
	}
	if len(a) != 6 || a[0]&0x03 != 0x02 {
		t.Errorf("gateway MAC %s is not a locally administered unicast address", a)
	}
}

func TestFreeName(t *testing.T) {
	taken := map[string]bool{linkName(tapPrefix, "k", 0): true, linkName(tapPrefix, "k", 1): true}
	n, err := freeName(tapPrefix, "k", func(n string) bool { return taken[n] })
	if err != nil || n != linkName(tapPrefix, "k", 2) {
		t.Errorf("free name %q %v, want the third attempt's", n, err)
	}
	if n, err := freeName(tapPrefix, "k", func(string) bool { return false }); err != nil || n != linkName(tapPrefix, "k", 0) {
		t.Errorf("free name %q %v with nothing taken, want the first", n, err)
	}
	if _, err := freeName(tapPrefix, "k", func(string) bool { return true }); err == nil {
		t.Error("found a free name with every name taken")
	}
}

func TestAliases(t *testing.T) {
	i := driver.Interface{
		MachineUID: "0192f3c4-5d6e-7f80-9a1b-2c3d4e5f6a7b", Name: "eth0", NetworkUID: "0192f3c4-5d6e-7f80-9a1b-2c3d4e5f6a7c",
		MAC: "02:00:00:00:00:01", Address: netip.MustParsePrefix("10.0.0.2/24"), Gateway: netip.MustParseAddr("10.0.0.1"),
	}
	a, err := encodeAlias(tapMetaOf(i))
	if err != nil {
		t.Fatal(err)
	}
	m, ok := decodeTap(a)
	if !ok {
		t.Fatalf("alias %s does not decode", a)
	}
	i.Device = "hnt0"
	if got := m.iface(i.NetworkUID, "hnt0"); got != i {
		t.Errorf("read back as %+v, want %+v", got, i)
	}
	for _, bad := range []string{"", "eth0", "{}", `{"machine":"m1"}`, `{"name":"eth0"}`} {
		if _, ok := decodeTap(bad); ok {
			t.Errorf("alias %q decodes as a tap's", bad)
		}
	}
	if _, err := encodeAlias(tapMeta{Machine: strings.Repeat("m", 250), Name: "eth0"}); err == nil {
		t.Error("encoded an alias longer than Linux keeps")
	}

	v, err := encodeAlias(vethMeta{Network: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	if uid, ok := decodeVeth(v); !ok || uid != "n1" {
		t.Errorf("veth alias %s reads as %q %t", v, uid, ok)
	}
	for _, bad := range []string{"", "{}", `{"network":"../x"}`, `{"network":""}`} {
		if _, ok := decodeVeth(bad); ok {
			t.Errorf("alias %q decodes as a veth's", bad)
		}
	}
}

func TestChecks(t *testing.T) {
	good := driver.NetworkSpec{UID: "n1", CIDR: netip.MustParsePrefix("10.0.0.0/24"), Transit: netip.MustParsePrefix("100.64.0.0/31")}
	if err := checkNetwork(good); err != nil {
		t.Error(err)
	}
	for name, mutate := range map[string]func(*driver.NetworkSpec){
		"uid with a slash":       func(n *driver.NetworkSpec) { n.UID = "a/b" },
		"empty uid":              func(n *driver.NetworkSpec) { n.UID = "" },
		"dot uid":                func(n *driver.NetworkSpec) { n.UID = ".." },
		"range not masked":       func(n *driver.NetworkSpec) { n.CIDR = netip.MustParsePrefix("10.0.0.1/24") },
		"range of one address":   func(n *driver.NetworkSpec) { n.CIDR = netip.MustParsePrefix("10.0.0.0/31") },
		"IPv6 range":             func(n *driver.NetworkSpec) { n.CIDR = netip.MustParsePrefix("fd00::/64") },
		"no range":               func(n *driver.NetworkSpec) { n.CIDR = netip.Prefix{} },
		"transit of /30":         func(n *driver.NetworkSpec) { n.Transit = netip.MustParsePrefix("100.64.0.0/30") },
		"transit not masked":     func(n *driver.NetworkSpec) { n.Transit = netip.MustParsePrefix("100.64.0.1/31") },
		"transit outside 100.64": func(n *driver.NetworkSpec) { n.Transit = netip.MustParsePrefix("10.0.0.0/31") },
		"no transit":             func(n *driver.NetworkSpec) { n.Transit = netip.Prefix{} },
	} {
		n := good
		mutate(&n)
		if err := checkNetwork(n); err == nil {
			t.Errorf("%s: %+v passed", name, n)
		}
	}

	nic := driver.Interface{
		MachineUID: "m1", Name: "eth0", NetworkUID: "n1", MAC: "02:00:00:00:00:01",
		Address: netip.MustParsePrefix("10.0.0.2/24"), Gateway: netip.MustParseAddr("10.0.0.1"),
	}
	if err := checkInterface(nic); err != nil {
		t.Error(err)
	}
	for name, mutate := range map[string]func(*driver.Interface){
		"no machine":      func(i *driver.Interface) { i.MachineUID = "" },
		"no name":         func(i *driver.Interface) { i.Name = "" },
		"name with slash": func(i *driver.Interface) { i.Name = "eth/0" },
		"machine with #":  func(i *driver.Interface) { i.MachineUID = "m#1" },
		"bad network":     func(i *driver.Interface) { i.NetworkUID = "a/b" },
		"bad MAC":         func(i *driver.Interface) { i.MAC = "nope" },
		"long MAC":        func(i *driver.Interface) { i.MAC = "02:00:00:00:00:00:00:01" },
		"IPv6 address":    func(i *driver.Interface) { i.Address = netip.MustParsePrefix("fd00::2/64") },
		"no gateway":      func(i *driver.Interface) { i.Gateway = netip.Addr{} },
	} {
		i := nic
		mutate(&i)
		if err := checkInterface(i); err == nil {
			t.Errorf("%s: %+v passed", name, i)
		}
	}
}

// comments returns the comments a table's rules are written with.
func comments(t *testing.T, chains []chainSpec) []string {
	t.Helper()
	var out []string
	for _, c := range chains {
		for _, r := range c.rules {
			s, err := r.comment()
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
	}
	return out
}

func TestChains(t *testing.T) {
	masq := comments(t, rootChains("eth0", netip.Addr{}))
	if len(masq) != 5 {
		t.Fatalf("%d root rules, want 5: %q", len(masq), masq)
	}
	// A change of parameters must change a comment, or tableIs would take
	// the old rules for the new.
	for _, other := range [][]string{
		comments(t, rootChains("eth0", netip.MustParseAddr("192.0.2.1"))),
		comments(t, rootChains("eth1", netip.Addr{})),
	} {
		if !slices.ContainsFunc(other, func(c string) bool { return !slices.Contains(masq, c) }) {
			t.Errorf("root rules for other parameters share every comment: %q", other)
		}
	}
	a := comments(t, nsChains(netip.MustParsePrefix("10.0.0.0/24"), netip.MustParseAddr("100.64.0.1")))
	b := comments(t, nsChains(netip.MustParsePrefix("10.0.0.0/16"), netip.MustParseAddr("100.64.0.1")))
	c := comments(t, nsChains(netip.MustParsePrefix("10.0.0.0/24"), netip.MustParseAddr("100.64.0.3")))
	if a[0] == b[0] || a[1] == b[1] || a[1] == c[1] {
		t.Errorf("namespace rules for other parameters share a comment: %q %q %q", a, b, c)
	}
	// What a comment says is not all: the same words over other
	// expressions are another comment.
	r := rootChains("eth0", netip.Addr{})[0].rules[0]
	before, _ := r.comment()
	r.exprs = r.exprs[:len(r.exprs)-1]
	if after, _ := r.comment(); after == before {
		t.Error("a rule's comment does not change with its expressions")
	}
}

func TestIsOurs(t *testing.T) {
	ours := &nftables.Table{Family: nftables.TableFamilyIPv4, Name: Table}
	for _, c := range []struct {
		data any
		want bool
	}{
		{ours, true},
		{&nftables.Chain{Table: ours}, true},
		{&nftables.Rule{Table: ours}, true},
		{&nftables.Table{Family: nftables.TableFamilyINet, Name: Table}, false},
		{&nftables.Table{Family: nftables.TableFamilyIPv4, Name: "hangar_testbed_guest"}, false},
		{&nftables.Rule{Table: &nftables.Table{Family: nftables.TableFamilyIPv4, Name: "other"}}, false},
		{nil, false},
	} {
		if got := isOurs(c.data); got != c.want {
			t.Errorf("isOurs(%+v) = %t", c.data, got)
		}
	}
}
