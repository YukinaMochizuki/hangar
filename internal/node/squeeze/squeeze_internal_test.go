package squeeze

import (
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// sharedVM is a Shared member of 4096 guaranteed 1024 whose guest has
// size MiB, uses inUse of it and has avail available, its unit holding
// use, its guest reporting its reclaims.
func sharedVM(uid string, size, use, inUse, avail int64) state {
	return state{
		Member: Member{UID: uid, MemoryMiB: 4096, GuaranteedMiB: 1024}, now: longAfter, useMiB: use, balloonMiB: 4096 - size,
		stats: true, inUseMiB: inUse, availableMiB: avail, counters: true,
	}
}

// longAfter is a time long after its members' VMs started.
var longAfter = time.Unix(1_000_000_000, 0)

// dedicatedVM is a member guaranteed all its memory, holding use.
func dedicatedVM(uid string, memory, use int64) state {
	return state{Member: Member{UID: uid, MemoryMiB: memory, GuaranteedMiB: memory}, useMiB: use}
}

func check(t *testing.T, r Result, wantStage Stage, want map[string]int64) {
	t.Helper()
	if r.Stage != wantStage || !maps.Equal(r.Balloons, want) {
		t.Errorf("got %s %v (allowances %v), want %s %v", r.Stage, r.Balloons, r.Allowances, wantStage, want)
	}
}

// TestAGuestNeedsMoreWhenItIsShort: a guest needs more with little of its
// size available or its reclaims moving; one that reports neither, or no
// statistics at all, once its unit holds 90% of its size; never one whose
// balloon cannot be set.
func TestAGuestNeedsMoreWhenItIsShort(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		m    state
		want bool
	}{
		"plenty available":           {sharedVM("a", 2048, 2000, 300, 1700), false},
		"little available":           {sharedVM("a", 2048, 2000, 1900, 200), true},
		"reclaiming":                 {func() state { m := sharedVM("a", 2048, 2000, 300, 1700); m.reclaimed = true; return m }(), true},
		"no counters, its unit full": {func() state { m := sharedVM("a", 2048, 1900, 300, 1700); m.counters = false; return m }(), true},
		"no counters, room":          {func() state { m := sharedVM("a", 2048, 1500, 300, 1700); m.counters = false; return m }(), false},
		"no statistics, full":        {func() state { m := sharedVM("a", 2048, 1900, 0, 0); m.stats = false; return m }(), true},
		"no statistics, room":        {func() state { m := sharedVM("a", 2048, 1000, 0, 0); m.stats = false; return m }(), false},
		"no balloon":                 {func() state { m := sharedVM("a", 4096, 4000, 0, 0); m.stats, m.noBalloon = false, true; return m }(), false},
	} {
		if got := c.m.needs(); got != c.want {
			t.Errorf("%s: needs %t, want %t", name, got, c.want)
		}
	}
}

// pool is a pool of 5120 with a Dedicated member of 2048, idle at 300,
// and two Shared ones.
func pool(a, c state) []state { return []state{a, c, dedicatedVM("d", 2048, 300)} }

// TestNobodyNeedingNobodyGivesBack: with no guest in need, each keeps its
// size, the Dedicated member's whole memory held for it whatever it uses.
func TestNobodyNeedingNobodyGivesBack(t *testing.T) {
	t.Parallel()
	r := decide(5120, pool(sharedVM("a", 2048, 2000, 200, 1800), sharedVM("c", 1024, 900, 100, 900)), StepMiB)
	check(t, r, Hold, map[string]int64{})
	if want := map[string]int64{"a": 2048, "c": 1024}; !maps.Equal(r.Allowances, want) || r.Hot {
		t.Errorf("allowances %v, hot %t; want %v, not hot", r.Allowances, r.Hot, want)
	}
}

// TestWhatIsFreeIsHandedOut: with nobody in need, what the Shared guests
// do not have of what the Dedicated memory leaves is given them, by how
// far each could grow, at once: no step holds a balloon shrinking.
func TestWhatIsFreeIsHandedOut(t *testing.T) {
	t.Parallel()
	r := decide(5120, pool(sharedVM("a", 1024, 900, 100, 900), sharedVM("c", 1024, 900, 100, 900)), StepMiB)
	check(t, r, Deflate, map[string]int64{"a": 2560, "c": 2560})
	// More than a step at once.
	r = decide(8192, pool(sharedVM("a", 1024, 900, 100, 900), sharedVM("c", 1024, 900, 100, 900)), StepMiB)
	check(t, r, Deflate, map[string]int64{"a": 1024, "c": 1024})
}

// TestAGuestInNeedIsGivenWhatTheOthersFree: a guest in need is given what
// the others have past their floors, they shrinking by a step a poll and
// it growing only into what their balloons have freed, a poll later.
func TestAGuestInNeedIsGivenWhatTheOthersFree(t *testing.T) {
	t.Parallel()
	a := sharedVM("a", 1024, 1000, 700, 100)
	c := sharedVM("c", 2048, 2000, 100, 1900)
	r := decide(5120, pool(a, c), StepMiB)
	check(t, r, Safe, map[string]int64{"c": 2560})
	if want := map[string]int64{"a": 2048, "c": 1024}; !maps.Equal(r.Allowances, want) || !slices.Equal(r.Needing, []string{"a"}) || !r.Hot {
		t.Errorf("allowances %v, needing %v, hot %t; want %v, a, hot", r.Allowances, r.Needing, r.Hot, want)
	}
	// Under pressure, a step of 1024.
	check(t, decide(5120, pool(a, c), PressureStepMiB), Safe, map[string]int64{"c": 3072})
	// C's balloon at 2560: 512 freed, all A's at once, A's size and C's
	// as it is and the Dedicated memory together the pool's.
	c.balloonMiB = 2560
	check(t, decide(5120, pool(a, c), StepMiB), Safe, map[string]int64{"a": 2560, "c": 3072})
	if sum := (4096 - 2560) + c.size() + 2048; sum != 5120 {
		t.Errorf("sizes after add up to %d, want 5120", sum)
	}
}

// TestAShortPoolTakesTowardTheGuarantees: floors past what the Dedicated
// memory leaves are cut by how far each is above its guarantee, never
// below it: the guarantee stage.
func TestAShortPoolTakesTowardTheGuarantees(t *testing.T) {
	t.Parallel()
	// Floors 2209 and 1909, 1046 past 3072: cuts of 599 and 448.
	r := decide(5120, pool(sharedVM("a", 2560, 2500, 1800, 700), sharedVM("c", 2048, 2000, 1500, 500)), StepMiB)
	check(t, r, Guarantee, map[string]int64{"a": 2048, "c": 2560})
	if want := map[string]int64{"a": 1610, "c": 1461}; !maps.Equal(r.Allowances, want) {
		t.Errorf("allowances %v, want %v", r.Allowances, want)
	}
	r = decide(5120, pool(sharedVM("a", 4096, 4000, 4000, 50), sharedVM("c", 4096, 4000, 4000, 50)), StepMiB)
	if want := map[string]int64{"a": 1536, "c": 1536}; !maps.Equal(r.Allowances, want) {
		t.Errorf("both past: allowances %v, want %v", r.Allowances, want)
	}
}

// TestAGuestGrowsOnlyIntoRoomFreed: N started at its guarantee into a
// pool C had filled, past its size together: N and A wait, owed their
// shares, while C's balloon takes its own back a step at a time.
func TestAGuestGrowsOnlyIntoRoomFreed(t *testing.T) {
	t.Parallel()
	a := sharedVM("a", 1024, 400, 300, 700)
	c := sharedVM("c", 3072, 3000, 300, 2700)
	n := sharedVM("n", 1024, 200, 100, 900)
	r := decide(5120, []state{a, c, n, dedicatedVM("d", 1024, 300)}, StepMiB)
	check(t, r, Safe, map[string]int64{"c": 1536})
	if want := map[string]int64{"a": 1365, "c": 1365, "n": 1365}; !maps.Equal(r.Allowances, want) || !r.Hot {
		t.Errorf("allowances %v, hot %t; want %v, hot", r.Allowances, r.Hot, want)
	}
}

func TestADedicatedMachinesBalloonIsGivenBack(t *testing.T) {
	t.Parallel()
	d := dedicatedVM("d", 2048, 300)
	d.balloonMiB = 1000
	check(t, decide(4096, []state{sharedVM("a", 2048, 1000, 200, 1800), d}, StepMiB), Deflate, map[string]int64{"d": 0})
}

func TestABalloonThatCannotBeSetIsLeftAlone(t *testing.T) {
	t.Parallel()
	old := sharedVM("old", 4096, 4000, 0, 0)
	old.noBalloon, old.stats = true, false
	r := decide(5120, pool(old, sharedVM("c", 1024, 1000, 900, 50)), StepMiB)
	if _, ok := r.Balloons["old"]; ok || slices.Contains(r.Needing, "old") {
		t.Errorf("set %v, needing %v; want the old one left alone", r.Balloons, r.Needing)
	}
}

func TestASizeNearItsAllowanceIsLeft(t *testing.T) {
	t.Parallel()
	// 20 under its share of what is free: within the deadband.
	r := decide(5120, pool(sharedVM("a", 1516, 900, 100, 900), sharedVM("c", 1536, 900, 100, 900)), StepMiB)
	check(t, r, Hold, map[string]int64{})
}

func TestAStaleReportSaysNothing(t *testing.T) {
	t.Parallel()
	now := time.Now()
	b := driver.Balloon{Total: 1500 << 20, Available: 500 << 20}
	if _, ok := GuestInUse(b, now); ok {
		t.Error("a guest that never reported says what it uses")
	}
	b.Updated = now.Add(-31 * time.Second)
	if _, ok := GuestInUse(b, now); ok {
		t.Error("a report 31s old says what the guest uses")
	}
	b.Updated = now.Add(-5 * time.Second)
	if used, ok := GuestInUse(b, now); !ok || used != 1000 {
		t.Errorf("a report 5s old: %d, %v; want 1000 MiB", used, ok)
	}
}

// TestABootingGuestIsLeftToItsBalloon: a Shared guest whose VM started
// lately, still inflating the balloon it boots with, is left to it: its
// balloon untouched, counted at its guarantee, and polled for soon; once
// down to its guarantee, or past the window, it is a member as any.
func TestABootingGuestIsLeftToItsBalloon(t *testing.T) {
	t.Parallel()
	booting := sharedVM("b", 3600, 300, 100, 3400)
	booting.Started = longAfter.Add(-5 * time.Second)
	idle := sharedVM("c", 1024, 900, 100, 900)
	r := decide(5120, pool(booting, idle), StepMiB)
	check(t, r, Hold, map[string]int64{})
	if !r.Hot || r.Allowances["c"] != 1536 {
		t.Errorf("hot %t, allowances %v; want hot, c at half of what is left", r.Hot, r.Allowances)
	}
	// Inflated, to 1060: given its share of the 988 left, by how far each
	// could grow, as any.
	booting.balloonMiB = 4096 - 1060
	if r := decide(5120, pool(booting, idle), StepMiB); r.Balloons["b"] != 4096-1551 {
		t.Errorf("inflated: %v, want b given its share", r.Balloons)
	}
	// Past the window, inflated or not: squeezed as any.
	booting.balloonMiB, booting.Started = 4096-3600, longAfter.Add(-BootWindow)
	if r := decide(5120, pool(booting, idle), StepMiB); r.Balloons["b"] != 4096-3600+StepMiB {
		t.Errorf("past the window: %v, want b squeezed a step", r.Balloons)
	}
}
