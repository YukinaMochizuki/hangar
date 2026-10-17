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

A test waits for what it needs, never for time to pass: a sleep long enough
on a fast machine is too short on a loaded CI runner, and too long
everywhere else. Which tool it waits with depends on its layer:

- **In-process tests (L0) that time something** run inside
  `testing/synctest`, on virtual time, so a slow runner cannot stretch
  what the test measures. A bubble cannot hold a goroutine blocked on a
  socket or on SQLite, so tests that need those stay out.
- **L1 and L2 tests**, which run the apiserver, SQLite and the node agent,
  wait for the controllers with `WaitSettled`
  (`internal/controller/controllertest`): it returns once two polls in a
  row, each reading the same store revision before and after the statuses,
  find every controller with an empty queue, nothing in flight, no retry of
  a failure pending, and every change through that revision followed. Keys
  waiting on something outside the platform are returned, not waited for.
  Real HTTP and SQLite are what these layers exist to exercise, so they are
  not replaced with fakes to fit a bubble.
- **The shared `testutil.Eventually`** is for what neither covers.

Two more rules hold everywhere:

- **"Nothing happened" is checked on the object itself**, such as its
  resourceVersion, never on the store-wide revision, which unrelated writes
  move.
- **golangci-lint's forbidigo flags `time.Sleep`** in tests and
  test-support packages, outside `test/testbed`. A sleep that stays says
  why with `//nolint:forbidigo // <reason>`: real work or real elapsed time
  the test needs, or `virtual time` inside a synctest bubble. A sleep that
  only lets background work run is never a reason.

`docs/conventions/testing.md` shows how to apply these, with examples: what
WaitSettled cannot see and how to wait for it, how to make a fake signal
what happened, and how to show that a test can fail.

## Consequences

Most features can be tested end to end without KVM. Only the thin layer of real
drivers needs the testbed, and only the L5 checklist needs the production host.
