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
  machines placed on it, nothing else. `Machine.spec.node` is set once and
  never changes; moving a machine means a new one.
- Resources are Kubernetes-style quantities: `cpu: 2`, `memory: 512Mi`,
  `disk: 10Gi`. A `Container` keeps what its user wrote; its `Machine` gets
  canonical forms (whole vCPUs, memory in whole `Mi` and disk in whole `Gi`,
  both rounded up), and nodes report capacity and allocatable the same way.
  vCPUs are whole, so `500m` is refused. A container's root disk can grow but
  never shrink, defaults to 10Gi, and an empty disk counts as that default.
  Only a change of disk is checked. Sizes compare as the disk has them, in
  whole Gi (`10G` after `10Gi` is no shrink), against the disk the
  container's placed machine has: a growth that never took effect, such as
  one its node had no room for, can be withdrawn down to that, as Kubernetes
  allows after a failed volume expansion. An unplaced machine has no disk
  yet, so its size can still go down. Sizes are capped at 8192 vCPUs (Cloud
  Hypervisor's limit), 4Ti of memory and 64Ti of disk.
- A scheduler in the apiserver places each machine on a ready, uncordoned node
  with room for its memory and its disk, preferring the most free memory.
  Memory and disk are the resources it reserves; vCPUs are shared. Root disks
  are sparse, so a machine holds the disk size it declares, and a node that
  reports no disk is not checked for it. A placed machine is only grown
  (memory or disk) if its node still has room. The scheduler owns the machine's
  `Scheduled` condition and sets phase `Pending` until the node reports; the
  node owns the rest of the machine's status. A container is `Ready` only
  while its node is reporting.
- Deleting a `Container` waits for its machine, and a placed machine waits for
  its node to report `Released` (VM and disks gone) before its finalizer is
  removed. A client watching the container's deletion sees the host's state,
  not a promise. Open question: a machine on a node that never comes back can
  never be released, which also holds up its container, project and tenant;
  the way out belongs with node lifecycle (draining, removing a node).

## Consequences

- The `Machine` state machine reserves a `Suspended` state, and snapshots will be
  a separate resource rather than part of `Machine.spec`.
- The guest agent protocol reserves a post-restore hook (clock, network and
  identity resync) and filesystem freeze, needed once snapshots exist.
