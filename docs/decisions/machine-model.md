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
- `Machine` is project-scoped and owned by the object it was made from, so
  garbage collection, tenant isolation and quotas need nothing new. Users can
  read their machines (it is where placement and boot failures show) but never
  write them: `Machine` and `Node` are system-managed kinds, whose spec only the
  apiserver writes, whatever role the caller holds.
- `Node` is a global kind, one per host. The apiserver creates it for each node
  agent it is configured to accept; the agent only reports status (capacity,
  heartbeat), and the apiserver marks a node `Ready=Unknown` when the reports
  stop, timing them on its own clock so host clock skew cannot fake or hide
  silence. A node agent writes the status of its own `Node` and of the
  machines placed on it. `Machine.spec.node` is set once and never changes;
  moving a machine means a new one.

## Consequences

- The `Machine` state machine reserves a `Suspended` state, and snapshots will be
  a separate resource rather than part of `Machine.spec`.
- The guest agent protocol reserves a post-restore hook (clock, network and
  identity resync) and filesystem freeze, needed once snapshots exist.
