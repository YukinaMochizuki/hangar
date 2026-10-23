# Custom images: a project's own disk images, captured from its stopped disks or uploaded

Status: accepted, 2026-10.

## Context

Tenants want golden images: set a VM up, stop it, keep its root disk as an
image of their own, and make new disks from it. Until now the only images
were the platform's global catalogue (`docs/decisions/vm-images.md`), which
made room for project images without saying what they are. Tenants also
want to bring an image file of their own, such as a NixOS qcow2 built with
nixos-generators: an upload is a second way to make one, from the CLI and
the console alike, and a file a tenant uploads is the first thing of a
tenant's the platform would parse as a disk image.

## Decision

- **A `CustomImage` is a kind of its own, project-scoped** (`storage`
  group, short name `cimg`). It is the project's and no one else's: no
  sharing across projects yet. Its `spec.source` is a oneof: `disk {name}`,
  a Disk of the same project, captured; or `upload {size, sha256}`, a raw
  or qcow2 file, uploaded (below). `os`, `version`, `default_user` and
  `min_disk_size` mean what they mean on an `Image`. An image made either
  way is the same once Ready: VMs boot from it, and a disk of it can be
  captured again.
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
- **An upload is a file of a declared size, and maybe a sha256.**
  `source.upload.size` is the file's bytes, 1 to `hangar-apiserver
  --max-upload-size` (16Gi); `sha256`, 64 lowercase hex digits, is
  optional: without one each chunk is checked, the whole file not. Both are
  immutable, as the rest of `source`. The image is placed as it is made on
  the node a machine would be (on one host, the only node), among those
  that take uploads, stored as `status.node`; with no such node ready it is
  refused. A node takes uploads, and says so in its status
  (`custom_image_uploads`), when it has the converter initramfs and a
  place to stage files; one placed on a node that since takes none still
  fails at `upload_expires`. Toward `disk` it reserves `size`, rounded up
  to whole Gi, from creation; once the node has read the file, the larger
  of that and the disk the file holds (`status.virtual_size`, below); and
  it settles to `used_size` once Ready, as a capture does. It counts toward `custom_images`. Formats
  other than raw and qcow2, URLs, sharing and copying between nodes are
  not offered.
- **A file goes up in chunks, in order, resumable.** `GET
  /v1/projects/{project}/customimages/{name}:upload` answers JSON
  `{received_bytes, chunk_size, size}`: where the next chunk starts, as the
  image's node counts it, and the most a chunk holds, 64 MiB. A `PUT` of
  the same path is one chunk, with `Content-Range: bytes a-b/size`, `a`
  the node's count, and `X-Hangar-Chunk-SHA256`, its hex sha256; it
  answers `{received_bytes}`, the new count. Both need
  `storage.customimages.apply` on the project, and nothing of the image is
  told before. They take every credential; a browser's session counts only
  on a request whose `Origin` is the console's
  (`docs/decisions/console-design.md`), so a cookie GET without one is
  refused, and the console reads `status.received_bytes` and corrects
  itself from a 409 instead. Refused before any of the body is relayed, as
  Connect-style JSON (`internal/uploadwire`): an image not uploaded, being
  deleted, failed, whose file is whole, or past `upload_expires`
  (`failed_precondition`); a malformed range or sha256, or a size not the
  image's (`invalid_argument`); a chunk over 64 MiB (413); one not at the
  node's count (409 `aborted`, its body carrying a top-level
  `received_bytes`); and the caps on calls to nodes. A chunk whose bytes
  are not its sha256 is dropped by the node (`invalid_argument`), the count
  as it was.
- **The apiserver relays a chunk; the node stages it.** The apiserver asks
  the node's count (`NodeService.GetCustomImageUpload`), then streams the
  body on the client-streaming `NodeService.UploadCustomImage`
  (`docs/decisions/node-tunnel.md`), 256 KiB a message, holding no more.
  The node writes it at its offset in the file, plus 4 KiB, to
  `<image dir>/uploads/<image uid>` on the images dataset, whose quota
  bounds it, one chunk of an upload at a time, counted only once its
  sha256 matches and it is synced. The staged file is 4 KiB of zeros, the
  tenant's bytes, then zeros to the next 4 KiB boundary and 4 KiB more:
  Cloud Hypervisor reads a disk's first and last bytes for a format's
  header (qcow2's and VHDX's at the start, VHD's footer at the end) even
  when told the disk is raw, refusing a mismatch, so the host never hands
  it the tenant's bytes at either end. The layout is the node's alone:
  `received_bytes`, every offset and the sha256 are of the tenant's file.
  The whole file's sha256 is computed as the chunks come, and the count and
  the hash's state (`encoding.BinaryMarshaler`) are kept beside the file,
  so an upload goes on, its hash right, after the agent restarts. The node
  reports `received_bytes` at most four times a second, and
  `upload_expires`: the image's creation and `hangar-node --upload-expiry`
  (24h).
- **A whole file is checked, then converted in a VM of its own.** At
  `size` bytes the node compares the declared sha256, if any (a mismatch
  is `DigestMismatch`), and reports `Uploaded` True with the `sha256` it
  computed. The host never parses the file. A **converter VM** does: the
  platform kernel booting the **converter initramfs**, hangar-init in a
  converter role and qemu-img from pinned Alpine packages
  (`docs/runbook/converter.md`), not the containers' initramfs, which
  stays as it is. It has no network interface, its agent answers CID 2
  alone over vsock, as hangar-init's does, and it is confined as every VM,
  with a VM user of its own, cgroup CPU and memory limits (1 vCPU,
  512 MiB) and Cloud Hypervisor's confinement, the staged file bound
  read-only into its unit and attached read-only as raw, its ends zeros, so
  Cloud Hypervisor finds no format in it. The guest maps the tenant's bytes
  alone, by a read-only loop device at the offset and size its boot disk
  gives (a size of a partial last sector rounded up to a whole one, the
  staging's zeros), and refuses a disk too short for them; qemu-img reads
  that device. Step one, with the file alone: it reports `qemu-img
  info`, and the node refuses anything not raw or qcow2, naming a backing
  or a data file (`InvalidImage`), or holding a disk over `hangar-node
  --max-image-disk-size`, 256Gi (`ImageTooLarge`).
- **The disk a file holds is kept within the quota before it is made.**
  A qcow2 of a few megabytes can hold a disk of 256Gi, which the base takes
  once converted. The node reports the size it read as
  `status.virtual_size`, with `Ready` still `Converting`; the apiserver,
  taking that report under the same lock as every apply that counts toward
  a quota, keeps it only if the project and the tenant have room for it,
  rounded up to whole Gi, besides everything else they hold. Otherwise it
  stores `Ready` False `QuotaExceeded` in place of the report, naming the
  limit, `Uploaded` still True; the image then counts nothing and the node
  removes the file. The node makes the base only once it reads the
  virtual size kept; nothing else of the node's can change it, nor undo
  `QuotaExceeded`. After the agent restarts the file is read again, and
  one found to hold another size fails as `ConvertFailed`. On the node,
  the pool's room for a base counts the bases being filled at their whole
  size, as a sparse volume takes its room only as it is written.
- **Then the file is converted.** The node makes
  `hangar/vmbases/customimage-<uid>` of that size, and the converter,
  given the file and the zvol, runs `qemu-img convert -O raw` with the
  format given, never probed. The base is finished and marked exactly as a
  captured one, the property set on `@base` itself, read-only; the node
  reports `Ready` with `used_size` and `disk_size`, the base's size in
  whole Gi, which only the node knows and it sets once. Reading the file is
  bounded by `hangar-node --inspect-timeout` (5m) and the conversion alone
  by `--convert-timeout` (2h), each VM's unit by `RuntimeMaxSec` too; the
  two share the node's slots for such work.
- **An upload that fails, fails for good**, as a capture does: `Ready`
  False with `UploadExpired`, `DigestMismatch`, `InvalidImage`,
  `ImageTooLarge`, `QuotaExceeded` or `ConvertFailed`; a partial base is
  destroyed, and the user deletes the image and uploads again. The staged
  file goes once the image is Ready or failed, and the sweep removes a
  deleted image's.
- **The platform never changes what an image holds.** No generalization:
  no machine id, host keys, users or logs are removed. A VM made from one
  is a new instance to cloud-init anyway, as its instance id is the VM's
  uid, which gives it its own hostname and runs its per-instance modules
  again. The user runs `cloud-init clean --machine-id` (and anything else
  their software needs) before stopping the source, so each copy gets its
  own `/etc/machine-id` too.

## Consequences

- **Deleting a CustomImage needs no finalizer.** It is gone at once, and
  admission refuses new disks of it from then on. Disks admitted from it
  keep working, as disks of a deleted Image do: the apiserver stamps each
  with the image's uid as it admits it (the reserved annotation
  `hangarlab.cloud/custom-image`), and its node makes one not made yet from
  that uid's base, while the base is there. The node's sweep removes the
  base only once the image is gone, no volume on the node is cloned from
  it, and no disk admitted from it, bound to the node or to none yet, is
  still to be made. The deleted image's quota is released at once
  meanwhile: the platform carries the space a kept base takes.
- **An image lives on one node**, its source disk's or the one an upload
  was placed on, and disks of it can be made only there; nothing copies an
  image between nodes. With one host, that is every disk.
- A tenant's file reaches qemu-img only in a converter VM, which sees
  nothing but that file and the base it writes: what a crafted file gains
  there is that VM, which writes only the tenant's own image.
- An upload briefly takes its room twice on the host, the staged file on
  the images dataset and the base on the pool. A chunk is 64 MiB, which a
  slow link sends within the front door's body-rate rules
  (`docs/decisions/front-door.md`).
- **A source disk deleted after it was snapshotted waits for the capture**:
  ZFS refuses to destroy a volume with a snapshot, so its node releases it
  once the base is made or the capture failed, and the snapshot is gone.
- Bases are on the pool, outside any dataset quota, as Images' are; the
  tenant's disk quota is what bounds them.
- A capture copies the whole of what the source holds, at the pool's speed,
  and briefly holds both the snapshot and the copy.
