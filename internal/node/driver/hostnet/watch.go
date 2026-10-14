package hostnet

import (
	"net"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/google/nftables"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// watch follows one namespace and signals Changes when something the driver
// made there may have changed; docs/decisions/networking.md says what counts.
type watch struct {
	// stop is closed when the driver stops the watch; closing subs ends the
	// subscriptions, at a stop or when a test breaks them.
	stop, subs chan struct{}
	end        sync.Once
	mon        *nftables.Monitor
	// ns is the inode of the namespace watched, to tell it from another
	// mounted under the same name since.
	ns uint64
	// dead is set when a subscription failed; the namespace is watched
	// again at the next ensure.
	dead atomic.Bool
}

// endSubs ends the watch's subscriptions; netlink's close their channels.
func (w *watch) endSubs() {
	w.end.Do(func() {
		close(w.subs)
		if w.mon != nil {
			_ = w.mon.Close()
		}
	})
}

// subscribeNS, set by testbed builds only, gives a subscription another
// namespace to subscribe in, as a test of one that fails.
var subscribeNS atomic.Pointer[func(what string) (netns.NsHandle, bool)]

func (d *Driver) signal() {
	select {
	case d.changes <- struct{}{}:
	default:
	}
}

func (d *Driver) stopWatch(uid string) {
	if w := d.watches[uid]; w != nil {
		// stop first: the subscriptions' error callbacks then know it was
		// meant.
		close(w.stop)
		w.endSubs()
		delete(d.watches, uid)
	}
}

// rewatch makes sure the namespace of network uid, or the root namespace
// for "", is watched as it is mounted now.
func (d *Driver) rewatch(uid string) error {
	var ns *netns.NsHandle
	var id uint64
	if uid != "" {
		h, ok, err := openNS(uid)
		if err != nil || !ok {
			d.stopWatch(uid)
			return err
		}
		defer func() { _ = h.Close() }()
		if id, err = nsID(h); err != nil {
			return err
		}
		ns = &h
	}
	if w := d.watches[uid]; w != nil && !w.dead.Load() && w.ns == id {
		return nil
	}
	d.stopWatch(uid)
	w, err := d.startWatch(ns)
	if err != nil {
		return err
	}
	w.ns = id
	d.watches[uid] = w
	return nil
}

// startWatch subscribes to ns, or to the root namespace if ns is nil.
func (d *Driver) startWatch(ns *netns.NsHandle) (*watch, error) {
	w := &watch{stop: make(chan struct{}), subs: make(chan struct{})}
	fail := func(error) {
		select {
		case <-w.stop:
			return
		default:
		}
		w.dead.Store(true)
		d.signal()
	}
	at := func(what string) *netns.NsHandle {
		if f := subscribeNS.Load(); f != nil {
			if h, ok := (*f)(what); ok {
				return &h
			}
		}
		return ns
	}
	links := make(chan netlink.LinkUpdate, 64)
	addrs := make(chan netlink.AddrUpdate, 64)
	routes := make(chan netlink.RouteUpdate, 64)
	lister := d.root
	if ns != nil {
		h, err := netlink.NewHandleAt(*ns)
		if err != nil {
			return nil, err
		}
		defer h.Close()
		lister = h
	}
	all, err := lister.LinkList()
	if err != nil {
		return nil, err
	}
	// seen is each link as last seen, kept after it goes for the routes and
	// addresses that go with it.
	seen := map[int]linkState{}
	for _, l := range all {
		seen[l.Attrs().Index] = stateOf(l)
	}
	// A subscription that failed to start never closes its channel, so only
	// those that started are drained.
	var started []func()
	err = netlink.LinkSubscribeWithOptions(links, w.subs, netlink.LinkSubscribeOptions{Namespace: at("links"), ErrorCallback: fail})
	if err == nil {
		started = append(started, func() { drain(links) })
		err = netlink.AddrSubscribeWithOptions(addrs, w.subs, netlink.AddrSubscribeOptions{Namespace: at("addrs"), ErrorCallback: fail})
	}
	if err == nil {
		started = append(started, func() { drain(addrs) })
		err = netlink.RouteSubscribeWithOptions(routes, w.subs, netlink.RouteSubscribeOptions{Namespace: at("routes"), ErrorCallback: fail})
	}
	var opts []nftables.ConnOption
	if ns != nil {
		opts = append(opts, nftables.WithNetNSFd(int(*ns)))
	}
	var conn *nftables.Conn
	if err == nil {
		started = append(started, func() { drain(routes) })
		conn, err = nftables.New(opts...)
	}
	var tables chan *nftables.MonitorEvent
	if err == nil {
		w.mon = nftables.NewMonitor(nftables.WithMonitorObject(nftables.MonitorObjectTables | nftables.MonitorObjectChains | nftables.MonitorObjectRules))
		if tables, err = conn.AddMonitor(w.mon); err == nil {
			started = append(started, func() { drain(tables) })
		}
	}
	if err != nil {
		close(w.stop)
		w.endSubs()
		for _, f := range started {
			f()
		}
		return nil, err
	}

	// In the root namespace, only what is the driver's or the uplink's
	// matters. A link the watch does not know may be.
	ours := func(index int) bool {
		l, ok := seen[index]
		return ns != nil || !ok || strings.HasPrefix(l.name, Prefix) || l.name == d.cfg.Uplink
	}
	// Of IPv6, only the driver's links' matter, and only with a prefix:
	// without one, a link's IPv6 is the kernel's own, as the link-local
	// address it adds once a link is up, and the uplink's comes and goes
	// with the host's own configuration.
	ours6 := func(index int) bool {
		l, ok := seen[index]
		return d.cfg.IPv6Prefix.IsValid() && index != 0 && (ns != nil || !ok || strings.HasPrefix(l.name, Prefix))
	}
	go func() {
		defer func() {
			for _, f := range started {
				f()
			}
		}()
		for {
			var changed bool
			select {
			case <-w.stop:
				return
			// netlink calls fail before it closes a subscription's channel.
			case u, ok := <-links:
				if !ok {
					links = nil
					continue
				}
				// A bridge port's state comes in messages of its own, which
				// lack the rest of the link; its master is in the link's.
				if u.Family == unix.AF_BRIDGE {
					continue
				}
				// A link's carrier comes and goes on its own, as a tap's
				// with its VM: only what the driver sets is a change.
				idx, now := int(u.Index), stateOf(u.Link)
				before, known := seen[idx]
				seen[idx] = now
				changed = ours(idx) && (u.Header.Type == unix.RTM_DELLINK || !known || before != now)
			case u, ok := <-addrs:
				if !ok {
					addrs = nil
					continue
				}
				if u.LinkAddress.IP.To4() != nil {
					changed = ours(u.LinkIndex)
				} else {
					changed = ours6(u.LinkIndex)
				}
			case u, ok := <-routes:
				if !ok {
					routes = nil
					continue
				}
				changed = u.Family == unix.AF_INET && (u.LinkIndex == 0 || ours(u.LinkIndex)) ||
					u.Family == unix.AF_INET6 && ours6(u.LinkIndex)
			case e, ok := <-tables:
				if !ok {
					fail(nil)
					tables = nil
					continue
				}
				changed = e.Type == nftables.MonitorEventTypeOOB || ns != nil || isOurs(e.Data)
			}
			if changed {
				d.signal()
			}
		}
	}()
	return w, nil
}

// linkState is what of a link the driver sets or relies on.
type linkState struct {
	name   string
	up     bool
	master int
	alias  string
	mac    string
}

func stateOf(l netlink.Link) linkState {
	a := l.Attrs()
	return linkState{name: a.Name, up: a.Flags&net.FlagUp != 0, master: a.MasterIndex, alias: a.Alias, mac: a.HardwareAddr.String()}
}

// drain reads what a subscription still sends after its watch stops, so its
// goroutine ends once its socket closes.
func drain[T any](c chan T) {
	if c == nil { // closed already
		return
	}
	go func() {
		for range c {
		}
	}()
}
