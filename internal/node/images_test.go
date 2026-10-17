package node

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/fake"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/registry"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// TestThePullerSaysWhenAnImageIsReady: the agent hears as soon as an image
// it waits on is ready, rather than at its next look.
func TestThePullerSaysWhenAnImageIsReady(t *testing.T) {
	host := fake.New()
	host.AddImage("app", "sha256:1", driver.ImageConfig{Cmd: []string{"true"}})
	letGo := host.HoldUnpack("app")
	p := newPuller(host.Images(), host.Storage(), clockwork.NewRealClock(), time.Minute, 2)
	ready := make(chan struct{}, 1)
	p.start(t.Context(), func() { ready <- struct{}{} })

	if _, _, ok, err := p.get("m1", "app", "", driver.Anonymous, false); ok || err != nil {
		t.Fatalf("ready before it was pulled: %t %v", ok, err)
	}
	letGo()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("never told the image was ready")
	}
	digest, config, ok, err := p.get("m1", "app", "", driver.Anonymous, false)
	if !ok || err != nil || digest != "sha256:1" || len(config.Cmd) != 1 {
		t.Errorf("get after ready: %s %+v %t %v", digest, config, ok, err)
	}
}

// TestAnImageAtFaultIsNotTriedAgain: an image too large, or invalid, fails
// for good: however long after, the next look does not unpack it again (a
// tag is resolved again, a pinned digest not even that), while a failure
// that may pass is tried again.
func TestAnImageAtFaultIsNotTriedAgain(t *testing.T) {
	for name, tt := range map[string]struct {
		err    error
		digest string
		again  bool
	}{
		"too large":          {fmt.Errorf("%w: more than 4 entries", driver.ErrImageTooLarge), "", false},
		"invalid":            {fmt.Errorf("%w: a name out of the root", driver.ErrInvalidImage), "", false},
		"invalid, pinned":    {fmt.Errorf("%w: a name out of the root", driver.ErrInvalidImage), "sha256:1", false},
		"a registry fault":   {fmt.Errorf("%w: 503", driver.ErrRegistryUnreachable), "", true},
		"a fault, by digest": {fmt.Errorf("%w: 503", driver.ErrRegistryUnreachable), "sha256:1", true},
	} {
		t.Run(name, func(t *testing.T) {
			host := fake.New()
			host.AddImage("app", "sha256:1", driver.ImageConfig{Cmd: []string{"true"}})
			host.FailUnpack("app", tt.err)
			clock := clockwork.NewFakeClock()
			p := newPuller(host.Images(), host.Storage(), clock, time.Minute, 2)
			done := make(chan struct{}, 1)
			p.start(t.Context(), func() { done <- struct{}{} })
			if _, _, ok, err := p.get("m1", "app", tt.digest, driver.Anonymous, false); ok || err != nil {
				t.Fatalf("first look: %t %v", ok, err)
			}
			<-done
			if _, _, _, err := p.get("m1", "app", tt.digest, driver.Anonymous, false); !errors.Is(err, tt.err) {
				t.Fatalf("the failure: %v", err)
			}
			clock.Advance(time.Hour)
			_, _, _, err := p.get("m1", "app", tt.digest, driver.Anonymous, false)
			if err == nil {
				<-done
			} else if tt.again {
				t.Errorf("an hour later, not tried again: %v", err)
			}
			if err == nil && tt.digest != "" && !tt.again {
				t.Error("a pinned digest at fault was tried again")
			}
			if _, _, _, err := p.get("m1", "app", tt.digest, driver.Anonymous, false); !errors.Is(err, tt.err) {
				t.Errorf("an hour later: %v", err)
			}
			p.wait()
			if begun, _, _ := host.Unpacks("app"); begun != map[bool]int{false: 1, true: 2}[tt.again] {
				t.Errorf("unpacked %d times", begun)
			}
			if tt.digest != "" && !tt.again && host.Resolved("app") != 0 {
				t.Errorf("a pinned digest at fault resolved %d times", host.Resolved("app"))
			}
		})
	}
}

// TestAFixedTagIsTriedAgain: an image at fault fails its digest, not its
// tag: once the tag names a fixed image, the next look pulls that.
func TestAFixedTagIsTriedAgain(t *testing.T) {
	host := fake.New()
	host.AddImage("app:1", "sha256:bad", driver.ImageConfig{Cmd: []string{"true"}})
	host.FailUnpack("sha256:bad", fmt.Errorf("%w: a name out of the root", driver.ErrInvalidImage))
	clock := clockwork.NewFakeClock()
	p := newPuller(host.Images(), host.Storage(), clock, time.Minute, 2)
	done := make(chan struct{}, 1)
	p.start(t.Context(), func() { done <- struct{}{} })
	look := func() (string, bool, error) {
		t.Helper()
		if _, _, _, err := p.get("m1", "app:1", "", driver.Anonymous, false); err == nil {
			<-done
		}
		digest, _, ok, err := p.get("m1", "app:1", "", driver.Anonymous, false)
		return digest, ok, err
	}
	if _, _, err := look(); !errors.Is(err, driver.ErrInvalidImage) {
		t.Fatalf("the bad image: %v", err)
	}
	// Still the bad image: resolved again, not unpacked again.
	clock.Advance(time.Minute)
	if _, _, err := look(); !errors.Is(err, driver.ErrInvalidImage) {
		t.Fatalf("the bad image, again: %v", err)
	}
	if begun, _, _ := host.Unpacks("app:1"); begun != 1 {
		t.Errorf("the bad image unpacked %d times", begun)
	}
	host.AddImage("app:1", "sha256:good", driver.ImageConfig{Cmd: []string{"true"}})
	clock.Advance(time.Minute)
	if digest, ok, err := look(); !ok || err != nil || digest != "sha256:good" {
		t.Errorf("pushed again, fixed: %s %t %v", digest, ok, err)
	}
	p.wait()
}

// TestTimeoutsBackOff: a job that keeps timing out is tried again later each
// time, from a minute up to the job's timeout, not every minute.
func TestTimeoutsBackOff(t *testing.T) {
	for name, timeout := range map[string]error{
		"the job's own": fmt.Errorf("stalled: %w", context.DeadlineExceeded),
		"a registry's":  fmt.Errorf("%w: %w", driver.ErrRegistryUnreachable, &registry.StallError{What: "sent nothing"}),
	} {
		t.Run(name, func(t *testing.T) {
			host := fake.New()
			host.AddImage("app", "sha256:1", driver.ImageConfig{Cmd: []string{"true"}})
			host.FailPull("app", timeout)
			clock := clockwork.NewFakeClock()
			p := newPuller(host.Images(), host.Storage(), clock, 30*time.Minute, 2)
			done := make(chan struct{}, 1)
			p.start(t.Context(), func() { done <- struct{}{} })
			// tried says whether a look after the wait tries the image again.
			tried := func(wait time.Duration) bool {
				t.Helper()
				clock.Advance(wait)
				before := host.Resolved("app")
				if _, _, _, err := p.get("m1", "app", "", driver.Anonymous, false); err == nil {
					<-done
				}
				return host.Resolved("app") > before
			}
			if !tried(0) {
				t.Fatal("never tried")
			}
			for i, gap := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute} {
				if tried(gap - time.Second) {
					t.Fatalf("after timeout %d, tried again before %v", i+1, gap)
				}
				if !tried(time.Second) {
					t.Fatalf("after timeout %d, not tried again at %v", i+1, gap)
				}
			}
			// A failure that is not a timeout is tried again soon.
			host.FailPull("app", fmt.Errorf("%w: 503", driver.ErrRegistryUnreachable))
			if !tried(30 * time.Minute) {
				t.Fatal("not tried again")
			}
			if !tried(time.Minute) {
				t.Error("after a failure that is no timeout, not tried again within a minute")
			}
			p.wait()
		})
	}
}

// TestPullsAreBounded: no more images are prepared at once than the bound;
// the rest wait their turn.
func TestPullsAreBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		host := fake.New()
		var letGo []func()
		for _, ref := range []string{"a", "b", "c", "d"} {
			host.AddImage(ref, "sha256:"+ref, driver.ImageConfig{Cmd: []string{"true"}})
			letGo = append(letGo, host.HoldUnpack(ref))
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		p := newPuller(host.Images(), host.Storage(), clockwork.NewRealClock(), time.Minute, 2)
		done := make(chan struct{}, 4)
		p.start(ctx, func() { done <- struct{}{} })
		for i, ref := range []string{"a", "b", "c", "d"} {
			_, _, _, _ = p.get(fmt.Sprint("m", i), ref, "", driver.Anonymous, false)
		}
		// Every pull has gone as far as it can: two unpack, held, and the
		// rest wait their turn.
		synctest.Wait()
		if _, n, _ := host.Unpacks(""); n != 2 {
			t.Errorf("%d unpacks under way, with a bound of 2", n)
		}
		for _, f := range letGo {
			f()
		}
		for range 4 {
			<-done
		}
		if _, _, most := host.Unpacks(""); most != 2 {
			t.Errorf("%d unpacks under way at once, with a bound of 2", most)
		}
		cancel()
		p.wait()
		for _, ref := range []string{"a", "b", "c", "d"} {
			if begun, _, _ := host.Unpacks(ref); begun != 1 {
				t.Errorf("%s unpacked %d times", ref, begun)
			}
		}
	})
}

// TestUnpacksTakeTurns: images waiting to be unpacked are unpacked in the
// order they came to wait.
func TestUnpacksTakeTurns(t *testing.T) {
	host := fake.New()
	refs := []string{"a", "b", "c", "d"}
	letGo := map[string]func(){}
	for _, ref := range refs {
		host.AddImage(ref, "sha256:"+ref, driver.ImageConfig{Cmd: []string{"true"}})
		letGo[ref] = host.HoldUnpack(ref)
	}
	p := newPuller(host.Images(), host.Storage(), clockwork.NewRealClock(), time.Minute, 1)
	p.start(t.Context(), func() {})
	begun := func(ref string) bool { n, _, _ := host.Unpacks(ref); return n > 0 }
	_, _, _, _ = p.get("ma", "a", "", driver.Anonymous, false)
	testutil.Eventually(t, "a unpacking", func() bool { return begun("a") })
	for i, ref := range refs[1:] {
		_, _, _, _ = p.get("m"+ref, ref, "", driver.Anonymous, false)
		testutil.Eventually(t, ref+" waiting", func() bool { return p.slots.waiting() == i+1 })
	}
	for i, ref := range refs[:len(refs)-1] {
		next := refs[i+1]
		letGo[ref]()
		testutil.Eventually(t, next+" unpacking", func() bool { return begun(next) })
		for _, later := range refs[i+2:] {
			if begun(later) {
				t.Errorf("%s unpacked before %s, which waited first", later, next)
			}
		}
	}
	letGo["d"]()
	p.wait()
}

// TestAJobNoMachineWantsGivesUpItsTurn: an image no machine wants any more
// is not unpacked when its turn would come.
func TestAJobNoMachineWantsGivesUpItsTurn(t *testing.T) {
	host := fake.New()
	host.AddImage("a", "sha256:a", driver.ImageConfig{Cmd: []string{"true"}})
	host.AddImage("b", "sha256:b", driver.ImageConfig{Cmd: []string{"true"}})
	letGo := host.HoldUnpack("a")
	p := newPuller(host.Images(), host.Storage(), clockwork.NewRealClock(), time.Minute, 1)
	p.start(t.Context(), func() {})
	_, _, _, _ = p.get("m1", "a", "", driver.Anonymous, false)
	testutil.Eventually(t, "a unpacking", func() bool { _, n, _ := host.Unpacks("a"); return n == 1 })
	_, _, _, _ = p.get("m2", "b", "", driver.Anonymous, false)
	testutil.Eventually(t, "b waiting", func() bool { return p.slots.waiting() == 1 })
	// m2 is deleted.
	digests := p.keep(map[string]bool{"m1": true})
	if digests["sha256:b"] || !digests["sha256:a"] {
		t.Errorf("bases kept: %v, want a's alone", digests)
	}
	testutil.Eventually(t, "b gone from the queue", func() bool { return p.slots.waiting() == 0 })
	letGo()
	p.wait()
	if n, _, _ := host.Unpacks("b"); n != 0 {
		t.Errorf("b, which no machine wants, unpacked %d times", n)
	}
	if n, _, _ := host.Unpacks("a"); n != 1 {
		t.Errorf("a unpacked %d times", n)
	}
}

// TestAnImageBeingUnpackedIsSeenThrough: an image no machine wants any more,
// already being unpacked, is unpacked to the end, and its base spared by
// the sweep meanwhile.
func TestAnImageBeingUnpackedIsSeenThrough(t *testing.T) {
	host := fake.New()
	host.AddImage("a", "sha256:a", driver.ImageConfig{Cmd: []string{"true"}})
	letGo := host.HoldUnpack("a")
	p := newPuller(host.Images(), host.Storage(), clockwork.NewRealClock(), time.Minute, 1)
	p.start(t.Context(), func() {})
	_, _, _, _ = p.get("m1", "a", "", driver.Anonymous, false)
	testutil.Eventually(t, "a unpacking", func() bool { _, n, _ := host.Unpacks("a"); return n == 1 })
	if digests := p.keep(map[string]bool{}); !digests["sha256:a"] {
		t.Errorf("bases kept: %v, want the one being made", digests)
	}
	letGo()
	p.wait()
	if host.Unpacked("sha256:a") != 1 {
		t.Error("the base was not made")
	}
}
