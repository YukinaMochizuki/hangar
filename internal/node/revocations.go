package node

import (
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// The firewall's cut of a client removed from a share waits at most
// revokeBound for the share's server to take an export without it, then
// revokeGrace more, for the client's next operation to get the server's
// refusal: clients mount with timeo=50, 5 s.
const (
	revokeBound = 10 * time.Second
	revokeGrace = 5 * time.Second
)

// revocations hold back the firewall's cut of a client removed from a
// share while its server tells it so (docs/decisions/file-shares.md). Its
// own lock: the firewall is written outside the host lock.
type revocations struct {
	mu sync.Mutex
	// held is, by server machine uid, the boot and the clients of the last
	// export the server took; none once it refused one.
	held map[string]heldExport
	// cuts is, by server machine uid and client address, a removal's cut.
	cuts map[string]map[netip.Addr]cut
	// nets are, by server machine uid, the networks it is on.
	nets map[string][]controller.Key
}

type heldExport struct {
	boot    time.Time
	clients []netip.Addr
}

// cut is when the firewall cuts a removed client still answered by the
// server's boot; taken, once that boot took an export without it.
type cut struct {
	at    time.Time
	taken bool
	boot  time.Time
}

func newRevocations() *revocations {
	return &revocations{held: map[string]heldExport{}, cuts: map[string]map[netip.Addr]cut{}, nets: map[string][]controller.Key{}}
}

// on records the networks a server is on.
func (r *revocations) on(server string, nets []controller.Key) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nets[server] = nets
}

// took records the export a server's boot took at now. A client its last
// export in the same boot held, and this one does not, is cut revokeGrace
// later: the networks returned are to be looked at again then.
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
	dropped := false
	for _, c := range prev.clients {
		if !prev.boot.Equal(boot) || slices.Contains(h.clients, c) || cuts[c].taken {
			continue
		}
		if cuts == nil {
			cuts = map[netip.Addr]cut{}
			r.cuts[server] = cuts
		}
		cuts[c] = cut{at: now.Add(revokeGrace), taken: true, boot: boot}
		dropped = true
	}
	if !dropped {
		return nil
	}
	return r.nets[server]
}

// refused forgets what a server holds, and cuts its removed clients now,
// as it refused an export or is gone; the networks returned are to be
// written again.
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

// lingering returns the clients in inNet, network k's, the firewall still
// lets reach server though the share no longer has them (want): those the
// server's running boot (zero if none) holds, or took an export without,
// until their cut. A removal first seen gets its cut, and schedule a look.
func (r *revocations) lingering(k controller.Key, server string, boot time.Time, inNet netip.Prefix, want []netip.Addr,
	now time.Time, schedule func(controller.Key, time.Duration)) []netip.Addr {
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
		cuts[c] = cut{at: now.Add(revokeBound), boot: held.boot}
		schedule(k, revokeBound)
	}
	var out []netip.Addr
	for c, ct := range cuts {
		if !inNet.Contains(c) {
			continue
		}
		switch {
		case slices.Contains(want, c), !now.Before(ct.at),
			!ct.taken && !slices.Contains(held.clients, c):
			// The share has it again, its time is up, or the server
			// holds it no more without having taken an export.
			delete(cuts, c)
		case !boot.IsZero() && ct.boot.Equal(boot):
			// Only the boot that holds it, or took the export without
			// it, answers it.
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
