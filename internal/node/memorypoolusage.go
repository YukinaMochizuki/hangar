package node

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/squeeze"
)

// poolReads are what the squeeze last read of each memory pool on the
// node, which MemoryPoolUsage answers: the squeeze reads each member's
// balloon anyway, so the answer asks no VM anything
// (docs/decisions/node-agent.md).
type poolReads struct {
	mu    sync.Mutex
	pools map[string]poolRead
}

// poolRead is one pool as a poll read it.
type poolRead struct {
	at      time.Time
	members map[string]memberRead
}

// memberRead is a member as a poll read it: what its guest has, if its
// balloon was read; what its guest uses, if it reported it lately; and
// when its VM was last killed whole, zero for never.
type memberRead struct {
	sizeMiB     int64
	known       bool
	inUseMiB    int64
	inUseKnown  bool
	lastOOMKill time.Time
}

// readOf is what a poll at at, with result r, says of p's members; oom are
// the VMs' last OOM kills, by machine uid.
func readOf(at time.Time, p *poolPlan, r squeeze.Result, oom map[string]time.Time) poolRead {
	unread := map[string]bool{}
	for _, uid := range r.Unread {
		unread[uid] = true
	}
	read := poolRead{at: at, members: map[string]memberRead{}}
	for _, m := range p.members {
		size, ok := r.Sizes[m.UID]
		inUse, used := r.InUse[m.UID]
		read.members[m.UID] = memberRead{sizeMiB: size, known: ok && !unread[m.UID], inUseMiB: inUse, inUseKnown: used, lastOOMKill: oom[m.UID]}
	}
	return read
}

// keep stores a pool's read.
func (k *poolReads) keep(name string, read poolRead) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.pools[name] = read
}

// forget drops the reads of the pools not in keep, as pools the node no
// longer runs.
func (k *poolReads) forget(keep map[string]*poolPlan) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for name := range k.pools {
		if _, ok := keep[name]; !ok {
			delete(k.pools, name)
		}
	}
}

// read is the pool's last read, and false when there is none.
func (k *poolReads) read(name string) (poolRead, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	r, ok := k.pools[name]
	return r, ok
}

// lastOOMKills are the VMs' last OOM kills, by machine uid, those never
// killed left out.
func lastOOMKills(vms []driver.VM) map[string]time.Time {
	out := map[string]time.Time{}
	for _, vm := range vms {
		if !vm.LastOOMKill.IsZero() {
			out[vm.MachineUID] = vm.LastOOMKill
		}
	}
	return out
}

// staleReadPolls is how many calm polls old a pool's last read may be and
// still be answered: a poll that fails keeps no read, so the last one ages.
const staleReadPolls = 3

// memoryPoolAnswer is what the node says of the pool: its last read, each
// member's balloon as the squeezer follows it now; nothing for a read older
// than staleReadPolls calm polls, as for none.
func (a *Agent) memoryPoolAnswer(pool string) *nodev1.MemoryPoolUsageResponse {
	res := &nodev1.MemoryPoolUsageResponse{}
	read, ok := a.poolReads.read(pool)
	if !ok || a.cfg.Clock.Now().Sub(read.at) > staleReadPolls*cmp.Or(a.cfg.SqueezePoll, DefaultSqueezePoll) {
		return res
	}
	res.SampleTime = timestamppb.New(read.at)
	for _, uid := range slices.Sorted(maps.Keys(read.members)) {
		m := read.members[uid]
		member := &nodev1.MemoryPoolMember{MachineUid: uid}
		if m.known {
			b := uint64(max(m.sizeMiB, 0)) << 20
			member.SizeBytes = &b
		}
		if m.inUseKnown {
			u := uint64(max(m.inUseMiB, 0)) << 20
			member.InUseBytes = &u
		}
		if since, stuck := a.squeezer.UnresponsiveSince(uid); stuck {
			member.BalloonUnresponsiveSince = timestamppb.New(since)
		}
		if !m.lastOOMKill.IsZero() {
			member.LastOomKillTime = timestamppb.New(m.lastOOMKill)
		}
		res.Members = append(res.Members, member)
	}
	return res
}

func (s nodeService) MemoryPoolUsage(_ context.Context, req *connect.Request[nodev1.MemoryPoolUsageRequest]) (*connect.Response[nodev1.MemoryPoolUsageResponse], error) {
	if s.a.cfg.Drivers == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this node runs no machines"))
	}
	pool := req.Msg.GetPool()
	if pool == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("pool is required"))
	}
	return connect.NewResponse(s.a.memoryPoolAnswer(pool)), nil
}
