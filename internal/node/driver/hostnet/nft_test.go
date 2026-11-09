package hostnet

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/secgroup"
)

// fakeKernel is netfilter as a table's writer and reader see it: it keeps
// the tables, chains and rules a batch adds, as the messages that added
// them, and lists them back. lost is what becomes of a batch's replies
// (each rule's echo and each message's ack): with ENOBUFS, the kernel had
// no room for them on the socket, having committed the batch if commits.
// A batch larger than sendBuffer, if set, is refused whole with EMSGSIZE,
// which reaches the writer from each reply as nltest has it, rather than
// from the sendmsg as from the kernel; largest is the largest batch sent,
// in bytes.
type fakeKernel struct {
	msgs       []netlink.Message
	lost       error
	commits    bool
	batches    int
	sendBuffer int
	largest    int
}

// batchSize is the bytes of a batch's one sendmsg: each message's header
// and data, padded to four bytes.
func batchSize(req []netlink.Message) int {
	n := 0
	for _, m := range req {
		n += (unix.NLMSG_HDRLEN + len(m.Data) + unix.NLMSG_ALIGNTO - 1) &^ (unix.NLMSG_ALIGNTO - 1)
	}
	return n
}

func nftMsg(m netlink.Message) uint16 { return uint16(m.Header.Type) & 0xff }

// attr is m's attribute typ, a NUL-terminated string as nftables sends it.
func attr(t *testing.T, m netlink.Message, typ uint16) string {
	t.Helper()
	ad, err := netlink.NewAttributeDecoder(m.Data[4:])
	if err != nil {
		t.Fatal(err)
	}
	for ad.Next() {
		if ad.Type() == typ {
			return ad.String()
		}
	}
	if err := ad.Err(); err != nil {
		t.Fatal(err)
	}
	return ""
}

func (k *fakeKernel) dial(t *testing.T) func([]netlink.Message) ([]netlink.Message, error) {
	return func(req []netlink.Message) ([]netlink.Message, error) {
		if len(req) == 0 {
			return nil, nil
		}
		if req[0].Header.Type == netlink.HeaderType(unix.NFNL_MSG_BATCH_BEGIN) {
			size := batchSize(req)
			k.largest = max(k.largest, size)
			if k.sendBuffer > 0 && size > k.sendBuffer {
				return nil, syscall.EMSGSIZE
			}
			k.batches++
			if k.commits {
				for _, m := range req[1 : len(req)-1] {
					switch nftMsg(m) {
					case unix.NFT_MSG_DELTABLE:
						k.msgs = slices.DeleteFunc(k.msgs, func(have netlink.Message) bool {
							return attr(t, have, tableAttrOf(nftMsg(have))) == attr(t, m, unix.NFTA_TABLE_NAME)
						})
					case unix.NFT_MSG_NEWTABLE, unix.NFT_MSG_NEWCHAIN, unix.NFT_MSG_NEWRULE:
						k.msgs = append(k.msgs, m)
					}
				}
			}
			return nil, k.lost
		}
		// A dump: what the batches added of the kind asked for.
		var kind uint16
		switch nftMsg(req[0]) {
		case unix.NFT_MSG_GETTABLE:
			kind = unix.NFT_MSG_NEWTABLE
		case unix.NFT_MSG_GETCHAIN:
			kind = unix.NFT_MSG_NEWCHAIN
		case unix.NFT_MSG_GETRULE:
			kind = unix.NFT_MSG_NEWRULE
		default:
			t.Fatalf("unexpected nft message %d", nftMsg(req[0]))
		}
		var out []netlink.Message
		for _, m := range k.msgs {
			if nftMsg(m) != kind ||
				(kind == unix.NFT_MSG_NEWRULE && attr(t, m, unix.NFTA_RULE_CHAIN) != attr(t, req[0], unix.NFTA_RULE_CHAIN)) {
				continue
			}
			m.Header.Flags, m.Header.Sequence, m.Header.PID = 0, req[0].Header.Sequence, req[0].Header.PID
			out = append(out, m)
		}
		return out, nil
	}
}

// tableAttrOf is the attribute of a message of kind naming its table.
func tableAttrOf(kind uint16) uint16 {
	switch kind {
	case unix.NFT_MSG_NEWCHAIN:
		return unix.NFTA_CHAIN_TABLE
	case unix.NFT_MSG_NEWRULE:
		return unix.NFTA_RULE_TABLE
	}
	return unix.NFTA_TABLE_NAME
}

func testChains(n int) []chainSpec {
	c := chainSpec{name: "forward", typ: nftables.ChainTypeFilter, hook: *nftables.ChainHookForward, prio: *nftables.ChainPriorityFilter}
	for i := range n {
		c.rules = append(c.rules, ruleSpec{says: string(rune('a' + i)), exprs: []expr.Any{&expr.Verdict{Kind: expr.VerdictAccept}}})
	}
	return []chainSpec{c}
}

// A write whose replies the socket had no room for is as the table then
// is: the kernel commits a batch before it replies, and drops replies, not
// batches, when the socket is full.
func TestEnsureTableTakesACommittedWriteWhoseRepliesWereLost(t *testing.T) {
	k := &fakeKernel{commits: true}
	conn, err := nftables.New(nftables.WithTestDial(k.dial(t)))
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureNamedTable(conn, nftables.TableFamilyBridge, "t", testChains(1)); err != nil {
		t.Fatal(err)
	}

	k.lost = syscall.ENOBUFS
	if err := ensureNamedTable(conn, nftables.TableFamilyBridge, "t", testChains(3)); err != nil {
		t.Fatalf("a write the kernel committed failed: %v", err)
	}
	if same, _, err := tableIs(conn, nftables.TableFamilyBridge, "t", testChains(3)); err != nil || !same {
		t.Fatalf("the table is not as written: same=%v, err=%v", same, err)
	}
	if k.batches != 2 {
		t.Errorf("%d batches, want 2: one per write", k.batches)
	}
}

// One the kernel did not commit fails, though its error was lost too.
func TestEnsureTableFailsAnUncommittedWriteWhoseRepliesWereLost(t *testing.T) {
	k := &fakeKernel{commits: true}
	conn, err := nftables.New(nftables.WithTestDial(k.dial(t)))
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureNamedTable(conn, nftables.TableFamilyBridge, "t", testChains(1)); err != nil {
		t.Fatal(err)
	}

	k.lost, k.commits = syscall.ENOBUFS, false
	err = ensureNamedTable(conn, nftables.TableFamilyBridge, "t", testChains(3))
	if !errors.Is(err, syscall.ENOBUFS) {
		t.Fatalf("got %v, want the lost replies' ENOBUFS", err)
	}
	if same, _, err := tableIs(conn, nftables.TableFamilyBridge, "t", testChains(1)); err != nil || !same {
		t.Fatalf("the table changed: same=%v, err=%v", same, err)
	}
}

// A table larger than the socket's send buffer is never sent: the write
// fails saying so, and the table is as it was.
func TestEnsureTableSaysATableIsTooLargeToSend(t *testing.T) {
	k := &fakeKernel{commits: true}
	conn, err := nftables.New(nftables.WithTestDial(k.dial(t)))
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureNamedTable(conn, nftables.TableFamilyBridge, "t", testChains(1)); err != nil {
		t.Fatal(err)
	}

	k.sendBuffer = k.largest
	err = ensureNamedTable(conn, nftables.TableFamilyBridge, "t", testChains(3))
	if !errors.Is(err, syscall.EMSGSIZE) || !strings.Contains(err.Error(), "its 3 rules are more than one netlink message takes") {
		t.Fatalf("got %v, want EMSGSIZE saying the table is too large", err)
	}
	if same, _, err := tableIs(conn, nftables.TableFamilyBridge, "t", testChains(1)); err != nil || !same {
		t.Fatalf("the table changed: same=%v, err=%v", same, err)
	}
}

// nftSendBuffer takes the largest tables the driver writes in one batch:
// a firewall of driver.MaxFirewallRules of either of its longest rules,
// policy rules from IPv6 peers written in full and file share client
// rules, and the binding table of a /16 network's first thousand taps.
// Whether newConn's sockets get that buffer is the testbed's to show
// (TestLargeTablesApply).
func TestNftSendBufferTakesTheLargestTables(t *testing.T) {
	const taps = 100
	full6 := func(i int) netip.Addr {
		return netip.AddrFrom16([16]byte{0x24, 0x02, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, byte(i>>8) | 0x10, byte(i) | 0x10})
	}
	var peers []netip.Prefix
	var clients []netip.Addr
	for i := range driver.MaxFirewallRules / taps {
		peers = append(peers, netip.PrefixFrom(full6(i), 128))
		clients = append(clients, netip.AddrFrom4([4]byte{172, 31, 255, byte(i)}))
	}
	var policies, servers []tapPolicy
	for i := range taps {
		p := secgroup.Policy{Ingress: []secgroup.Rule{{Protocol: secgroup.TCP, Ports: []secgroup.PortRange{{From: 10000, To: 20000}}, Peers: peers}}}
		policies = append(policies, tapPolicy{name: fmt.Sprintf("%s%012d", tapPrefix, i), policy: p})
		servers = append(servers, tapPolicy{name: fmt.Sprintf("%s%011d", serverTapPrefix, i), clients: clients})
	}
	var bound []boundTap
	for i := range 1000 {
		bound = append(bound, boundTap{name: fmt.Sprintf("%s%012d", tapPrefix, i), mac: net.HardwareAddr{2, 0, 0, 0, byte(i >> 8), byte(i)},
			addr: netip.AddrFrom4([4]byte{10, 0, byte(i >> 8), byte(i)}), ipv6: full6(i)})
	}
	tables := []struct {
		name, table string
		chains      []chainSpec
	}{{"binding", Table, bindingChains(bound)}}
	for _, fw := range []struct {
		name string
		taps []tapPolicy
	}{{"firewall of IPv6 peers", policies}, {"firewall of file share clients", servers}} {
		chains, err := firewallChains(netip.MustParseAddr("10.0.0.1"), fw.taps)
		if err != nil {
			t.Fatal(err)
		}
		tables = append(tables, struct {
			name, table string
			chains      []chainSpec
		}{fw.name, FirewallTable, chains})
	}
	for _, c := range tables {
		k := &fakeKernel{commits: true, sendBuffer: nftSendBuffer}
		conn, err := nftables.New(nftables.WithTestDial(k.dial(t)))
		if err != nil {
			t.Fatal(err)
		}
		if err := ensureNamedTable(conn, nftables.TableFamilyBridge, c.table, c.chains); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
		// The host's default buffer would not take it: the case is real.
		if k.largest <= 212992 {
			t.Errorf("%s: %d bytes, which the host's default send buffer takes", c.name, k.largest)
		}
		rules := 0
		for _, ch := range c.chains {
			rules += len(ch.rules)
		}
		if c.table == FirewallTable && rules < driver.MaxFirewallRules {
			t.Errorf("%s: %d rules, fewer than the bound %d", c.name, rules, driver.MaxFirewallRules)
		}
		t.Logf("%s: %d rules, %d bytes", c.name, rules, k.largest)
	}
}

// Any other error the write's replies bring fails it as it is, though the
// table be as wanted: the fake commits the batch it reports refused, so
// that a write rereading the table on any error would pass.
func TestEnsureTableFailsAWriteTheKernelRefused(t *testing.T) {
	k := &fakeKernel{commits: true, lost: syscall.EINVAL}
	conn, err := nftables.New(nftables.WithTestDial(k.dial(t)))
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureNamedTable(conn, nftables.TableFamilyBridge, "t", testChains(1)); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("got %v, want EINVAL", err)
	}
}
