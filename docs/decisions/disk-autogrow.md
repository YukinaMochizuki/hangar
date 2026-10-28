# A container's root disk grows by itself, within quota, while it runs

Status: accepted, 2026-10.

## Context

A container's root disk has the size its spec asks for, and the tenant's
disk quota counts that size. A workload that starts many containers at once,
each using about 1Gi but now and then much more, has to choose: ask for a
large disk each, and fill the plan's disk limit with space never written; or
ask for a small one, and have a container stop on a full disk.

Root disks are sparse zvols (`storage-zfs.md`), so a large disk costs the
host nothing until it is written. Counting what is written instead of what
is asked looks like the obvious fix, and is not: a tenant could then ask for
large disks and fill them, taking the shared host's NVMe past anything its
quota held, and nothing below the guest's filesystem can stop the writes
cleanly (a pool or parent-dataset quota fails them as I/O errors, not as a
full filesystem, and holds for one node, while a plan spans many).

## Decision

- **`resources.disk_grow` turns growth on.** `max`, required, is the
  largest the disk grows to, at least `resources.disk`, which is then the
  size it starts at. `threshold`, a whole percentage from 50% to 95%, is
  how full the root filesystem is when it grows; empty is 80%. `step` is a
  factor over 1x and at most 16x, such as `2x`, or a quantity of at least
  1Gi, such as `3Gi`; empty is `2x`. Each step rounds up to whole Gi, and
  none passes `max`. The rules are `computeapi.ParseDiskGrow`, which
  admission, the container controller and the node read alike; the console
  mirrors them (`web/src/components/disk-grow.tsx`).
- **The guest says how full it is; the node asks; the apiserver decides.**
  The node asks each running container's hangar-init for its root
  filesystem's size and use (`RootUsage`), and past the threshold sets the
  machine's `status.disk_wanted`. The container controller grants the next
  step from the disk the machine has, never the node's number, so an ask
  repeated is granted once.
- **A grant is a quota check and a node check, as one decision.** Holding
  the capacity lock and then the quota lock, the controller checks the
  container at the new size against the tenant's and project's limits
  (`checkFits`) and the growth against the node's room (`growthFits`). A
  step that fits is recorded in `ContainerStatus.disk`; the machine's disk
  is then the larger of `resources.disk` and it. A step that does not fit
  changes nothing: the container's `DiskGrowBlocked` condition says why
  (`QuotaExceeded`, or the node's `ResizeDoesNotFit`), the container stays
  Ready, and the step is tried again when the tenant's or a project's
  usage or limits change, or the node's room does. The node's room is
  taken only when the machine is written, after the grant: room another
  machine takes in between holds the granted step back as a resize in the
  spec is held back, on Ready, until there is room again.
- **Quota counts the larger of `resources.disk` and `status.disk`.** What
  the tenant holds is the disk as grown, never `max`: what the host can be
  made to hold is what the quota allows.
- **The spec is never rewritten.** `status.disk` holds the growth, so a
  manifest applied again, with its smaller `resources.disk`, shrinks
  nothing and is not refused. Turning growth off keeps the disk as grown.
- **The disk grows while the container runs, with no restart.** The node
  grows the zvol, then tells Cloud Hypervisor (`vm.resize-disk`, the size
  read back from the device, which it must match exactly), then asks
  hangar-init to grow the mounted filesystem (`GrowRoot`,
  `EXT4_IOC_RESIZE_FS`). A root disk's size is no part of what a VM runs
  (`driver.VMSpec.Hash`), so a resize in the spec does the same. A VM
  started before its launcher gave containers an API socket, or a guest
  whose hangar-init predates `RootUsage` and `GrowRoot`, grows at its next
  boot, as hangar-init grows the filesystem at every boot; meanwhile the
  machine's `RootGrowth` condition, which its container carries as it is,
  is False with `VMCannotResize` or `GuestCannotGrow`, and the container
  stays Ready.
- **A rebuilt root disk keeps the grown size.** A new image rebuilds the
  root disk at the machine's disk, which `status.disk` still sets.

## Consequences

- A container whose growth is blocked fills its disk as one without growth
  does: the guest sees a full filesystem, never an I/O error, since the
  filesystem is always the size the zvol has.
- A guest is not trusted: one reporting itself full grows its own disk to
  its own `max`, which its tenant's quota pays for, and no further.
- A disk only grows; nothing shrinks a root disk, grown or asked.
- OpenZFS 2.4.1's `zvol_os_update_volsize` oops (`storage-zfs.md`) may meet
  a growth of a zvol a running VM holds; the testbed's ZFS 2.2 does not
  show it, and production has not run it yet.
- VMs, Disks and FileShares do not grow by themselves.
