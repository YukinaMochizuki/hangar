package node

import (
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"

	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/internal/node/squeeze"
)

func memoryPoolUsage(t *testing.T, a *Agent, pool string) (*nodev1.MemoryPoolUsageResponse, error) {
	t.Helper()
	res, err := nodeService{a: a, agent: t.Context()}.MemoryPoolUsage(t.Context(), connect.NewRequest(&nodev1.MemoryPoolUsageRequest{Pool: pool}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func membersByUID(res *nodev1.MemoryPoolUsageResponse) map[string]*nodev1.MemoryPoolMember {
	out := map[string]*nodev1.MemoryPoolMember{}
	for _, m := range res.GetMembers() {
		out[m.GetMachineUid()] = m
	}
	return out
}

// TestMemoryPoolUsageAnswersTheLastSqueeze: a pool not squeezed yet
// answers nothing; once squeezed, each member as that poll read it, its
// memory less its balloon and what its guest uses of it, with neither when
// its balloon could not be read and no use when its guest's report is
// stale, and when its VM was last killed whole. A pool the node no longer
// runs is forgotten.
func TestMemoryPoolUsageAnswersTheLastSqueeze(t *testing.T) {
	t.Parallel()
	a, h := newPoolAgent(t)
	a.memoryShares = map[string]memoryShare{"a": share("a", "acme", 4096, 4096, 1024), "s": share("s", "acme", 4096, 4096, 1024)}
	res, err := memoryPoolUsage(t, a, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if res.GetSampleTime() != nil || len(res.GetMembers()) != 0 {
		t.Errorf("before a squeeze: %v, want nothing", res)
	}

	killed := a.cfg.Clock.Now().Add(-30 * time.Minute)
	h.vms["s"].lastOOMKill = killed
	h.vms["a"].unreadable = true
	a.squeeze(t.Context(), false)
	res, err = memoryPoolUsage(t, a, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if !res.GetSampleTime().AsTime().Equal(a.cfg.Clock.Now()) {
		t.Errorf("sampled at %v, want %v", res.GetSampleTime().AsTime(), a.cfg.Clock.Now())
	}
	m := membersByUID(res)
	if len(m) != 2 || m["a"].SizeBytes != nil || m["a"].InUseBytes != nil || m["a"].GetLastOomKillTime() != nil {
		t.Errorf("a, its balloon unread: %v, want no size, no use and no kill", m["a"])
	}
	if m["s"].GetSizeBytes() != 2048*mib || m["s"].InUseBytes == nil || m["s"].GetInUseBytes() != 100*mib || !m["s"].GetLastOomKillTime().AsTime().Equal(killed) || m["s"].GetBalloonUnresponsiveSince() != nil {
		t.Errorf("s: %v, want 2048 MiB using 100, killed at %v, its balloon following", m["s"], killed)
	}

	// A guest whose last report is older than the squeeze reads says
	// nothing of its use, though its size is known.
	h.vms["s"].staleStats = true
	a.squeeze(t.Context(), false)
	res, err = memoryPoolUsage(t, a, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if s := membersByUID(res)["s"]; s.SizeBytes == nil || s.InUseBytes != nil {
		t.Errorf("s, its report stale: %v, want a size and no use", s)
	}
	if other, err := memoryPoolUsage(t, a, "other"); err != nil || other.GetSampleTime() != nil || len(other.GetMembers()) != 0 {
		t.Errorf("a pool not on the node: %v, %v; want nothing", other, err)
	}

	clear(h.vms)
	a.squeeze(t.Context(), false)
	if res, err := memoryPoolUsage(t, a, "acme"); err != nil || res.GetSampleTime() != nil {
		t.Errorf("its VMs gone: %v, %v; want nothing", res, err)
	}
	if _, err := memoryPoolUsage(t, a, ""); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("no pool named: %v, want InvalidArgument", err)
	}
}

// TestMemoryPoolUsageSaysSinceWhenABalloonIsUnresponsive: a member whose
// balloon stopped following says since when, no earlier than
// squeeze.StuckFor after it was first asked.
func TestMemoryPoolUsageSaysSinceWhenABalloonIsUnresponsive(t *testing.T) {
	t.Parallel()
	a, h := newPoolAgent(t)
	clock := a.cfg.Clock.(*clockwork.FakeClock)
	start := clock.Now()
	h.vms["s"].stuck = true
	a.memoryShares = map[string]memoryShare{"a": share("a", "acme", 2048, 4096, 1024), "s": share("s", "acme", 2048, 4096, 1024)}
	for range 10 {
		a.squeeze(t.Context(), false)
		clock.Advance(10 * time.Second)
	}
	res, err := memoryPoolUsage(t, a, "acme")
	if err != nil {
		t.Fatal(err)
	}
	m := membersByUID(res)
	since := m["s"].GetBalloonUnresponsiveSince()
	if since == nil || since.AsTime().Before(start.Add(squeeze.StuckFor)) || since.AsTime().After(clock.Now()) {
		t.Errorf("s unresponsive since %v, want between %v and %v", since, start.Add(squeeze.StuckFor), clock.Now())
	}
	if m["a"].GetBalloonUnresponsiveSince() != nil {
		t.Errorf("a's balloon follows, but %v", m["a"].GetBalloonUnresponsiveSince())
	}
}
