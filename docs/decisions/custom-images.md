# Custom images: a project's own disk images, captured from its stopped disks

Status: accepted, 2026-10.

## Context

Tenants want golden images: set a VM up, stop it, keep its root disk as an
image of their own, and make new disks from it. Until now the only images
were the platform's global catalogue (`docs/decisions/vm-images.md`), which
made room for project images without saying what they are. Uploading an
image file is wanted later, as a second way to make one.

## Decision

- **A `CustomImage` is a kind of its own, project-scoped** (`storage`
  group, short name `cimg`). It is the project's and no one else's: no
  sharing across projects yet. Its `spec.source` is a oneof that holds only
  `disk {name}`, a Disk of the same project, for now; an upload will be a
  second source of the same kind. `os`, `version`, `default_user` and
  `min_disk_size` mean what they mean on an `Image`.
- **A disk names one by `source.image.project`.** `ImageSource` gains
  `project`: unset, `name` is a global Image, as before, so no manifest
  changes meaning; set, it names a CustomImage of that project. Admission
  accepts only the disk's own project, and says so. A VM's
  `root_disk.from_image.image` is the same message, so a VM is made from a
  custom image as from an Image.
- **The source must not be running.** Admission refuses a CustomImage whose
  source disk does not exist, is being deleted, or was never made on a node
  (a disk is made when a machine with it first runs); and one whose disk a
  machine lists that its node has not reported stopped (phase `STOPPED` or
  `FAILED`) at its current generation, or a VM or container lists that asks
  to run. A disk attached to nothing, or only to stopped machines, is
  captured. Capturing a running machine (a `force`) is not offered.
- **Until the node has snapshotted the disk, the disk is held.** While a
  CustomImage holds its source (not `Snapshotted` True, not failed, not
  being deleted), admission refuses starting a machine, VM or container
  that lists the disk, attaching the disk to anything, and deleting it. Once
  `Snapshotted` is True the disk is free: it can run, change or go without
  touching the image. Every write of a CustomImage, Machine, VM, Container
  and Disk holds the apiserver's `attach` lock from its checks to its
  write, so a capture and a start or an attach never both pass. A project
  being deleted takes the disk anyway.
- **The image is an independent copy, captured on the disk's node.** The
  apiserver stores the source disk's node and size, in whole Gi, as the
  image's `status.node` and `status.disk_size` when the image is made; that
  node alone reports on it. Its agent snapshots the disk's volume
  (`hangar/disks/<disk uid>@customimage-<image uid>`), reports
  `Snapshotted`, and then, off its controllers, sends that snapshot whole
  (`zfs send | zfs recv`) into a new base, read-only, and destroys the
  snapshot. The base references nothing of the source, so the source disk
  and its own image base can go. Not a clone of the source's snapshot:
  that would keep the source volume for as long as the image lives.
- **A base is keyed by the CustomImage's uid**: `hangar/vmbases/
  customimage-<uid>`, received with its `@base` snapshot, keyed
  `customimage:<uid>` where an Image's base is keyed by its `sha256:` digest
  (`docs/decisions/storage-zfs.md`), so neither can be taken for the other.
  The uid, not the name, so a new image of a deleted one's name is another
  base. The base, its snapshot, the capture snapshot and every volume
  cloned from the base carry `hangar:customimage=<uid>`, and the driver
  leaves alone a dataset whose properties do not match its name.
- **A capture that fails, fails for good.** What it made is destroyed, the
  base and the capture snapshot, and the image says `Ready` False with
  reason `CaptureFailed`; it is never tried again (the disk may be running
  by then), and the user deletes it and captures again. A copy that takes
  longer than `hangar-node --capture-timeout` (6 hours by default, a full
  2 TiB disk at 100 MB/s) fails so too, rather than leaving the image
  capturing for ever behind a stalled `zfs`. An agent restarted
  mid-capture starts the send again from the snapshot, which it keeps
  until the base is made; a snapshot reported taken and since lost fails
  the image.
- **Ready says the image can be used.** The node reports `Ready` True with
  `status.used_size`: the base's `logicalreferenced`, rounded up to whole
  Gi. A disk of a custom image is admitted only while the image is Ready,
  not being deleted, and at least `disk_size` and `min_disk_size`; its
  volume is cloned from the base by the same `EnsureVolume` as an Image's,
  on the image's node.
- **Quota counts what the image holds, and how many there are.** `Limits`
  and `Usage` gain `custom_images`, a count. Toward `disk` an image counts
  its `disk_size` while it is captured, which is reserved as it is made,
  and its `used_size` once Ready; one that failed or is gone counts
  nothing, toward either. The default plan allows 5.
- **The platform never changes what an image holds.** No generalization:
  no machine id, host keys, users or logs are removed. A VM made from one
  is a new instance to cloud-init anyway, as its instance id is the VM's
  uid, which gives it its own hostname and runs its per-instance modules
  again. The user runs `cloud-init clean --machine-id` (and anything else
  their software needs) before stopping the source, so each copy gets its
  own `/etc/machine-id` too.

## Consequences

- **Deleting a CustomImage needs no finalizer.** It is gone at once, and
  admission refuses new disks of it from then on. Disks already made of it
  keep working: their volumes are clones of its base, which the node's sweep
  removes only once the image is gone and no volume on the node is cloned
  from it. A disk admitted from it but not yet made on its node when it is
  deleted cannot be made, and says so.
- **An image lives on one node**, its source disk's, and disks of it can be
  made only there; nothing copies an image between nodes. With one host,
  that is every disk.
- **A source disk deleted after it was snapshotted waits for the capture**:
  ZFS refuses to destroy a volume with a snapshot, so its node releases it
  once the base is made or the capture failed, and the snapshot is gone.
- Bases are on the pool, outside any dataset quota, as Images' are; the
  tenant's disk quota is what bounds them.
- A capture copies the whole of what the source holds, at the pool's speed,
  and briefly holds both the snapshot and the copy.
