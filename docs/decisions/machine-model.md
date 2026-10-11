# Containers and VMs are both microVMs

Status: accepted, 2026-10.

## Context

Users want ordinary VPS-style VMs and also containers that agents start, edit
code in, and run tests in. Snapshots (disk clone, memory suspend, fork) matter
for that agent use case, even though they come later. Hangar uses Cloud
Hypervisor directly, without Kata or Kubernetes.

## Decision

- A container is booted the way Fly Machines does it: the OCI image becomes the
  root disk of a microVM, booted with the platform kernel, with `hangar-init`
  as PID 1 and guest agent. There is no CRI or Pod model. Multi-process or
  sidecar setups run Docker inside the VM, so the guest kernel must support
  Docker (overlayfs, netfilter, cgroup v2, bridge, veth).
- Users see `Container` and `VirtualMachine`. Controllers translate both into
  an internal `Machine`, the only kind the node handles. Snapshots, disks,
  networking and quotas exist once, at the `Machine` level.
- A `Container`'s root disk lives and dies with it: kept across restarts,
  rebuilt when the image changes. A `VirtualMachine`'s root disk is a durable
  `Disk`.

## Consequences

- The `Machine` state machine reserves a `Suspended` state, and snapshots will be
  a separate resource rather than part of `Machine.spec`.
- The guest agent protocol reserves a post-restore hook (clock, network and
  identity resync) and filesystem freeze, needed once snapshots exist.
