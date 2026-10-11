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
  snapshots) and for shared storage.
- Shared storage (`FileShare`) is a dataset exported over NFSv4 on the tenant's
  private network. It is for sharing data, not a working directory: workspaces,
  databases and `/var/lib/docker` stay on zvols.
- Guests never mount virtio-fs.

## Consequences

- A clone depends on its origin snapshot, so the API protects origins with
  finalizers.
- `zfs_arc_max` must be set so the ARC does not compete with guest memory.
- zvol snapshots are crash-consistent only; the guest agent will need
  filesystem freeze.
- NFS is fine for large files and slow for small-file metadata and heavy fsync
  workloads.
