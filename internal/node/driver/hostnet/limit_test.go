package hostnet

import (
	"net/netip"
	"slices"
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// forwardOf is a table's forward chain.
func forwardOf(t *testing.T, chains []chainSpec) chainSpec {
	t.Helper()
	for _, c := range chains {
		if c.name == "forward" {
			return c
		}
	}
	t.Fatal("no forward chain")
	return chainSpec{}
}

// TestNoMailFromANetwork: TCP from a network to 25 and 465 is dropped on the
// host over both families, and nothing else of mail is: 587 stays open.
func TestNoMailFromANetwork(t *testing.T) {
	prefix := netip.MustParsePrefix("2001:db8:0:1::/64")
	for _, c := range []struct {
		name   string
		chains []chainSpec
	}{
		{"IPv4", rootChains("eth0", netip.Addr{})},
		{"IPv6", rootChains6("eth0", prefix, netip.Addr{})},
	} {
		t.Run(c.name, func(t *testing.T) {
			var dropped []uint16
			for _, r := range forwardOf(t, c.chains).rules {
				from, to, ok := dportsOf(r)
				if !ok {
					continue
				}
				if verdictOf(r) != expr.VerdictDrop || from != to {
					t.Errorf("rule %q: verdict %v on ports %d-%d, want a drop of one port", r.says, verdictOf(r), from, to)
				}
				if !matchesMeta(r, expr.MetaKeyL4PROTO, []byte{unix.IPPROTO_TCP}) {
					t.Errorf("rule %q does not match TCP", r.says)
				}
				if !matchesMeta(r, expr.MetaKeyIIFNAME, []byte(vethPrefix)) {
					t.Errorf("rule %q does not match a network's link, %s*", r.says, vethPrefix)
				}
				dropped = append(dropped, from)
			}
			if !slices.Equal(dropped, []uint16{25, 465}) {
				t.Errorf("forward drops TCP to %v, want 25 and 465 alone", dropped)
			}
		})
	}
}

// TestNewConnectionsAreLimited: a network's new connections out of its
// namespace, of either family, share one bucket of 100 a second after a
// burst of 300, and only new ones are counted.
func TestNewConnectionsAreLimited(t *testing.T) {
	chains := limitChains()
	if len(chains) != 1 {
		t.Fatalf("%d chains, want forward alone", len(chains))
	}
	c := chains[0]
	if c.hook != *nftables.ChainHookForward || c.typ != nftables.ChainTypeFilter {
		t.Errorf("chain %s is not a forward filter", c.name)
	}
	if len(c.rules) != 1 {
		t.Fatalf("%d rules, want one", len(c.rules))
	}
	r := c.rules[0]
	if verdictOf(r) != expr.VerdictDrop {
		t.Errorf("verdict %v, want drop", verdictOf(r))
	}
	if !matchesMeta(r, expr.MetaKeyIIFNAME, ifname(nsBridge)) || !matchesMeta(r, expr.MetaKeyOIFNAME, ifname(nsUplink)) {
		t.Errorf("rule %q does not match what leaves %s by %s", r.says, nsBridge, nsUplink)
	}
	// Any family: a match on one would give the other a bucket of its own,
	// or none.
	if slices.ContainsFunc(r.exprs, func(e expr.Any) bool {
		m, ok := e.(*expr.Meta)
		return ok && (m.Key == expr.MetaKeyNFPROTO || m.Key == expr.MetaKeyPROTOCOL)
	}) {
		t.Errorf("rule %q matches a family", r.says)
	}
	var limit *expr.Limit
	var state *expr.Bitwise
	for i, e := range r.exprs {
		switch x := e.(type) {
		case *expr.Limit:
			limit = x
		case *expr.Ct:
			if x.Key == expr.CtKeySTATE {
				state, _ = r.exprs[i+1].(*expr.Bitwise)
			}
		}
	}
	want := expr.Limit{Type: expr.LimitTypePkts, Rate: 100, Unit: expr.LimitTimeSecond, Burst: 300, Over: true}
	if limit == nil || *limit != want {
		t.Errorf("limit %+v, want %+v", limit, want)
	}
	if state == nil || !slices.Equal(state.Mask, binaryutil.NativeEndian.PutUint32(expr.CtStateBitNEW)) {
		t.Errorf("rule %q does not match new connections alone", r.says)
	}
}

func ifname(name string) []byte {
	b := make([]byte, unix.IFNAMSIZ)
	copy(b, name)
	return b
}
