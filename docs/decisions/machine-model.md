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
- A `VirtualMachine` boots its root disk's own operating system through
  firmware (`vm-boot.md`), not the platform kernel; its controller turns it
  into a machine `vm-<name>` whose boot is `firmware {root_disk}`, its root
  disk made from an image as `<name>-root` (deleted with it unless its
  `deletePolicy` is `RETAIN`) or one it names, its data disks after it, its
  interfaces `eth0` and on, one on `default` if it lists none, each in its
  security groups as a container's (`networking.md`), and its
  restart policy `on-failure`. It is `Ready` once its VM runs: nothing in
  the guest answers for it. `runState` stops it as a container's does, by
  its power button; only a guest that powers itself off stays off, until the
  VM is stopped and started, and one its host's shutdown stopped boots again
  with the host.
- A `Container` is stopped and started through its spec, `runState: STOPPED`
  or `RUNNING` (`CONTAINER_RUN_STATE_*`; unset is RUNNING), which its
  controller copies to `Machine.spec.run_state`. Stopping is `docker stop`:
  SIGTERM, SIGKILL after a grace, the VM powered off and never brought back by
  the restart policy, which stays as written and applies again once the
  container runs. The root disk and the address are kept, and RUNNING boots
  the same disk. The enum leaves room for more states, such as a paused VM.
  The container's `Stopped` condition is True once its node has applied the
  stop (the machine's generation observed), so `hangar wait --for=condition=Stopped`
  waits for it as `--for=condition=Ready` waits for a start; it is False
  while the container is asked to run, and Unknown while its node is not
  reporting. `Ready` is False with reason `Stopped` meanwhile. Apply replaces
  the whole spec (`apply-semantics.md`), so applying a manifest that leaves
  `runState` out starts a stopped container again, by design, as `kubectl
  apply` after `kubectl scale` resets the replicas. `hangar stop` and
  `hangar start` apply the container with only the run state changed, at
  the resource version they read, and read again on a conflict.
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
- A `Disk` (`storage` group) is a durable block device in a project, of a
  `size` in whole Gi that can grow but never shrink. A machine lists the
  disks it has by name, `spec.disks`, and its node attaches them as
  virtio-blk devices after the ones its boot makes, in order: a container's
  root and boot disks are `vda` and `vdb`, so its first disk is `vdc`. A disk
  is attached to one machine at a time: machine admission refuses a disk
  another machine lists, live or being deleted, or a disk being deleted, and
  every apply and delete of machines and disks holds one lock from admission
  to write, so two machines never both get one. A disk lives on one node,
  the one its first machine is placed on: the scheduler binds it there
  (`status.node`, and `status.size` from its spec) before writing the
  machine's node, and a machine with a bound disk goes only to that node.
  A bound disk holds `status.size` of its node's disk, as a machine holds its
  root disk; the disk controller grants a growth only if the node has room
  (else `Allocated` is False, `ResizeDoesNotFit`), and admission refuses a
  size below `status.size`, so a growth that never took effect can be
  withdrawn. A grown disk restarts the machine it is attached to, as a grown
  root disk does. Deleting an attached disk is refused, unless its project is
  being deleted; `disk-in-use` keeps a deleted disk until no machine lists
  it, then `node-cleanup` until its node reports `Released` (at once for a
  disk never bound). The apiserver writes a disk's node, size, machine, phase
  and `Allocated`; its node writes `Ready` and `Released`.
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
