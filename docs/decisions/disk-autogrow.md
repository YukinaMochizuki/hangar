# A container's disks grow by themselves, within quota, while it runs

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

### A Disk a container mounts

A container's volumes (`container-volumes.md`) are where its data goes, and
are sized up front just as its root disk was, so a Disk grows the same way.

- **`spec.grow` turns it on**, a `DiskGrow` of the storage group with the
  fields and rules of `resources.disk_grow` (`max` at least `spec.size`), read
  by the same `computeapi.ParseDiskGrow`. The storage group cannot use the
  compute group's message, which imports it.
- **It grows only while a running container mounts it read-write.** That
  container's node asks the guest how full the volume's filesystem is
  (`VolumeUsage`, by its device), and past the threshold lists the Disk in
  the machine's `status.disks_wanted`. A Disk attached to a VM, mounted
  read-only, or attached to nothing does not grow by itself.
- **The Disk's controller grants it, into `status.size`.** `status.size`
  already is the size the Disk has on its node, never less than before, and
  quota already counts the larger of it and `spec.size`. The grant is the
  root disk's: the next step from `status.size`, never the node's number,
  capacity then quota held, `checkFits` and `growthFits`, `DiskGrowBlocked`
  on the Disk when it does not fit, retried when the tenant's or project's
  usage or limits change or the node's room does. A Disk's spec is never
  rewritten: a `spec.size` below `status.size` is accepted for a Disk that
  grows, or grew, by itself, and shrinks nothing.
- **A mounted volume grows while its container runs**, as the root disk
  does: the zvol, then Cloud Hypervisor's `vm.resize-disk` for that disk,
  then `GrowVolume` in the guest. So does a `spec.size` raised by hand. A
  container volume's size is no part of what its VM runs. A VM or a guest
  that cannot take it while it runs, as for the root disk, takes it at its
  next boot; meanwhile the machine's `VolumeGrowth` condition, which each
  Disk it mounts carries as it is, is False with `VMCannotResize` or
  `GuestCannotGrow`.
- **A CustomImage captured from a grown Disk is as large as the Disk.** Its
  `status.disk_size` is the source's size, and a Disk made from it starts
  at least that large (`custom-images.md`): to make small Disks, capture a
  Disk that never grew. Starting a Disk at what an image holds would mean
  shrinking an ext4 filesystem, which the host never touches; that is not
  offered.

## Consequences

- A container whose growth is blocked fills its disk as one without growth
  does: the guest sees a full filesystem, never an I/O error, since the
  filesystem is always the size the zvol has.
- A guest is not trusted: one reporting itself full grows its own disk to
  its own `max`, which its tenant's quota pays for, and no further.
- A disk only grows; nothing shrinks a root disk, grown or asked.
- OpenZFS 2.4.1's `zvol_os_update_volsize` oops (`storage-zfs.md`) may meet
  a growth of a zvol a running VM holds; a root disk grown so on production's
  2.4.1 (2026-10, v0.2.0-rc.14) showed none.
- A VM's disks and FileShares do not grow by themselves: a VM has no agent
  to say how full they are, nor to grow its own filesystems.
