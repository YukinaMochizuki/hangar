# ZFS zvols for machine disks, NFS for shared files

Status: accepted, 2026-10.

## Context

Snapshots need copy-on-write clones. Later memory snapshots need Cloud
Hypervisor's `ondemand` and `copyonwrite` restore modes. In CH, `ondemand` does
not support vhost-user devices, and `copyonwrite` works only with private
memory, while virtio-fs requires `memory shared=on`. Docker's overlay2 is also
unreliable on virtio-fs.

## Decision

- The host runs Ubuntu 26.04 (ZFS in-tree). A small mdadm RAID1 holds the root
  filesystem; the rest of the disks form a ZFS mirror (ZFS must not sit on
  mdadm).
- Every machine disk is a zvol. OCI and cloud images are converted once into a
  base zvol snapshot, and each machine clones from it.
- Datasets are for the host itself (image cache, database, logs, later memory
  snapshots).
- Shared storage (`FileShare`) is served over NFSv4 on the tenant's private
  networks by a server microVM of its own, from a zvol only that server
  mounts (`file-shares.md`); the host never parses NFS. It is for sharing
  data, not a working directory: workspaces, databases and
  `/var/lib/docker` stay on zvols.
- Guests never mount virtio-fs.

### Container images and root disks

- **Images are pulled into a cache on the host** (`hangar/images`), keyed by
  digest: the blobs, and a record of the image of the host's platform
  (linux/amd64) that the digest names, an index's or a manifest's. A cached
  digest needs no registry until it is forgotten, with its base, when the
  node agent evicts it: no machine uses it, and it is the least recently
  used past the image cache's bounds (`node-agent.md`). A blob is checked
  against its size and digest before it enters the cache.
- **The host unpacks a tenant's layers, as root, but never follows them out
  of the directory.** Every path is resolved inside it with `openat2` and
  `RESOLVE_IN_ROOT`, as the image's own processes would resolve it with the
  directory as their `/`; the last component is written with `*at` calls
  that never follow it, so an entry replaces a symlink rather than writing
  through it. A name or hardlink target climbing out by `..` fails the image,
  as Docker fails it. Device nodes are skipped; fifos are made. Owners, modes
  with their setuid bits, times, symlinks and hardlinks are kept, and of the
  xattrs only `security.capability` and `user.*`. What an image unpacks to is
  bounded, counting every node and byte made: file contents, names, symlink
  targets, xattrs (at most 64 KiB an entry) and the dirs made to hold
  entries (32 GiB and 4M entries by default); names are at most 256
  components deep, and dirs are made at most 256 deep and 3072 bytes from
  the root, whatever symlinks lead there, so the host can always walk what
  it unpacked. A layer declaring more is never fetched.
- **Unpacking is bounded in work, and stops when its context does.** A
  layer's walked dirs are kept, by their path without symlinks, and each
  symlink is resolved once a layer, in userspace, as `RESOLVE_IN_ROOT`
  would (an absolute target from the root, `..` no higher than it), with at
  most 40 symlinks in a name; what was walked is walked again only after
  something is removed. An opaque marker empties a dir once a layer. Every
  step is counted against a budget for the whole image (15M by default,
  `hangar-node --max-unpack-work`): an entry costs 10, and 1 more a KiB of
  its PAX records, which the tar reader parses whether or not they are
  kept; making a node 10 more; an xattr kept 14, and 1 more a KiB of it; a
  dir looked up 1, opened 2; a link read, a node removed or a dir entry
  listed 1. Over budget is `ErrImageTooLarge`; the context is checked every
  1024 steps.
- **At most 65,536 xattrs an image are kept.** On the default `xattr=on`
  each is a file in a hidden dir, and holds about 4.5 KiB of kernel memory
  (ZFS's dnodes and znodes, unreclaimable slab) while the unpacked tree
  lasts, until its base is made. The 64 KiB an entry allows hold about
  2,900 tiny ones: before this bound, 1.07M of them, all the budget allowed
  at 14 steps each, took 77 s and 4.8 GB of slab on the testbed, and at 1
  step each a testbed VM ran out of memory and hung. With `xattr=sa`
  memory stays flat, but each xattr set rewrites the file's whole list, so
  the same 1.07M took 8 min 24 s; the dataset keeps the default.
- **What the budget costs at worst**, measured on the testbed (4-vCPU nested
  VM, 8 GB, ZFS 2.2 with `xattr=on`) by `TestWorstCaseWork` in
  `test/testbed/disks`, at the default limits, each shape alone once ZFS
  had freed what the one before removed (2026-10, CPU time includes the
  test's own tar generator): files made with their metadata, 48 s (63 s of
  CPU; 31 to 79 s over four rounds); 1 MiB of unused PAX records an entry,
  30 s (52 s; 2 min 22 s at a step per 4 KiB, which is why it is a KiB);
  64 KiB xattrs, at the xattr bound after 19 s (28 s); tiny xattrs, at the
  xattr bound after 3 s; whiteouts of nothing 20 to 21 s; deep walks each
  after a removal 17 s; symlink chains each after a removal 10 s; trees
  made and removed 21 s. Under contention on the testbed's host, or right
  after shapes that left ZFS freeing gigabytes, a shape took up to ten
  times as long (whiteouts once 3 min 14 s, deep walks 1 min 42 s, trees
  3 min 40 s); none failed to end. The budget admits about 680,000 files
  of an ordinary image; a larger image needs `--max-unpack-work` raised.
  Before the budget, a 2000-entry layer of symlink chains took 16 s, and
  the limits allowed about 9 hours.
- **An image at fault is not unpacked again.** Too large, or holding what
  the host refuses (`ErrInvalidImage`: a name out of the root, an owner out
  of range, a type it cannot make, a tar or compression that does not
  read), it fails for good for that digest; a tag naming it is resolved
  again, so an image fixed under the same tag is pulled. A cached blob the
  host fails to read (EIO, as a ZFS checksum error gives) is not the
  image's fault: it is dropped from the cache, and fetched and checked
  against its digest again. Fetching takes no turn, and a registry that
  stalls is given up within about half a minute; at most two images are
  unpacked at once (`hangar-node --max-pulls`), in the order they came
  (`node-agent.md`).
- **Unpacking never takes the room running machines need.** `hangar/images`
  has a quota, and before unpacking an image the driver checks its layers'
  size, from the manifest, against the room left there and in the pool; one
  that does not fit fails with `DiskFailed` (no space) without being
  unpacked. The image dataset is mounted `setuid=off,exec=off,devices=off`
  and root's alone, so an unpacked image is inert on the host.
- **A base per image digest**: the zvol `hangar/bases/<sha256 hex>`, made
  with `mkfs.ext4 -d` from the unpacked image, so the host never mounts a
  tenant's filesystem, then snapshotted as `@base` and set read-only. It is
  sized from what it holds (4 KiB blocks, a fifth more, 64 MiB for the
  journal and metadata, and at least one inode per 16 KiB), with 4 KiB blocks
  and an inode ratio of 16 KiB forced, since mke2fs would otherwise make a
  small disk with 1 KiB blocks and too few inodes for it to grow well. It
  has no root reserve, as a container's disk has none, and its inode tables
  and journal are written when it is made, once, rather than by every
  guest in its clone.
- **A root disk per machine**: `hangar/roots/<machine uid>`, a clone of its
  image's `@base`, `volmode=dev`, sparse, its `volsize` the size the machine
  asks for. A root disk smaller than asked is grown (`zfs set volsize`),
  never shrunk; a size below its base's fails with `DiskTooSmall`. The size
  is part of the VM's spec, so a grown disk restarts the VM.
- **A base per disk image**: `hangar/vmbases/<sha256 hex>`, a sparse zvol
  of the size of the disk the image holds, onto which `qemu-img convert`
  writes it raw (`docs/decisions/vm-images.md`), then snapshotted as
  `@base` and set read-only, as an OCI image's base is. It goes once no
  volume is cloned from it, no disk still to be made on the node is of it,
  and no Image that is not being deleted names its digest
  (`docs/decisions/vm-images.md`).
- **A base per custom image**: `hangar/vmbases/customimage-<uid>`, keyed
  `customimage:<uid>` by its CustomImage's uid, never by a digest. The
  volume it is captured from is snapshotted as
  `hangar/disks/<disk uid>@customimage-<uid>`; that snapshot is sent
  whole (`zfs send | zfs receive`, no properties) into the base, received
  as its `@base` with `hangar:customimage` set on the volume, which `@base`
  then inherits; `@base` is set read-only, and only `hangar:customimage`
  set on `@base` itself (`zfs get -s local`), last, marks it finished. The
  source's snapshot is then destroyed. The base holds nothing of the
  volume, so the volume can go; until its snapshot is destroyed, ZFS keeps
  the volume from going. A base whose capture failed is destroyed, and an
  unfinished one is destroyed as the driver starts; the snapshot stays for
  the agent, which removes it once the capture failed for good, and keeps
  it for a capture cut short. It goes once no volume is cloned from it, no
  disk admitted from it is still to be made on the node, and its
  CustomImage is gone (`docs/decisions/custom-images.md`).
- **An uploaded custom image is staged on the images dataset**, then filled
  into the same base. Its file is written, chunk by chunk, to
  `<image dir>/uploads/<image uid>` on `hangar/images`, root's alone, whose
  quota bounds it, beside `<image uid>.state`, the count of bytes checked
  and the state of the file's sha256. The host only writes and hashes it;
  a converter VM reads it (`docs/decisions/vm-images.md`). Its base is a
  sparse zvol of the size the converter reports, made with
  `hangar:customimage` set, which the converter writes through its
  `/dev/zvol` link; it is then snapshotted as `@base`, set read-only, and
  marked finished as a captured one, the property set on `@base` itself,
  last. A fill that fails destroys it, as an unfinished one is destroyed as
  the driver starts. The staged file goes once the image is Ready or
  failed, or gone.
- **A volume per Disk**: `hangar/disks/<disk uid>`, a sparse zvol,
  `volmode=dev`, its `volsize` the size the Disk has on its node; grown,
  never shrunk, like a root disk. It is blank, or a clone of its disk
  image's or a custom image's `@base` (below whose size it fails with
  `DiskTooSmall`), and the
  host made no filesystem on it: the host never reads it. A container's
  guest makes one on a blank volume it mounts (`container-volumes.md`).
  It outlives the
  machines it is attached to, one at a time, and goes when its Disk is
  deleted, or when the sweep finds no Disk bound to the node for it. A grown
  volume restarts the machine it is attached to, as a grown root disk does.
- **A volume per FileShare**: `hangar/shares/<share uid>`, a sparse zvol,
  `volmode=dev`, blank, at the size its server's machine names, grown,
  never shrunk; only that server has it, and makes its filesystem
  (`file-shares.md`). Growing it restarts the server. It goes when its
  share is deleted, once its server is gone, or when the sweep finds no
  share bound to the node for it.
- **Every zvol is made `volmode=dev` from the start.** The driver's five
  parent datasets carry `volmode=dev`, so a zvol's device is made as `dev`
  when the zvol is. A zvol given `-o volmode=dev` under a parent without it
  gets its device made with the default mode (its partition table scanned),
  and setting its own `volmode` then removes and makes that device again,
  asynchronously. On OpenZFS 2.4.1 (kernel 7.0), a `zfs set volsize`, or a
  `zfs clone -o volsize=`, that lands while the device is being removed
  dereferences a NULL pointer in `zvol_os_update_volsize`: the kernel oopses
  and kills the `zfs` command, which the agent sees as `signal: killed`,
  after the new size is set. An oops inside a clone also leaves the clone
  without the properties after `volsize`, ownership among them. So a clone
  is made at its base's size and then grown, and a `zfs set volsize` that
  fails is followed by a read of the volsize: one at least what was asked
  is done. The driver gives an existing host's parents their `volmode` as
  it starts. OpenZFS 2.4 leaves the zvols already there as they are; 2.2
  makes the device of each one nothing holds open again, once, and leaves
  an open one, a running VM's, alone. An upstream report of the oops is worth
  filing: `zvol_find_by_name` hands out a zvol whose device is being
  removed.
- **The guest grows its filesystem.** hangar-init, after mounting the root
  disk and before switching to it, grows the ext4 to the whole device with
  `EXT4_IOC_RESIZE_FS`; a failure is logged and the guest boots as it is.
- **The driver knows its datasets by their user properties**: `hangar:digest`
  on a base and its snapshot, `hangar:machine` and `hangar:digest` on a root
  disk, `hangar:disk` (and `hangar:digest`, for one cloned from a disk
  image, or `hangar:customimage`, for one cloned from a custom image) on a
  volume, `hangar:customimage` on a custom image's base, its snapshot and
  the snapshot it is captured from, `hangar:fileshare` on a share's. It
  lists from them,
  and a dataset whose
  properties do not match its name is never listed, changed or destroyed,
  whatever it is called. It
  never destroys recursively; ZFS itself refuses to destroy a base with
  clones or a root disk that is open. A base without its snapshot is
  unfinished and is made again; so is a disk image's whose snapshot lacks
  its property set on itself, as a received one inherits it from its
  volume.
- **A zvol's `/dev/zvol` link is trusted only once the device it leads to
  names that zvol** (ZFS's `BLKZNAME`, as udev's `zvol_id` asks). udev makes
  links after `zfs` returns, so for a moment the link of a destroyed zvol of
  the same name, such as a machine's previous root disk, can lead to another
  zvol that took its minor. `mkfs` runs on the device so checked, and a root
  disk or volume is handed out only once its link leads to it; the VM's
  launcher checks each again as it opens it.
- **udev neither probes nor watches zvols.** On a stock Ubuntu host it reads
  the ext4 superblock of every base and root disk (`blkid`, for
  `/dev/disk/by-uuid`), which is the host parsing a tenant's filesystem, and
  opens a zvol whenever a writer closes it. `deploy/host` installs two rules
  for `zd*`: `UDEV_DISABLE_PERSISTENT_STORAGE_BLKID_FLAG=1` before
  `60-persistent-storage`, and `nowatch` after `60-block`; the `/dev/zvol`
  links, from `60-zvol`, stay. The flag is honoured by
  `ENV{UDEV_DISABLE_PERSISTENT_STORAGE_BLKID_FLAG}=="1",
  GOTO="persistent_storage_blkid_probe_end"`, line 123 of
  `/usr/lib/udev/rules.d/60-persistent-storage.rules` on the testbed (Ubuntu
  24.04, systemd 255.4), before every `IMPORT{builtin}="blkid"` in it; the
  same line is in systemd 259's `rules.d/60-persistent-storage.rules.in`,
  which Ubuntu 26.04 ships (259.5). A destroy refused as busy is still
  retried for a few seconds, as other tools may open a zvol.

## Consequences

- A clone depends on its origin snapshot, so the API protects origins with
  finalizers.
- `zfs_arc_max` must be set so the ARC does not compete with guest memory.
- zvol snapshots are crash-consistent only; the guest agent will need
  filesystem freeze.
- NFS is fine for large files and slow for small-file metadata and heavy fsync
  workloads.
- An image is held twice on the host, compressed in the cache and as its
  base, until no machine uses it. Root disks are sparse, so the pool is
  overcommitted: its free space, which the node reports, shrinks as guests
  write.
- Every clone of a base shares its filesystem UUID.
- The pool, the `hangar/images` dataset and its quota are the host's to
  provision (`docs/runbook/host-bootstrap.md`); the driver makes
  `hangar/bases`, `hangar/roots`, `hangar/disks`, `hangar/vmbases` and
  `hangar/shares` itself.
- The layers' size is a floor: an image compressing far more than the usual
  can still fill `hangar/images` while it unpacks, up to the quota, and
  fail there.
