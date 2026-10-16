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

## Consequences

Most features can be tested end to end without KVM. Only the thin layer of real
drivers needs the testbed, and only the L5 checklist needs the production host.
