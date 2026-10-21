//go:build testbed

package hostnet

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// BreakWatch ends the subscriptions of the watch on network networkUID, or on
// the root namespace for "", as a socket failing does; false if none runs.
func BreakWatch(d *Driver, networkUID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	w := d.watches[networkUID]
	if w == nil {
		return false
	}
	w.endSubs()
	return true
}

// BreakNftWatch ends the nft monitor of the watch on network networkUID, or
// on the root namespace for "", as its socket failing does.
func BreakNftWatch(d *Driver, networkUID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	w := d.watches[networkUID]
	if w == nil {
		return false
	}
	_ = w.mon.Close()
	return true
}

// EventsRead is how many conntrack events FollowNAT has read in this
// process, those its socket filter passed.
func EventsRead() int64 { return eventsRead.Load() }

// WriteFirewallOfOld writes a network's firewall as a driver did before
// file share servers had chains of their own: every tap closed but to the
// network's services and replies, a server's too.
func WriteFirewallOfOld(networkUID string) error {
	ns, ok, err := openNS(networkUID)
	if err != nil || !ok {
		return fmt.Errorf("network %s: %t %w", networkUID, ok, err)
	}
	defer func() { _ = ns.Close() }()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		return err
	}
	defer h.Close()
	gateway, err := gatewayOf(h)
	if err != nil {
		return err
	}
	chains, err := firewallChains(gateway, nil)
	if err != nil {
		return err
	}
	_, in, out := shareServerChains(nil)
	chains = slices.DeleteFunc(chains, func(c chainSpec) bool { return c.name == in || c.name == out })
	for i := range chains {
		chains[i].rules = slices.DeleteFunc(chains[i].rules, func(r ruleSpec) bool {
			return strings.HasPrefix(r.says, "a file share's server's taps")
		})
	}
	return writeFirewall(ns, chains)
}

// FailSubscriptions makes the named subscriptions ("links", "addrs" or
// "routes", or a network's "neighs") of watches started from now on fail;
// none makes them work again.
func FailSubscriptions(what ...string) error {
	if len(what) == 0 {
		subscribeNS.Store(nil)
		return nil
	}
	// A file that is not a namespace fails setns, so netlink's subscribe
	// fails as it does when the namespace cannot be entered.
	notNS, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	f := func(sub string) (netns.NsHandle, bool) {
		return netns.NsHandle(notNS.Fd()), slices.Contains(what, sub)
	}
	subscribeNS.Store(&f)
	return nil
}
