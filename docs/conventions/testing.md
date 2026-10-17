# Writing tests

How to write a test that holds to `docs/decisions/testing-strategy.md`. That
file says what the rules are and why; this one shows how, with examples taken
from the repo. Read it before you write or change a test.

## Wait for what you need

Never wait for time to pass. Pick the tool by the test's layer:

| The test | Waits with |
|---|---|
| Times something in-process (a grace period, a backoff, a rate, a ticker) | `synctest.Test` |
| Runs the apiserver, SQLite or the node agent (L1, L2) | `WaitSettled` |
| Neither, and nothing can be asked | `testutil.Eventually` |

### Timing something in-process: synctest

Inside `synctest.Test` time is virtual: it moves only when every goroutine
of the bubble is blocked, so a slow CI runner cannot stretch what the test
measures. `TestStopAsksTheGuestFirst` (`internal/node/driver/chvm`) checks
that a guest that powers off in 50 ms is not given the whole grace period:

```go
synctest.Test(t, func(t *testing.T) {
	// ...
	g.stop = func(_ context.Context, uid string) error {
		time.AfterFunc(50*time.Millisecond, func() { writeExit(t, h, uid, 143) })
		return nil
	}
	start := time.Now()
	if err := h.Stop(t.Context(), "m1"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d >= testGrace {
		t.Errorf("took %v, though the guest was off in 50ms", d)
	}
})
```

On the real clock this failed in CI at 496 ms against a 300 ms bound. In the
bubble the measured time is exact. `synctest.Wait()` waits until every
goroutine of the bubble is blocked, which replaces "let it run a moment".
A sleep inside a bubble moves the clock and is marked
`//nolint:forbidigo // virtual time`.

A goroutine blocked on a real socket or on SQLite never counts as blocked,
so a test that needs them stays out of the bubble, or uses `net.Pipe` or an
in-memory fake where that keeps what it checks. `TestARoundTrip`
(`internal/tunnel`) moved to `net.Pipe`, since the keepalive it checks works
the same over a pipe.

### Waiting for controllers: WaitSettled

L1 and L2 tests wait until the controllers have finished reacting. Each
harness wraps `controllertest.WaitSettled`: `waitSettled(t, s)` in
`internal/apiserver`, `Env.WaitSettled` in `apiservertest`, and
`w.waitSettled()` in `test/e2e`.

It returns the keys still waiting on something outside the platform
(`controller.ErrWaiting`, such as a VM booting) instead of waiting for them.
A test that expects none checks that the list is empty.

WaitSettled sees only writes to the store. Work that starts some other way
is invisible to it until it queues a key, so the test waits for that work's
visible effect first:
- a `Trigger` or a host event: wait for what it changes;
- a key whose every reconcile fails: it never settles, so wait for its
  attempts (`w.reconciled(ctrl, key, n)` in `test/e2e`, built on
  `controller.Retry.Attempts`);
- the node agent's sweeps and network changes, which run outside its
  controllers: `w.sweeps(n)` and `w.networkChangeTaken(from)`, built on
  `node.Agent.Activity`;
- the garbage collector's retries, which come only with its periodic sweep;
- with a fake clock, a retry comes only once the test advances the clock.

`docs/checklists/new-controller.md` says what a new controller must do so
that WaitSettled can see it.

### Neither: Eventually

`testutil.Eventually(t, what, cond)` polls `cond` up to one default bound,
and on failure says what it waited for and what it last saw, when `cond`
returns `(bool, string)`. `EventuallyWithin` takes a longer bound for
something slower than a process, such as a real VM. Both work inside
`t.Cleanup`. Never lengthen a bound to make a test pass: it bounds a test
that has gone wrong, not how fast anything should be.

## Check that something did not happen

"Nothing happened" is a statement about one object, so look at that object,
never at the store-wide revision, which any unrelated write moves.
`TestASettledDiskStaysQuiet` (`internal/apiserver/disks_test.go`) waits for
the controllers, then compares the disk's own resourceVersion:

```go
settled := waitDisk(t, s, system, "data", "in use", func(d *storagev1alpha1.Disk) bool {
	return d.GetStatus().GetPhase() == storagev1alpha1.DiskPhase_DISK_PHASE_IN_USE
})
waitSettled(t, s)
if now := getDisk(t, s, system, "data").GetMetadata().GetResourceVersion(); now != settled.GetMetadata().GetResourceVersion() {
	t.Errorf("a settled disk was written again: at %d, then at %d", settled.GetMetadata().GetResourceVersion(), now)
}
```

Where nothing can say when the work is done, `testutil.Unchanged` watches one
value over a window, for example the node of another machine in
`internal/node/node_test.go`. A window shows only that nothing happened
within it, so prefer a wait that knows when the work is done.

## Make the fake say what happened

When a test would sleep to give a fake's consumer time to act, give the fake
a signal instead. The fake host counts the packets it delivered and the reads
of its reader (`internal/node/driver/fake/packets.go`):

```go
// PacketsTaken reports whether what reads packets of the kind in the
// network's current namespace has begun a read past every packet sent.
func (h *Host) PacketsTaken(networkUID string, kind driver.PacketKind) bool {
	// ...
	return c.reads > c.sends
}
```

and the e2e test waits on it rather than on time:

```go
testutil.Eventually(w.t, "the packets sent taken", func() bool { return w.host.PacketsTaken(networkUID, kind) })
```

Count only what really happened (a packet delivered, not one offered), and
write down what the signal assumes ("one reader that answers a packet before
it reads the next"), so the next reader can tell when it stops holding.

## Show that the test can fail

A test that cannot fail when the behaviour breaks does not count. For every
new or changed wait or assertion:
1. Break the behaviour it checks: remove the line that does it, make a
   condition always true, swap a minimum for a maximum.
2. Run the test and see it fail, with a message that names what broke.
3. Restore the code.

Say in the PR which mutations you ran. A mutation that survives means the
test does not check what its name says.

## Permission and file-mode tests: run them as a regular user

CI runs `make test` as a regular user. Agent boxes run as root, and root
ignores file modes, so a permission test can pass in a box for the wrong
reason. `TestSecretFileModes` (`cmd/hangar-edge`) passed as root and failed
on every run as a user: each case rewrote one file that an earlier case had
left read-only. Before calling such a test done, run it as a user:

```sh
go test -c -o /tmp/pkg.test ./cmd/hangar-edge/
chmod 755 /tmp/pkg.test
su nobody -s /bin/sh -c 'TMPDIR=/tmp /tmp/pkg.test -test.run TestSecretFileModes'
```

## When a test is flaky

Find the root cause; about half the flakes so far were real bugs. While it
is open, quarantine the test with `testutil.Quarantine(t, "#N")`, naming the
GitHub issue that tracks it. Run `.github/workflows/stress.yml` by hand to
reproduce a flake or to show a fix holds. The rules are in "Flaky tests" in
`docs/decisions/testing-strategy.md`.
