# Container volumes, and what a Disk is used as

Status: accepted, 2026-10.

## Context

Containers had no storage but their root disk, which is rebuilt when the
image changes. Users coming from Docker and Compose expect named volumes:
a directory that outlives the container, survives a new image, and can be
given to the next container. Below the `Container`, the pieces already
existed: a `Disk` is a durable block device on one node, a machine lists
the disks it has (`spec.disks`), a container's machine gets them as `vdc`
on, and one machine has a disk at a time (`machine-model.md`).

A disk is also used in more than one way, and the same disk can change use:
a VM boots from it, a VM has it as a data disk, a container mounts it. A
broken VM's boot disk is attached to another VM to rescue it, as AWS, Azure
and OCI allow, and a container may take a disk a VM used. So a disk has no
fixed type, and a client must not work out what one is used as from where
it is listed: the API says so.

Fly.io's volumes are the closest prior art: microVMs, a volume pinned to a
host and attached to one machine at a time, formatted on first use. Docker
gives the semantics users already know.

## Decision

- **A container lists its volumes in Compose's long syntax.** The fields of
  `spec.volumes` are those of a Compose service's `volumes:` entry, named
  as the API names fields: `type`, `source`, `target`, `readOnly`,
  `volume.nocopy`, `volume.subpath`. Hangar does not invent a format of its
  own, and nothing else in the API takes a Compose file. A translator from
  `compose.yaml` to Disks and Containers, the short syntax
  (`pgdata:/data:ro`) included, belongs to the clients (CLI, console), and
  comes later.
- **`type` is `volume`, the only type for now.** `bind` has no host path to
  give, and is refused, as `tmpfs` is until it is built; both are refused
  by name, not as unknown.
- **`source` names a Disk, or what `sourceKind` says.** `sourceKind` is
  Hangar's, not Compose's: empty is `Disk`, the only kind built so far.
  `FileShare` (`file-shares.md`) is the other, the service side written
  the same way, mounted over NFS `nosuid,nodev` and never filled from the
  image; what backs a volume is chosen where Compose
  chooses it, by the top-level volume's driver, which the translator maps
  to the kind. Two kinds keep their own names, so a source never depends on
  which kind happens to hold a name.
- **A Disk is a Compose named volume on one node.** One machine has it at
  a time, so a disk shared between services, and a service with replicas
  and a volume, cannot be expressed; both wait for `FileShare`. A
  container naming a disk that does not exist yet, or that another machine
  has, is held back (`Ready` False, `MachineHeldBack`) until the disk is
  made or let go. A container with a volume is pinned to the volume's
  node, as a VM with a disk is: a disk not yet made that is added to a
  placed container is made on its node, if it has room, and one living on
  another node is refused (`MachineRefused`), since a machine never moves.
  Changing a container's volumes restarts it.
- **hangar-init formats a blank volume, and nothing else.** Before
  switching to the root disk, hangar-init looks at each volume. If its
  first and last MiB are all zeros (a new sparse zvol reads as zeros, and
  every partition table, filesystem and RAID signature it would harm lies
  in one or the other), it makes an ext4 filesystem on it, with no root
  reserve and no `lost+found`, so a new volume is an empty directory as
  Docker's is (`initdb`, for one, refuses a data directory holding one).
  If it is ext4, it mounts it and grows it to the device, as it grows the
  root disk. Anything else (a partition table, another filesystem, a VM's
  boot disk) fails the boot, the reason on the container's log; a volume
  is never formatted because it was attached in the wrong place.
- **The initramfs carries a static `mke2fs`.** Formatting in the guest
  keeps the host from ever touching a volume's contents (`storage-zfs.md`),
  and keeps the decision of what is blank with the only party allowed to
  read the data. A filesystem written in Go is not worth the risk of
  getting ext4 wrong under a tenant's data. `mke2fs` is built from a pinned
  e2fsprogs release, as the kernel is from a pinned source
  (`docs/runbook/guest-kernel.md`).
- **A new volume takes what the image has at its target, as Docker's named
  volumes do.** When a volume mounted whole is new, holding nothing or an
  empty `lost+found` directory alone, which it removes, hangar-init copies
  into it the image's directory at the target: contents, holes, owners,
  modes, times and xattrs, with the directory's own owner and mode.
  Without it, a fresh filesystem's root is root's, `0755`, and an image
  running as another user (`USER postgres`) cannot write to its own data
  directory. `volume.nocopy` turns it off. The copy never follows a
  symlink out of either tree. A volume holding anything is never copied
  into again: a copy that fails, such as one that does not fit, fails
  that boot, and a later boot, finding the volume not new, runs on what
  was copied.
- **The target is resolved as the image's processes would resolve it**,
  its symlinks followed inside the image's root, and it is made if it is
  missing, as Docker makes it. Admission refuses a target that is not
  absolute, is `/`, or lies under `/proc`, `/sys`, `/dev` or `/run`, which
  the platform mounts over; hangar-init refuses one that leads there
  through the image's symlinks, or where another volume's leads. Two
  volumes may nest, ordered by where they lead, the outer mounted first.
  `volume.subpath` mounts a directory of the volume, which must exist,
  resolved inside the volume; no copy is made into it.
- **The apiserver says what a disk is used as, in `status.attachment`.**
  It is worked out from the spec of the machine listing the disk, never
  reported by a node nor inferred by a client:

  | The machine lists the disk as | `as` | `device` | `path`, `readOnly` |
  |---|---|---|---|
  | `firmware.rootDisk` | `BOOT` | `vda` | |
  | a disk a container's boot mounts | `VOLUME` | `vdc` on | the target, as written |
  | any other disk | `DATA` | after the boot's disks | |

  `kind` and `name` are the object the machine was made from, its
  controller owner, such as Container `web`, so a client links to what the
  user made; `machine` is the machine itself, which `status.machine` also
  names, kept for the clients that read it. `device` is the disk's place
  in the order the hypervisor attaches disks, which is how a Linux guest
  names virtio disks; what another guest calls it is the guest's business.
  A disk no machine lists has no attachment. A machine being deleted still
  lists its disks, so the attachment stays until the machine is gone.
- **What a disk was made for is a label, and only a record of intent:**
  `hangar/intended-use`, `boot` or `data`, which the console sets from the
  template a disk is created with (a boot disk from an image, a blank data
  disk). It restricts nothing; `status.attachment` says what the disk is.
  The key is outside `hangarlab.cloud`, which only the system may write
  (`apply-semantics.md`), because the user sets it.

## Consequences

- An image's `VOLUME` directories are not volumes: they are on the root
  disk, kept across restarts and lost when the image changes, whereas
  Compose keeps anonymous volumes when it recreates a container. A user who
  wants the data kept lists a volume at that path.
- Compose has no size for a volume, and a Disk needs one: the translator
  takes it from an extension, or a default.
- A disk formatted by a container is a plain ext4 filesystem on the whole
  device, which a VM can mount as a data disk; a VM's partitioned disk
  cannot be a volume without being wiped first.
- The initramfs is no longer hangar-init alone, and e2fsprogs is one more
  pinned source to keep current.
- `status.attachment` is singular because a disk has one machine at a time;
  a `FileShare`, which many machines mount, reports its users in its own
  status.
