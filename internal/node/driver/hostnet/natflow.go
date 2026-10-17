package hostnet

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mdlayher/netlink"
	"github.com/ti-mo/conntrack"
	"github.com/ti-mo/netfilter"
	vnl "github.com/vishvananda/netlink"
	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

var _ driver.NATFlows = (*Driver)(nil)

// eventBytes is what one conntrack event is taken to hold of the socket's
// receive buffer, the kernel's accounting included.
const eventBytes = 2 << 10

// The event socket's receive buffer holds every network's burst at once
// (newConnBurst), within these bounds.
const (
	minEventBuffer = 8 << 20
	maxEventBuffer = 128 << 20
)

// natQueueLen bounds the events read and not yet joined.
const natQueueLen = 4096

// eventsRead counts the conntrack events any FollowNAT has read, which the
// testbed checks the socket filter by.
var eventsRead atomic.Int64

// FollowNAT follows the host's conntrack events for new connections from
// networks, which markNetworkRule marks and markFilter alone lets through.
// The mark names the network; its own conntrack, asked by reply tuple,
// gives the machine. A flow's Time is when it was read: events carry none.
func (d *Driver) FollowNAT(ctx context.Context, h driver.NATHandler) error {
	c, err := netlink.Dial(unix.NETLINK_NETFILTER, nil)
	if err != nil {
		return fmt.Errorf("conntrack events: %w", err)
	}
	var once sync.Once
	closeConn := func() { once.Do(func() { _ = c.Close() }) }
	defer closeConn()
	// Joining loads conntrack's netlink module, and conntrack with it, if
	// nothing has yet; only then is its sysctl there.
	if err := c.JoinGroup(uint32(netfilter.GroupCTNew)); err != nil {
		return fmt.Errorf("conntrack events: %w", err)
	}
	if v, err := sysctl("net.netfilter.nf_conntrack_events"); err != nil {
		return fmt.Errorf("conntrack events: %w", err)
	} else if v == "0" {
		return errors.New("conntrack events are off on the host (net.netfilter.nf_conntrack_events)")
	}
	filter, err := markFilter()
	if err == nil {
		err = c.SetBPF(filter)
	}
	if err != nil {
		return fmt.Errorf("conntrack events' filter: %w", err)
	}
	j := d.newNATJoin()
	defer d.dropNATJoin(j)
	resize := func() {
		if uids, err := namespaces(); err == nil {
			_ = j.grow(c, eventBuffer(len(uids)))
		}
	}
	if uids, err := namespaces(); err != nil {
		return err
	} else if err := j.grow(c, eventBuffer(len(uids))); err != nil {
		return fmt.Errorf("conntrack events' buffer: %w", err)
	}

	q := newNATQueue(natQueueLen)
	errc := make(chan error, 1)
	var reading sync.WaitGroup
	reading.Go(func() { errc <- readEvents(c, q) })
	defer func() {
		closeConn()
		reading.Wait()
	}()
	h.Started(time.Now())
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errc:
			if ctx.Err() != nil {
				return nil
			}
			return err
		case it := <-q.items:
			f := it.flow
			uid, fresh := j.network(it.index)
			if fresh {
				resize()
			}
			f.NetworkUID = uid
			if uid != "" {
				f.Inner = j.inner(uid, it.query)
			}
			h.Flow(f)
		case <-tick.C:
		}
		// A loss goes after what was queued before it.
		if len(q.items) == 0 {
			if l, ok := q.takeLoss(time.Now()); ok {
				h.Lost(l)
			}
		}
	}
}

// readEvents reads c into q until c fails or is closed.
func readEvents(c *netlink.Conn, q *natQueue) error {
	for {
		msgs, err := c.Receive()
		now := time.Now()
		eventsRead.Add(int64(len(msgs)))
		if errors.Is(err, unix.ENOBUFS) {
			q.lose(now, "the host's conntrack events overflowed the agent's socket")
			continue
		}
		if err != nil {
			return fmt.Errorf("conntrack events: %w", err)
		}
		for _, m := range msgs {
			var ev conntrack.Event
			if ev.Unmarshal(m) != nil || ev.Type != conntrack.EventNew || ev.Flow == nil {
				continue
			}
			if f, query, index, ok := natFlowOf(*ev.Flow, now); ok {
				q.put(natItem{flow: f, query: query, index: index}, now)
			}
		}
	}
}

// transitRangePrefix is transitRange, parsed.
var transitRangePrefix = netip.MustParsePrefix(transitRange)

// natFlowOf is what a host conntrack entry says of a connection out of a
// network: the flow, the index of the veth it came by, and the query for
// the same connection in the network's conntrack, by its reply tuple there.
// ok is false for any entry but a marked one the host SNATed.
func natFlowOf(f conntrack.Flow, at time.Time) (flow driver.NATFlow, query conntrack.Flow, index int, ok bool) {
	o, r := f.TupleOrig, f.TupleReply
	src, dst := o.IP.SourceAddress, o.IP.DestinationAddress
	if f.Mark&natMarkTag == 0 || !src.Is4() || !transitRangePrefix.Contains(src) || !f.Status.SrcNAT() {
		return driver.NATFlow{}, conntrack.Flow{}, 0, false
	}
	index = int(f.Mark &^ natMarkTag)
	flow = driver.NATFlow{
		Time:        at,
		Protocol:    o.Proto.Protocol,
		Public:      netip.AddrPortFrom(r.IP.DestinationAddress, 0),
		Transit:     netip.AddrPortFrom(src, 0),
		Destination: netip.AddrPortFrom(dst, 0),
	}
	query.TupleReply.IP = conntrack.IPTuple{SourceAddress: dst, DestinationAddress: src}
	query.TupleReply.Proto.Protocol = o.Proto.Protocol
	switch o.Proto.Protocol {
	case unix.IPPROTO_ICMP:
		// The network's NAT maps an echo's identifier as a port.
		query.TupleReply.Proto.ICMPID = o.Proto.ICMPID
		query.TupleReply.Proto.ICMPType, query.TupleReply.Proto.ICMPCode = r.Proto.ICMPType, r.Proto.ICMPCode
	case unix.IPPROTO_TCP, unix.IPPROTO_UDP, unix.IPPROTO_UDPLITE, unix.IPPROTO_SCTP, unix.IPPROTO_DCCP:
		flow.Public = netip.AddrPortFrom(r.IP.DestinationAddress, r.Proto.DestinationPort)
		flow.Transit = netip.AddrPortFrom(src, o.Proto.SourcePort)
		flow.Destination = netip.AddrPortFrom(dst, o.Proto.DestinationPort)
		query.TupleReply.Proto.SourcePort, query.TupleReply.Proto.DestinationPort = o.Proto.DestinationPort, o.Proto.SourcePort
	default:
		// Without ports, the network's entry cannot be told from another
		// machine's to the same destination.
		return flow, conntrack.Flow{}, index, true
	}
	return flow, query, index, true
}

// natItem is a queued event.
type natItem struct {
	flow  driver.NATFlow
	query conntrack.Flow
	index int
}

// natQueue holds events read and not yet joined, and the window in which
// it or the socket dropped some.
type natQueue struct {
	items chan natItem

	mu     sync.Mutex
	last   time.Time // of the last event queued, or the start
	losing bool
	from   time.Time
	reason string
}

func newNATQueue(n int) *natQueue {
	return &natQueue{items: make(chan natItem, n), last: time.Now()}
}

// put queues it, or counts it lost when the queue is full.
func (q *natQueue) put(it natItem, now time.Time) {
	select {
	case q.items <- it:
		q.mu.Lock()
		q.last = now
		q.mu.Unlock()
	default:
		q.lose(now, "the agent fell behind the host's connections")
	}
}

// lose opens a loss window, from the last event queued, unless one is open.
func (q *natQueue) lose(_ time.Time, reason string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.losing {
		q.losing, q.from, q.reason = true, q.last, reason
	}
}

// takeLoss closes the open loss window at now, if there is one.
func (q *natQueue) takeLoss(now time.Time) (driver.NATLost, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.losing {
		return driver.NATLost{}, false
	}
	q.losing = false
	return driver.NATLost{From: q.from, To: now, Reason: q.reason}, true
}

// eventBuffer is the receive buffer for networks' events.
func eventBuffer(networks int) int {
	return min(max(networks*newConnBurst*eventBytes, minEventBuffer), maxEventBuffer)
}

// setReadBufferForce sets c's receive buffer past rmem_max, as root may.
func setReadBufferForce(c *netlink.Conn, n int) error {
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	err = rc.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUFFORCE, n)
	})
	return errors.Join(err, serr)
}

// ctGetter is a conntrack socket in a network's namespace.
type ctGetter interface {
	Get(conntrack.Flow) (conntrack.Flow, error)
	Close() error
}

// natJoin names each veth's network, and holds a conntrack socket in each
// network's namespace, known by the namespace's inode.
type natJoin struct {
	// linkUID is the network of the veth of index; dial opens a socket in
	// a network's namespace and says which; inode is the namespace's now.
	linkUID func(index int) (string, bool)
	dial    func(uid string) (ctGetter, uint64, error)
	inode   func(uid string) (uint64, bool)

	mu      sync.Mutex
	byIndex map[int]string
	conns   map[string]natConn
	// gone are the networks being deleted, whose namespaces no socket may
	// be opened in.
	gone   map[string]bool
	buffer int
}

type natConn struct {
	c  ctGetter
	ns uint64
}

func (d *Driver) newNATJoin() *natJoin {
	h, err := vnl.NewHandle()
	j := &natJoin{
		byIndex: map[int]string{}, conns: map[string]natConn{}, gone: map[string]bool{},
		linkUID: func(index int) (string, bool) {
			if err != nil {
				return "", false
			}
			l, err := h.LinkByIndex(index)
			if err != nil || !isVeth(l) {
				return "", false
			}
			return decodeVeth(l.Attrs().Alias)
		},
		dial: dialNamespace,
		inode: func(uid string) (uint64, bool) {
			ns, ok, err := openNS(uid)
			if err != nil || !ok {
				return 0, false
			}
			defer func() { _ = ns.Close() }()
			id, err := nsID(ns)
			return id, err == nil
		},
	}
	d.natMu.Lock()
	defer d.natMu.Unlock()
	if d.natJoins == nil {
		d.natJoins = map[*natJoin]func(){}
	}
	d.natJoins[j] = func() {
		if h != nil {
			h.Close()
		}
	}
	return j
}

func (d *Driver) dropNATJoin(j *natJoin) {
	d.natMu.Lock()
	release := d.natJoins[j]
	delete(d.natJoins, j)
	d.natMu.Unlock()
	j.close()
	if release != nil {
		release()
	}
}

// forgettingNAT runs del, the deletion of network uid, keeping FollowNAT
// from opening a socket in its namespace meanwhile, and closing what it
// holds there before and after. If del fails, the network is still there:
// it is joined again, rather than its connections logged unknown.
func (d *Driver) forgettingNAT(uid string, del func() error) error {
	d.forgetNAT(uid)
	if err := del(); err != nil {
		d.reviveNAT(uid)
		return err
	}
	// Again, now no socket can be opened in it: one opened since would
	// keep it alive.
	d.forgetNAT(uid)
	return nil
}

// reviveNAT lets FollowNAT open a socket in a network's namespace again,
// as it is made (again).
func (d *Driver) reviveNAT(uid string) {
	d.natMu.Lock()
	defer d.natMu.Unlock()
	for j := range d.natJoins {
		j.revive(uid)
	}
}

// forgetNAT closes what FollowNAT holds of a network being deleted, and
// keeps it from opening anything there until the network is made again.
func (d *Driver) forgetNAT(uid string) {
	d.natMu.Lock()
	defer d.natMu.Unlock()
	for j := range d.natJoins {
		j.forget(uid)
	}
}

func dialNamespace(uid string) (ctGetter, uint64, error) {
	ns, ok, err := openNS(uid)
	if err != nil || !ok {
		return nil, 0, errors.Join(err, fmt.Errorf("network %s has no namespace", uid))
	}
	defer func() { _ = ns.Close() }()
	id, err := nsID(ns)
	if err != nil {
		return nil, 0, err
	}
	c, err := conntrack.Dial(&netlink.Config{NetNS: int(ns)})
	if err != nil {
		return nil, 0, err
	}
	return c, id, nil
}

func (j *natJoin) grow(c *netlink.Conn, n int) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if n <= j.buffer {
		return nil
	}
	if err := setReadBufferForce(c, n); err != nil {
		return err
	}
	j.buffer = n
	return nil
}

// network is the uid of the network whose veth has index; fresh says it
// was not known before.
func (j *natJoin) network(index int) (uid string, fresh bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if uid, ok := j.byIndex[index]; ok {
		return uid, false
	}
	uid, ok := j.linkUID(index)
	if !ok {
		return "", false
	}
	j.byIndex[index] = uid
	// A new veth for a network known before: its namespace may be new too.
	j.dropLocked(uid)
	return uid, true
}

// inner is the machine's source of the connection q is the reply tuple of,
// in network uid's conntrack; invalid if it has none.
func (j *natJoin) inner(uid string, q conntrack.Flow) netip.AddrPort {
	if !q.TupleReply.IP.SourceAddress.IsValid() {
		return netip.AddrPort{}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for try := 0; try < 2; try++ {
		c, err := j.connLocked(uid)
		if err != nil {
			return netip.AddrPort{}
		}
		got, err := c.c.Get(q)
		if err == nil {
			o := got.TupleOrig
			if o.Proto.Protocol == unix.IPPROTO_ICMP {
				return netip.AddrPortFrom(o.IP.SourceAddress, 0)
			}
			return netip.AddrPortFrom(o.IP.SourceAddress, o.Proto.SourcePort)
		}
		// Not there: asked in the namespace mounted now, it is not there
		// at all. Anything else: the socket may be broken.
		if errors.Is(err, unix.ENOENT) {
			if now, ok := j.inode(uid); ok && now == c.ns {
				return netip.AddrPort{}
			}
		}
		j.dropLocked(uid)
	}
	return netip.AddrPort{}
}

func (j *natJoin) connLocked(uid string) (natConn, error) {
	if c, ok := j.conns[uid]; ok {
		return c, nil
	}
	if j.gone[uid] {
		return natConn{}, fmt.Errorf("network %s is being deleted", uid)
	}
	c, ns, err := j.dial(uid)
	if err != nil {
		return natConn{}, err
	}
	j.conns[uid] = natConn{c: c, ns: ns}
	return j.conns[uid], nil
}

func (j *natJoin) dropLocked(uid string) {
	if c, ok := j.conns[uid]; ok {
		_ = c.c.Close()
		delete(j.conns, uid)
	}
}

// forget closes the socket in a deleted network's namespace, which would
// keep the namespace alive, and forgets its veths.
func (j *natJoin) forget(uid string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.gone[uid] = true
	j.dropLocked(uid)
	for index, u := range j.byIndex {
		if u == uid {
			delete(j.byIndex, index)
		}
	}
}

func (j *natJoin) revive(uid string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.gone, uid)
}

func (j *natJoin) close() {
	j.mu.Lock()
	defer j.mu.Unlock()
	for uid := range j.conns {
		j.dropLocked(uid)
	}
}

// ctaMark is the conntrack attribute holding the mark.
const ctaMark = 8

// markFilter is markProgram, assembled.
func markFilter() ([]bpf.RawInstruction, error) {
	p, err := markProgram()
	if err != nil {
		return nil, err
	}
	return bpf.Assemble(p)
}

// markProgram passes a conntrack event whose mark has natMarkTag, walking
// its attributes unrolled, as classic BPF cannot loop. Netlink's lengths
// and types are little-endian here; the mark is big-endian.
func markProgram() ([]bpf.Instruction, error) {
	if !littleEndian() {
		return nil, errors.New("the conntrack event filter assumes a little-endian host")
	}
	const (
		headers  = unix.NLMSG_HDRLEN + 4 // nlmsghdr, nfgenmsg
		maxAttrs = 32
		cursor   = 0 // scratch: the attribute's offset
		hi       = 1 // scratch: its length's high byte, shifted
	)
	var p []bpf.Instruction
	var toFound, toDrop []int
	jump := func(to *[]int) {
		*to = append(*to, len(p))
		p = append(p, bpf.Jump{})
	}
	p = append(p, bpf.LoadConstant{Dst: bpf.RegA, Val: headers}, bpf.StoreScratch{Src: bpf.RegA, N: cursor})
	for range maxAttrs {
		p = append(p,
			bpf.LoadScratch{Dst: bpf.RegX, N: cursor},
			bpf.LoadIndirect{Off: 2, Size: 1}, // type, low byte
			bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: ctaMark, SkipTrue: 3},
			bpf.LoadIndirect{Off: 3, Size: 1}, // type, high byte
			bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: 0, SkipTrue: 1},
		)
		jump(&toFound)
		p = append(p,
			bpf.LoadIndirect{Off: 1, Size: 1}, // length, high byte
			bpf.ALUOpConstant{Op: bpf.ALUOpShiftLeft, Val: 8},
			bpf.StoreScratch{Src: bpf.RegA, N: hi},
			bpf.LoadIndirect{Off: 0, Size: 1}, // length, low byte
			bpf.LoadScratch{Dst: bpf.RegX, N: hi},
			bpf.ALUOpX{Op: bpf.ALUOpOr},
			bpf.JumpIf{Cond: bpf.JumpLessThan, Val: 4, SkipFalse: 1},
		)
		jump(&toDrop)
		p = append(p,
			bpf.ALUOpConstant{Op: bpf.ALUOpAdd, Val: 3},
			bpf.ALUOpConstant{Op: bpf.ALUOpAnd, Val: ^uint32(3)},
			bpf.LoadScratch{Dst: bpf.RegX, N: cursor},
			bpf.ALUOpX{Op: bpf.ALUOpAdd},
			bpf.StoreScratch{Src: bpf.RegA, N: cursor},
		)
	}
	drop := len(p)
	p = append(p, bpf.RetConstant{Val: 0})
	found := len(p)
	p = append(p,
		bpf.LoadScratch{Dst: bpf.RegX, N: cursor},
		bpf.LoadIndirect{Off: 4, Size: 4}, // the mark
		bpf.ALUOpConstant{Op: bpf.ALUOpAnd, Val: natMarkTag},
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: 0, SkipTrue: 1},
		bpf.RetConstant{Val: 0xffffffff},
		bpf.RetConstant{Val: 0},
	)
	for _, i := range toFound {
		p[i] = bpf.Jump{Skip: uint32(found - i - 1)}
	}
	for _, i := range toDrop {
		p[i] = bpf.Jump{Skip: uint32(drop - i - 1)}
	}
	return p, nil
}

func littleEndian() bool { return binary.NativeEndian.Uint16([]byte{1, 0}) == 1 }
