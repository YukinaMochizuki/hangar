# Testing strategy

Status: accepted, 2026-10.

## Context

One person maintains Hangar, so coding agents must be able to write and run
realistic end-to-end tests without anyone preparing an environment for them.
Much of the system touches the host (KVM, ZFS, network namespaces), which
ordinary workspaces and CI runners cannot provide.

## Decision

| Layer | What runs | Where | When |
|---|---|---|---|
| L0 | Unit tests | workspace, CI | every PR |
| L1 | Control-plane e2e: real apiserver, SQLite, Connect, CLI binary | workspace, CI | every PR |
| L2 | Full-stack hermetic e2e: apiserver, controllers and `hangar-node` with fake host drivers | workspace, CI | every PR |
| L3 | Driver conformance: one suite run against the fake and the real driver | fake: CI; real: testbed | every PR; real when a driver changes |
| L4 | Real-host e2e: Cloud Hypervisor, ZFS, namespaces, vsock, Docker in a guest | disposable testbed VM | before merging node or guest changes |
| L5 | Behaviour only the production host has (OVH IPv6 upstream, public reachability, real Cloudflare) | the host | before a release, as a short checklist |

- **Every host dependency sits behind an interface with a fake**: hypervisor,
  storage, network, guest agent, systemd units, image fetching. Fakes support
  fault injection (boot failure, full disk, crashed process).
- **Conformance tests keep fakes honest.** The same suite runs against the fake
  and the real driver, so a fake that drifts from reality fails on the testbed.
  That is what lets L2 be trusted.
- **The control plane injects a clock**, so timeouts, compaction and heartbeats
  can be fast-forwarded, and signs people in through a configurable OIDC
  issuer, so tests run the real sign-in against a fake provider
  (`internal/auth/oidctest`) that signs its own ID tokens.
- **The testbed is a disposable VM** with nested KVM, set up by a script in the
  repo (`test/testbed/`) that shares most steps with host provisioning
  (`deploy/`), so host setup is exercised continuously. Production-only
  behaviour is emulated there where possible (a namespace playing OVH's
  upstream router, with MAC filtering).
- **L4 tests live under `test/testbed/`**, behind the `testbed` build tag,
  and `make e2e-host` runs them on the testbed (`docs/runbook/testbed.md`).

## Flaky tests

About half the flaky tests so far were real bugs that only concurrency
exposed, so a flake is always fixed at its root:

- **No timeout bumps, no retries.** A failing test is never rerun until it
  passes, in CI or by hand, and a deadline grows only when it was wrong for a
  reason other than speed.
- **Merges need green CI**, including in packages a change does not touch.
- **Quarantine is the only way out**, and it is visible: a known flake calls
  `testutil.Quarantine(t, "#N")`, which names the GitHub issue tracking its
  root cause. It then skips in PR CI and still runs in the stress job.
- **The stress job** (`.github/workflows/stress.yml`) is run by hand
  (`workflow_dispatch`, on any branch, optionally limited to some packages):
  `go test -race -shuffle=on -count=20` with `GOMAXPROCS=2` and CPU load in
  the background, with quarantined tests included
  (`HANGAR_RUN_QUARANTINED=1`). It shakes out flakes that one CI run rarely
  hits. A failure it finds is a new issue, or a comment on the one that
  already tracks the test.
- **A test leaves quarantine** in the change that fixes its root cause, once
  the stress job has passed on that change's branch for the test's package.

## Waiting in tests

A test waits for what it needs, not for time to pass. Which tool it waits
with depends on its layer:

- **In-process tests (L0) that time something** (a grace period, a
  backoff, a rate, a ticker, a liveness ping) run inside
  `testing/synctest`'s `synctest.Test`. Time there is virtual: it moves
  only when every goroutine of the bubble is blocked, so a slow runner
  cannot stretch what the test measures. `synctest.Wait` replaces "let
  the background work run", and `time.Sleep` moves the clock.
  A bubble cannot hold a goroutine blocked on a socket or on SQLite, so a
  test that needs real TCP or the store stays out, or moves to `net.Pipe`
  or an in-memory fake where that keeps what it checks.
- **L1 and L2 tests**, which run the apiserver, SQLite and the node agent,
  wait for the controllers with `WaitSettled`
  (`internal/controller/controllertest`; the apiserver harnesses and
  `test/e2e` wrap it). It returns once two polls in a row, each reading
  the same store revision before and after the statuses, find every
  controller (and the apiserver's garbage collector) with an empty queue,
  nothing in flight, no retry of a failure pending, and every change
  through that revision followed. A listing counts as in flight from the
  moment the watch or list before it ended. Keys waiting on something
  outside the platform (`controller.ErrWaiting`) are returned rather than
  waited for.
  - **Followed** is proven by progress events from the controller's
    `controller.Source`: inside the apiserver its store watch sees every
    write, of any kind; a node agent's comes from the API's bookmarks, the
    lowest across the kinds it watches, so `apiservertest` sends them every
    10ms.
  - **The same revision around and across polls** closes the gap between
    reading the revision and reading the statuses: a reconcile that wrote
    meanwhile moves it.
  - **It sees writes only.** What reaches a controller from outside the
    store (a `Trigger`, a host event) is invisible until it queues a key: a
    test waits for its visible effect first. A key whose reconciles keep
    failing never settles; its retry counts the reconciles in a row that
    failed or waited (`controller.Retry.Attempts`), so a test waits for
    the ones begun after it asks. The node agent counts what it does
    outside its controllers (`node.Agent.Activity`): the sweeps of the host
    its loop has finished, and the network changes it has taken in. The
    garbage collector retries what failed only in its periodic sweep, which
    WaitSettled does not see. With a fake clock, a retry after a failure
    comes only once the test advances the clock.
- **The shared `Eventually`** is for what neither covers: a condition in
  a test outside a bubble that nothing can be asked about.

`internal/testutil` holds the shared helpers: `Eventually` polls a condition
up to one default bound and reports what it last saw (`EventuallyWithin`
takes a bound of its own, such as a real VM's), and `Unchanged` checks that
"nothing happened" by watching one object's state, such as its
resourceVersion, over a window; never the store-wide revision, which
unrelated writes move. golangci-lint's forbidigo flags `time.Sleep` in
tests and test-support packages, outside `test/testbed`. A sleep that stays
says why with `//nolint:forbidigo // <reason>`: real work or real elapsed
time the test needs; or `virtual time`, for a sleep in a synctest bubble,
which moves the bubble's clock and waits for nothing real, marked exactly
so, so it can be found. A sleep that only lets background work run is
never the answer: wait for it with one of the above, or check the one
object's state with `Unchanged`.

## Consequences

Most features can be tested end to end without KVM. Only the thin layer of real
drivers needs the testbed, and only the L5 checklist needs the production host.
