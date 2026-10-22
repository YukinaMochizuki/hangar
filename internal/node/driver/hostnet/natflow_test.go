package hostnet

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/mdlayher/netlink"
	"github.com/ti-mo/conntrack"
	"github.com/ti-mo/netfilter"
	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// vethIndex is the index of the veth the tests' connections come by.
const vethIndex = 42

// hostEntry is the host's conntrack entry of a connection from a network's
// transit address tsrc:tport to dst:dport, SNATed to public:pport and
// marked as the root table marks it.
func hostEntry(proto uint8, tsrc string, tport uint16, dst string, dport uint16, public string, pport uint16) conntrack.Flow {
	var f conntrack.Flow
	f.Status = conntrack.StatusSrcNAT | conntrack.StatusConfirmed
	f.Mark = natMarkTag | vethIndex
	f.TupleOrig.IP = conntrack.IPTuple{SourceAddress: netip.MustParseAddr(tsrc), DestinationAddress: netip.MustParseAddr(dst)}
	f.TupleOrig.Proto = conntrack.ProtoTuple{Protocol: proto, SourcePort: tport, DestinationPort: dport}
	f.TupleReply.IP = conntrack.IPTuple{SourceAddress: netip.MustParseAddr(dst), DestinationAddress: netip.MustParseAddr(public)}
	f.TupleReply.Proto = conntrack.ProtoTuple{Protocol: proto, SourcePort: dport, DestinationPort: pport}
	return f
}

// TestNATFlowOfAConnection: the host's entry gives the public source, the
// transit source, the destination and the veth, and the network's entry is
// asked for by its reply tuple: from the destination back to the transit
// source.
func TestNATFlowOfAConnection(t *testing.T) {
	at := time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC)
	for _, proto := range []uint8{unix.IPPROTO_TCP, unix.IPPROTO_UDP} {
		f, q, index, ok := natFlowOf(hostEntry(proto, "100.64.0.1", 40000, "198.51.100.7", 443, "192.0.2.1", 40123), at)
		if !ok || index != vethIndex {
			t.Fatalf("protocol %d: taken %t, veth %d; want a network's marked connection, by veth %d", proto, ok, index, vethIndex)
		}
		want := driver.NATFlow{
			Time: at, Protocol: proto,
			Public:      netip.MustParseAddrPort("192.0.2.1:40123"),
			Transit:     netip.MustParseAddrPort("100.64.0.1:40000"),
			Destination: netip.MustParseAddrPort("198.51.100.7:443"),
		}
		if f != want {
			t.Errorf("protocol %d: flow %+v, want %+v", proto, f, want)
		}
		r := q.TupleReply
		if r.IP.SourceAddress != netip.MustParseAddr("198.51.100.7") || r.IP.DestinationAddress != netip.MustParseAddr("100.64.0.1") ||
			r.Proto.Protocol != proto || r.Proto.SourcePort != 443 || r.Proto.DestinationPort != 40000 {
			t.Errorf("protocol %d: the network is asked for %+v, want the reply 198.51.100.7:443 to 100.64.0.1:40000", proto, r)
		}
	}
}

// TestNATFlowOfAPing: ICMP has no ports; the echo's identifier, which the
// network's NAT maps, finds its entry.
func TestNATFlowOfAPing(t *testing.T) {
	e := hostEntry(unix.IPPROTO_ICMP, "100.64.0.1", 0, "198.51.100.7", 0, "192.0.2.1", 0)
	e.TupleOrig.Proto.ICMPID, e.TupleOrig.Proto.ICMPType = 7, 8
	e.TupleReply.Proto.ICMPID, e.TupleReply.Proto.ICMPType = 9, 0
	f, q, _, ok := natFlowOf(e, time.Now())
	if !ok || f.Public != netip.MustParseAddrPort("192.0.2.1:0") || f.Destination != netip.MustParseAddrPort("198.51.100.7:0") {
		t.Fatalf("ping: %+v %t", f, ok)
	}
	if p := q.TupleReply.Proto; p.ICMPID != 7 || p.ICMPType != 0 || q.TupleReply.IP.DestinationAddress != netip.MustParseAddr("100.64.0.1") {
		t.Errorf("the network is asked for %+v, want the echo reply with the identifier the host saw, 7", q.TupleReply)
	}
}

// TestNATFlowOfAnotherProtocol: one without ports is reported, but the
// network is not asked: its entry is not one of a machine's.
func TestNATFlowOfAnotherProtocol(t *testing.T) {
	f, q, _, ok := natFlowOf(hostEntry(unix.IPPROTO_GRE, "100.64.0.1", 0, "198.51.100.7", 0, "192.0.2.1", 0), time.Now())
	if !ok || f.Protocol != unix.IPPROTO_GRE || f.Public.Addr() != netip.MustParseAddr("192.0.2.1") {
		t.Fatalf("GRE: %+v %t", f, ok)
	}
	if q.TupleReply.IP.SourceAddress.IsValid() {
		t.Errorf("the network is asked for %+v", q.TupleReply)
	}
}

// TestNATFlowOfSkips what is not a network's connection out of the host.
func TestNATFlowOfSkips(t *testing.T) {
	unmarked := hostEntry(unix.IPPROTO_TCP, "100.64.0.1", 5000, "198.51.100.7", 443, "192.0.2.1", 5000)
	unmarked.Mark = vethIndex
	frontDoor := hostEntry(unix.IPPROTO_TCP, "100.64.0.1", 5000, "192.0.2.1", 443, "100.64.0.1", 5000)
	frontDoor.Status = conntrack.StatusConfirmed
	v6 := hostEntry(unix.IPPROTO_TCP, "2001:db8::2", 5000, "2001:db8:1::7", 443, "2001:db8::2", 5000)
	outside := hostEntry(unix.IPPROTO_TCP, "100.128.0.1", 5000, "198.51.100.7", 443, "192.0.2.1", 5000)
	// A port forward's mark is not the NAT log's tag, whatever else the
	// entry looks like.
	forwarded := hostEntry(unix.IPPROTO_TCP, "100.64.0.1", 5000, "198.51.100.7", 443, "192.0.2.1", 5000)
	forwarded.Mark = forwardMark
	for name, e := range map[string]conntrack.Flow{
		"an unmarked": unmarked, "a network's to the front door": frontDoor, "IPv6": v6, "outside the transit range": outside,
		"a port forward's": forwarded,
	} {
		if f, _, _, ok := natFlowOf(e, time.Now()); ok {
			t.Errorf("%s connection is taken: %+v", name, f)
		}
	}
}

// event is the bytes of a conntrack NEW event with attrs.
func event(t *testing.T, attrs []netfilter.Attribute) (netlink.Message, []byte) {
	t.Helper()
	m, err := netfilter.MarshalNetlink(netfilter.Header{
		SubsystemID: netfilter.NFSubsysCTNetlink, MessageType: 0, Family: netfilter.ProtoIPv4,
		Flags: netlink.Create | netlink.Excl,
	}, attrs)
	if err != nil {
		t.Fatal(err)
	}
	m.Header.Length = uint32(unix.NLMSG_HDRLEN + len(m.Data))
	b, err := m.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return m, b
}

func be16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }
func be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

// tuple is a conntrack tuple attribute of type typ.
func tuple(typ uint16, src, dst string, sport, dport uint16) netfilter.Attribute {
	return netfilter.Attribute{Type: typ, Nested: true, Children: []netfilter.Attribute{
		{Type: 1, Nested: true, Children: []netfilter.Attribute{
			{Type: 1, Data: netip.MustParseAddr(src).AsSlice()}, {Type: 2, Data: netip.MustParseAddr(dst).AsSlice()},
		}},
		{Type: 2, Nested: true, Children: []netfilter.Attribute{
			{Type: 1, Data: []byte{unix.IPPROTO_TCP}}, {Type: 2, Data: be16(sport)}, {Type: 3, Data: be16(dport)},
		}},
	}}
}

// eventAttrs are a SNATed connection's, with the mark if it is not nil,
// after padding attributes as a real event has others.
func eventAttrs(mark *uint32, padding int) []netfilter.Attribute {
	attrs := []netfilter.Attribute{
		tuple(1, "100.64.0.1", "198.51.100.7", 40000, 443),
		tuple(2, "198.51.100.7", "192.0.2.1", 443, 40123),
		{Type: 3, Data: be32(uint32(conntrack.StatusSrcNAT | conntrack.StatusConfirmed))},
	}
	for i := range padding {
		attrs = append(attrs, netfilter.Attribute{Type: 22, Data: make([]byte, 1+i%5)}) // CTA_LABELS, of odd lengths
	}
	if mark != nil {
		attrs = append(attrs, netfilter.Attribute{Type: ctaMark, Data: be32(*mark)})
	}
	return attrs
}

// TestTheEventFilterPassesMarkedEvents: the socket filter passes an event
// whose mark has the tag, wherever the mark is among its attributes, and
// drops one without the tag, a port forward's among them, without a mark,
// or cut short.
func TestTheEventFilterPassesMarkedEvents(t *testing.T) {
	prog, err := markProgram()
	if err != nil {
		t.Fatal(err)
	}
	vm, err := bpf.NewVM(prog)
	if err != nil {
		t.Fatal(err)
	}
	tagged, untagged, forwarded := uint32(natMarkTag|vethIndex), uint32(vethIndex), uint32(forwardMark)
	for _, c := range []struct {
		name    string
		mark    *uint32
		padding int
		cut     int
		pass    bool
	}{
		{"tagged", &tagged, 0, 0, true},
		{"tagged after many attributes", &tagged, 20, 0, true},
		{"untagged", &untagged, 0, 0, false},
		{"a port forward's", &forwarded, 0, 0, false},
		{"without a mark", nil, 3, 0, false},
		{"cut in the mark", &tagged, 0, 2, false},
	} {
		m, b := event(t, eventAttrs(c.mark, c.padding))
		b = b[:len(b)-c.cut]
		n, err := vm.Run(b)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if (n > 0) != c.pass {
			t.Errorf("%s: kept %d bytes, want passed %t", c.name, n, c.pass)
		}
		// What passes is what natFlowOf takes.
		if c.cut == 0 {
			var ev conntrack.Event
			if err := ev.Unmarshal(m); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if _, _, _, ok := natFlowOf(*ev.Flow, time.Now()); ok != c.pass {
				t.Errorf("%s: natFlowOf takes it %t, the filter %t", c.name, ok, c.pass)
			}
		}
	}
}

// TestALossIsReportedOnce: a full queue drops events, and the window from
// the last one queued is reported once, when asked.
func TestALossIsReportedOnce(t *testing.T) {
	q := newNATQueue(2)
	t0 := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	for i := range 4 {
		q.put(natItem{index: i}, t0.Add(time.Duration(i)*time.Second))
	}
	if len(q.items) != 2 {
		t.Fatalf("%d queued, want 2", len(q.items))
	}
	l, ok := q.takeLoss(t0.Add(10 * time.Second))
	if !ok || !l.From.Equal(t0.Add(time.Second)) || !l.To.Equal(t0.Add(10*time.Second)) {
		t.Errorf("loss %+v %t, want from the last queued, 1s, to when asked, 10s", l, ok)
	}
	if _, ok := q.takeLoss(t0.Add(11 * time.Second)); ok {
		t.Error("the loss is reported twice")
	}
	q.lose(t0.Add(12*time.Second), "overflowed")
	if l, ok := q.takeLoss(t0.Add(13 * time.Second)); !ok || l.Reason != "overflowed" {
		t.Errorf("a socket's overflow is reported as %+v %t", l, ok)
	}
}

// fakeCT is a namespace's conntrack holding one connection.
type fakeCT struct {
	ns     uint64
	has    bool
	closed bool
}

func (c *fakeCT) Get(q conntrack.Flow) (conntrack.Flow, error) {
	if !c.has {
		return conntrack.Flow{}, unix.ENOENT
	}
	var f conntrack.Flow
	f.TupleOrig.IP.SourceAddress = netip.MustParseAddr("10.0.0.2")
	f.TupleOrig.Proto = conntrack.ProtoTuple{Protocol: unix.IPPROTO_TCP, SourcePort: 51000}
	return f, nil
}

func (c *fakeCT) Close() error { c.closed = true; return nil }

// joinHost is the host natJoin sees: veths by index, and each network's
// namespace by inode, with what is in its conntrack.
type joinHost struct {
	veths map[int]string
	ns    map[string]uint64
	has   map[string]bool
	open  []*fakeCT
}

func (h *joinHost) join() *natJoin {
	return &natJoin{
		byIndex: map[int]string{}, conns: map[string]natConn{}, gone: map[string]bool{},
		linkUID: func(index int) (string, bool) { uid, ok := h.veths[index]; return uid, ok },
		dial: func(uid string) (ctGetter, uint64, error) {
			ns, ok := h.ns[uid]
			if !ok {
				return nil, 0, errors.New("no namespace")
			}
			c := &fakeCT{ns: ns, has: h.has[uid]}
			h.open = append(h.open, c)
			return c, ns, nil
		},
		inode: func(uid string) (uint64, bool) { ns, ok := h.ns[uid]; return ns, ok },
	}
}

func (h *joinHost) openSockets() int {
	n := 0
	for _, c := range h.open {
		if !c.closed {
			n++
		}
	}
	return n
}

var replyQuery = func() conntrack.Flow {
	_, q, _, _ := natFlowOf(hostEntry(unix.IPPROTO_TCP, "100.64.0.1", 40000, "198.51.100.7", 443, "192.0.2.1", 40123), time.Now())
	return q
}()

// TestTheJoinFollowsNetworks: a network deleted and another made on its
// transit link, by a new veth, is told apart; a deleted network's socket is
// closed, and a namespace made again is asked anew.
func TestTheJoinFollowsNetworks(t *testing.T) {
	h := &joinHost{veths: map[int]string{1: "net-a"}, ns: map[string]uint64{"net-a": 100}, has: map[string]bool{"net-a": true}}
	j := h.join()
	if uid, _ := j.network(1); uid != "net-a" {
		t.Fatalf("veth 1 is %q, want net-a", uid)
	}
	if got := j.inner("net-a", replyQuery); got != netip.MustParseAddrPort("10.0.0.2:51000") {
		t.Fatalf("net-a's machine is %s", got)
	}

	// net-a goes, net-b comes on the same transit link: its veth is new.
	j.forget("net-a")
	delete(h.veths, 1)
	delete(h.ns, "net-a")
	h.veths[2], h.ns["net-b"], h.has["net-b"] = "net-b", 200, true
	if n := h.openSockets(); n != 0 {
		t.Errorf("%d sockets open after net-a was deleted, want its closed", n)
	}
	if uid, _ := j.network(2); uid != "net-b" {
		t.Errorf("a connection by the new veth is from %q, want net-b", uid)
	}
	if uid, _ := j.network(1); uid != "" {
		t.Errorf("a late event of net-a's is from %q, want no network", uid)
	}

	// net-b's namespace is made again under it: the old socket finds
	// nothing, and the new namespace is asked.
	if got := j.inner("net-b", replyQuery); !got.IsValid() {
		t.Fatal("net-b's machine not found")
	}
	h.ns["net-b"] = 201
	h.open[len(h.open)-1].has = false
	if got := j.inner("net-b", replyQuery); got != netip.MustParseAddrPort("10.0.0.2:51000") {
		t.Errorf("after net-b's namespace was made again, its machine is %s", got)
	}
	if n := h.openSockets(); n != 1 {
		t.Errorf("%d sockets open, want the new namespace's alone", n)
	}

	// Not there in the namespace as it is: unknown, the socket kept.
	h.has["net-b"] = false
	h.open[len(h.open)-1].has = false
	if got := j.inner("net-b", replyQuery); got.IsValid() {
		t.Errorf("a connection net-b does not know is from %s", got)
	}
	if n := h.openSockets(); n != 1 {
		t.Errorf("%d sockets open, want the one kept", n)
	}
	j.close()
	if n := h.openSockets(); n != 0 {
		t.Errorf("%d sockets open after close", n)
	}
}

// TestDeletionAndAJoinInterleaved: a socket being opened in a network's
// namespace as its deletion begins is closed by it, none is opened while
// the network is being deleted, and one is once the network is made again.
func TestDeletionAndAJoinInterleaved(t *testing.T) {
	h := &joinHost{veths: map[int]string{1: "net-a"}, ns: map[string]uint64{"net-a": 100}, has: map[string]bool{"net-a": true}}
	j := h.join()
	dial := j.dial
	dialing, release := make(chan struct{}), make(chan struct{})
	j.dial = func(uid string) (ctGetter, uint64, error) {
		close(dialing)
		<-release
		return dial(uid)
	}
	joined := make(chan netip.AddrPort)
	go func() { joined <- j.inner("net-a", replyQuery) }()
	<-dialing
	forgot := make(chan struct{})
	go func() { j.forget("net-a"); close(forgot) }()
	close(release)
	<-joined
	<-forgot
	if n := h.openSockets(); n != 0 {
		t.Errorf("%d sockets open after the deletion began during a dial, want none", n)
	}
	j.dial = dial
	if got := j.inner("net-a", replyQuery); got.IsValid() || h.openSockets() != 0 {
		t.Errorf("an event during the deletion opened a socket (%d) and joined %s", h.openSockets(), got)
	}
	j.revive("net-a")
	if got := j.inner("net-a", replyQuery); !got.IsValid() {
		t.Error("the network made again is not joined")
	}
}

// TestAFailedDeletionKeepsTheNetworkJoined: while a network is deleted its
// connections are not joined; if the deletion fails partway the network is
// still there and they are again, rather than logged as of no machine; if
// it succeeds, nothing is held there.
func TestAFailedDeletionKeepsTheNetworkJoined(t *testing.T) {
	h := &joinHost{veths: map[int]string{1: "net-a"}, ns: map[string]uint64{"net-a": 100}, has: map[string]bool{"net-a": true}}
	j := h.join()
	d := &Driver{natJoins: map[*natJoin]func(){j: func() {}}}
	if got := j.inner("net-a", replyQuery); !got.IsValid() {
		t.Fatal("net-a's machine not found")
	}
	err := d.forgettingNAT("net-a", func() error {
		if got := j.inner("net-a", replyQuery); got.IsValid() {
			t.Errorf("joined %s while net-a is deleted", got)
		}
		return errors.New("partway")
	})
	if err == nil {
		t.Fatal("the deletion's error is lost")
	}
	if got := j.inner("net-a", replyQuery); got != netip.MustParseAddrPort("10.0.0.2:51000") {
		t.Errorf("after a failed deletion net-a's machine is %s, want it joined again", got)
	}
	if err := d.forgettingNAT("net-a", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := j.inner("net-a", replyQuery); got.IsValid() || h.openSockets() != 0 {
		t.Errorf("after the deletion, joined %s with %d sockets open", got, h.openSockets())
	}
}

// TestTheEventBufferHoldsEveryBurst: the socket holds every network's
// burst at once, within bounds.
func TestTheEventBufferHoldsEveryBurst(t *testing.T) {
	if got := eventBuffer(0); got != minEventBuffer {
		t.Errorf("no network: %d, want the floor %d", got, minEventBuffer)
	}
	if got := eventBuffer(40); got < 40*newConnBurst*eventBytes {
		t.Errorf("40 networks: %d, less than their bursts", got)
	}
	if got := eventBuffer(1 << 20); got != maxEventBuffer {
		t.Errorf("a million networks: %d, want the ceiling %d", got, maxEventBuffer)
	}
}
