package node

import (
	"maps"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// A removed client's cut waits at most revokeBound for its server to take
// an export without it, then revokeGrace more (the clients' timeo=50), for
// its next operation to get the server's refusal.
const (
	revokeBound = 10 * time.Second
	revokeGrace = 5 * time.Second
)

// revocations hold back the firewall's cut of a client removed from a
// share while its server tells it so (docs/decisions/file-shares.md); its
// own lock, as the firewall is written outside the host lock.
type revocations struct {
	mu sync.Mutex
	// held is, by server machine uid, the boot and clients of the last
	// export the server took; none once it refused one.
	held map[string]heldExport
	// cuts is, by server machine uid and client address, a removal's cut.
	cuts map[string]map[netip.Addr]cut
	// nets and owners are, by server machine uid, its networks, and the
	// interface (machine uid/name) at each of its clients' addresses.
	nets   map[string][]controller.Key
	owners map[string]map[netip.Addr]string
}

type heldExport struct {
	boot    time.Time
	clients []netip.Addr
}

// cut is when the firewall cuts a removed client, the interface owner,
// still answered by the server's boot; taken, once that boot took an
// export without it. Past at, it stays cut until the server holds it again.
type cut struct {
	at    time.Time
	taken bool
	boot  time.Time
	owner string
}

func newRevocations() *revocations {
	return &revocations{held: map[string]heldExport{}, cuts: map[string]map[netip.Addr]cut{},
		nets: map[string][]controller.Key{}, owners: map[string]map[netip.Addr]string{}}
}

// on records a server's networks and its clients' interfaces by address.
func (r *revocations) on(server string, nets []controller.Key, clients map[netip.Addr]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nets[server] = nets
	owners := r.owners[server]
	if owners == nil {
		owners = map[netip.Addr]string{}
		r.owners[server] = owners
	}
	maps.Copy(owners, clients)
	for c := range owners {
		if _, cut := r.cuts[server][c]; !cut && clients[c] == "" && !slices.Contains(r.held[server].clients, c) {
			delete(owners, c)
		}
	}
}

// forget drops what is known of a server gone.
func (r *revocations) forget(server string) []controller.Key {
	keys := r.refused(server)
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.nets, server)
	delete(r.owners, server)
	return keys
}

// took records the export a server's boot took at now: a client it holds
// again is no cut's, and one its last export in this boot held, and this
// one does not, is cut revokeGrace on; the networks returned to look then.
func (r *revocations) took(server string, boot time.Time, e driver.ShareExport, now time.Time) []controller.Key {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev := r.held[server]
	h := heldExport{boot: boot}
	for _, c := range e.Clients {
		h.clients = append(h.clients, c.Address)
	}
	r.held[server] = h
	cuts := r.cuts[server]
	for _, c := range h.clients {
		delete(cuts, c)
	}
	dropped := false
	for _, c := range prev.clients {
		ct, seen := cuts[c]
		switch {
		case !prev.boot.Equal(boot), slices.Contains(h.clients, c), ct.taken:
			continue
		case seen && !now.Before(ct.at):
			// Its bound passed: cut already, and for good.
			delete(cuts, c)
			continue
		}
		if cuts == nil {
			cuts = map[netip.Addr]cut{}
			r.cuts[server] = cuts
		}
		cuts[c] = cut{at: now.Add(revokeGrace), taken: true, boot: boot, owner: r.owners[server][c]}
		dropped = true
	}
	if !dropped {
		return nil
	}
	return r.nets[server]
}

// refused forgets what a server holds, and cuts its removed clients now,
// as it refused an export or is gone; the networks returned to write.
func (r *revocations) refused(server string) []controller.Key {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.held, server)
	if _, ok := r.cuts[server]; !ok {
		return nil
	}
	delete(r.cuts, server)
	return r.nets[server]
}

// lingering returns the clients in inNet, network k's, still let reach
// server though the share no longer has them (want), while its running
// boot (zero if none) answers them and owners (by address) are unchanged.
func (r *revocations) lingering(k controller.Key, server string, boot time.Time, inNet netip.Prefix, want []netip.Addr,
	owners map[netip.Addr]string, now time.Time, schedule func(controller.Key, time.Duration)) []netip.Addr {
	r.mu.Lock()
	defer r.mu.Unlock()
	held := r.held[server]
	cuts := r.cuts[server]
	if cuts == nil {
		cuts = map[netip.Addr]cut{}
	}
	for _, c := range held.clients {
		if _, seen := cuts[c]; seen || !inNet.Contains(c) || slices.Contains(want, c) {
			continue
		}
		cuts[c] = cut{at: now.Add(revokeBound), boot: held.boot, owner: owners[c]}
		schedule(k, revokeBound)
	}
	var out []netip.Addr
	for c, ct := range cuts {
		if !inNet.Contains(c) {
			continue
		}
		if ct.owner == "" {
			ct.owner = owners[c]
			cuts[c] = ct
		}
		switch {
		case slices.Contains(want, c), ct.taken && !now.Before(ct.at),
			!ct.taken && !slices.Contains(held.clients, c):
			// Taken back, its grace over, or no longer held.
			delete(cuts, c)
		case now.Before(ct.at) && !boot.IsZero() && ct.boot.Equal(boot) && owners[c] == ct.owner:
			out = append(out, c)
		}
	}
	if len(cuts) == 0 {
		delete(r.cuts, server)
	} else {
		r.cuts[server] = cuts
	}
	slices.SortFunc(out, func(a, b netip.Addr) int { return a.Compare(b) })
	return out
}
