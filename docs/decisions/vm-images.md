# Disk images: a platform catalogue fetched by URL and sha256

Status: accepted, 2026-10.

## Context

VirtualMachines boot a distribution's own cloud image
(`docs/decisions/machine-model.md`). Distributions publish those as qcow2 or
raw files over HTTPS, with their checksums. Tenants will want images of
their own later, which the API must make room for without a breaking
change; for now only the platform chooses which images exist.

## Decision

- **An `Image` is global and the platform's** (`storage` group, beside
  `Disk`, which it is a source of). Only platform administrators apply or
  delete one; everyone allowed to use Hangar at all reads every image
  (`readableByAll`), since tenants choose from the catalogue.
- **An image is a URL and the sha256 of the file there**, with display
  metadata (os, version, arch, only amd64) and an optional
  `min_disk_size`. The digest, not the URL, is what an image is: the base
  a node makes is named after it, and a URL serving another file is never
  used. No OCI repackaging: the distribution's file is used as it is.
- **A disk names its image by a structured reference**,
  `source.image.name`, which names a global image. Project images will add
  a field beside it (a project), and a reference without one keeps
  meaning a global image, so no existing manifest changes meaning. Not a
  path string such as `global/<name>`: a structured field is validated and
  extended field by field.
- **A node fetches an image when a disk of it is first made there**: over
  HTTPS only (redirects too), bounded in size (16 GiB a file, 256 GiB of
  disk, by default) and in progress as registries are (no answer within
  30 s, or a body slower than 30 KiB in 30 s, fails the fetch), into a
  directory on the image dataset, whose quota bounds it. The sha256 is
  checked as the file comes. `qemu-img info` must find raw or qcow2 naming
  no backing or data file; `qemu-img convert` then writes it raw onto a
  zvol of its size, the base (`docs/decisions/storage-zfs.md`), and the
  file is removed. A disk is a clone of the base grown to the disk's size;
  the guest grows its partition and filesystem itself (cloud-init).
- **qemu-img runs as root, unsandboxed.** It parses only files a platform
  administrator chose and pinned by sha256, so it reads nothing a tenant
  controls. Project images would change that: their files would have to be
  converted in a sandbox (an unprivileged, namespaced process with only the
  file and the device), or by a guest.
- **An image at fault fails for good.** A file that does not match its
  sha256, that qemu-img refuses or finds of another format, or that is too
  large, is reported on the Image (`Verified` False, with reason
  `HashMismatch`, `InvalidImage` or `ImageTooLarge`, at that generation):
  no node fetches it again, and admission refuses new disks of it, until
  the image's spec changes. A failure that may pass (a server down, a
  stall) is tried again with backoff. Any node writes an image's status:
  what a URL and digest turn out to be holds on every node.
- **What a node found is on the Image**: `Verified` True and
  `status.disk_size`, the image's own size in whole Gi, which admission
  then holds new disks of it to, with `min_disk_size`. A disk admitted
  before its image's size was known, and smaller, is told `DiskTooSmall`
  by its node.

## Consequences

- **A node keeps an image's base for as long as its Image exists**, not
  only while a disk is cloned from it, so a disk made after every other
  disk of the image is gone needs no fetch and no conversion. The sweep
  removes a base once no disk on the node is cloned from it or still to be
  made of it, and no Image that is not being deleted names its digest:
  after its Image is deleted, or names another digest. A node never fetches
  an image no disk on it was made of: what it keeps is what it has used.
  The node learns which Images exist by listing them, as every user may
  (`readableByAll`); nothing about what nodes may read changed.
- **Kept bases are on the pool, not under the images quota.** A base is a
  sparse zvol under `hangar/vmbases` (`docs/decisions/storage-zfs.md`);
  `hangar/images` holds only the downloaded file, until its base is made.
  `hangar-node --reserved-disk` holds back only the dataset quotas, so
  the pool's free space must leave room for the bases of the Images used
  on the host (`docs/runbook/host-bootstrap.md`).
- The image server must be reachable from the nodes when a node first
  makes a disk of the image; nothing is mirrored.
- Changing an image's URL or digest affects only disks made afterwards: a
  disk keeps the base it was cloned from.
- `qemu-img` is a host package (`deploy/host/versions.env`), kept current
  by unattended upgrades, with a version floor `verify.sh` checks.
