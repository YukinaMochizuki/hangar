# File shares: one NFS server microVM per share, on its own zvol

Status: accepted, 2026-10. The API, its admission and the quota are
built; a share stays Pending, a container declaring it is held back and a
VM runs without it, as its server, its node's part and the mounts are not
built.

## Context

A `Disk` is attached to one machine at a time (`machine-model.md`), so a
disk shared between services, and a service with replicas and a volume,
cannot be expressed; `container-volumes.md` leaves both to a `FileShare`,
which many machines mount at once over NFSv4 on the tenant's private
network (`storage-zfs.md`). Guests never mount virtio-fs.

What decides the design is where the NFS server runs, because whatever
parses a tenant's NFS requests is a new attack surface. Today a guest
reaches the host through the hypervisor and the node's Go services in each
network's namespace (resolver, DHCP, metadata), all bounded. No public
cloud lets tenants' NFS reach the kernel of the hosts their machines run
on: AWS EFS, Azure Files and DigitalOcean's file storage are fleets of
their own reached through an endpoint in the tenant's network; GCP
Filestore and OpenStack Manila's generic driver run a managed server
instance per share (or share network) with a block volume behind it.
Hangar has one host, so only the second shape keeps the host out of it.

## Decision

### The server

- **Each `FileShare` is served by a microVM of its own**, a system
  `Machine` the share owns (`fs-<share name>`), running the platform
  kernel's `nfsd`. The host never parses NFS: a tenant reaches the
  server's guest kernel, and the host sees only that VM's virtio devices,
  as it sees every machine's. Rejected: the host's `nfsd` in each
  network's namespace (a tenant's packets parsed by the host kernel),
  nfs-ganesha on the host (C on the host, needing `CAP_DAC_READ_SEARCH`),
  and a dataset given to the server by virtio-fs (`virtiofsd` on the host
  parsing what a compromised server sends).
- **Its data is a zvol**, `hangar/shares/<share uid>`, sparse,
  `volmode=dev`, attached to the server alone as a virtio-blk disk, as a
  Disk's volume is (`storage-zfs.md`). The server makes an ext4
  filesystem on it when it is blank and grows it to the device, as
  hangar-init does a container's volume (`container-volumes.md`), with no
  `lost+found` at its root; only the server's kernel ever mounts it, and
  clients see files over NFS, never the device. The host never reads it.
  The export is `sync`: a write a client was told is stable is on the
  zvol.
- **The server is the platform's, not the tenant's.** It cannot be exec'd
  or ssh'd into, has no egress and no NAT, and is driven by the node over
  vsock alone, its agent answering only CID 2 as hangar-init does
  (`guest-agent.md`). It runs while its share exists; a share used by no
  one keeps running (stopping idle servers may come later). Its size is
  the platform's: one vCPU and 256Mi, which held two busy clients with
  170 MB to spare on the testbed (128Mi worked; 64Mi did not boot).
- **Everything the server runs beside `nfsd` is Go, in its PID 1**: the
  answers to the kernel's export upcalls (`auth.unix.ip`, `nfsd.fh`,
  `nfsd.export`) and its client tracking (in place of `nfsdcld`). No
  nfs-utils, and no C of ours. `auth.unix.gid` is never opened, so a
  client's own groups count. Client records are kept in a `trusted.*`
  xattr of the share's root, which travels with the zvol and which NFS
  never shows a client.
- **The wire block size is pinned (1 MiB), never derived from the
  server's memory.** `nfsd` defaults it to a 4096th of RAM, so a 256Mi
  server would cap I/O at 32 KiB (a third of the throughput), and a
  server restarted smaller than its clients mounted against hangs them.
- **NFSv4.2 over TCP only**, port 2049, one export, the share's root, as
  the pseudo-root `/`: a client mounts `<address>:/`. No NFSv3, 4.0 or
  4.1, so no rpcbind or mountd on the wire. The export is `secure`: a
  client's requests come from privileged ports, so an unprivileged user
  in a client cannot speak AUTH_SYS as another.
- **A restart is seconds, not minutes.** The server's lease and grace
  periods are 15 s, not 90; containers mount with `timeo=50` (5 s), not
  600; a server is stopped gracefully where it can be; and it starts
  `nfsd` only once it has its export, which its boot disk carries, so a
  client is never refused for being unknown. On the testbed a graceful
  restart paused a writer 3 s, and a killed server 4 to 5 s, its clients'
  locks kept; with the defaults a killed server stalled them a minute.
- **Each client has an NFSv4 identity of its own.** Without one, every
  guest is the same client (`Linux NFSv4.2 (none)`), and one guest's
  mount evicts another's state. hangar-init writes the machine's uid to
  `/sys/fs/nfs/net/nfs_client/identifier` before mounting, rather than
  trust a hostname the tenant sets; a VM's mount instructions set
  `nfs.nfs4_unique_id` the same way.
- **A share, its server and every machine using it are on one node**, the
  share's: the scheduler binds the share when its server is placed, and
  a machine declaring a share goes only to that node, as a machine with a
  bound Disk does. There is no network between nodes yet; when there is,
  a share's traffic between nodes must be encrypted (RPC-with-TLS,
  RFC 9289, with certificates per machine), before any crosses one.

### Networks and who may mount

- **A share is on one to eight networks of its project**
  (`spec.networks`, `[default]` when empty), and its server has an
  interface on each, with an address the apiserver assigns as it assigns
  every machine interface's (`status.addresses`, one per network). A
  client mounts the address on its own network. One share on several
  networks is how a cache is shared between segments, as an enterprise
  NAS has an interface on each VLAN, an Azure storage account a private
  endpoint in each VNet, and an EFS file system a mount target in each
  subnet. The alternative, a client with a second interface on the
  share's network, makes that client a bridge between the networks.
- **The server can never connect to anything.** On the host, in each
  network's `bridge hangar_sg` (`networking.md`), the server's taps take
  only new TCP connections to 2049 from the machines allowed below, and
  send only replies on connections made to it; with the binding every tap
  has, they send only from their own address and MAC. A server whose
  guest is taken can answer its clients and nothing else: it reaches no
  machine and no other network, and what passes between the networks of
  a share is the share's files, which is its purpose.
- **A machine mounts only the shares it declares.** A container declares
  one as a volume, `sourceKind: FileShare` (`container-volumes.md`); a VM
  in `spec.fileShares` (`name`, `readOnly`). A declaration naming a share
  on none of the machine's networks is refused at admission, naming the
  share's networks; one naming a share that does not exist yet holds the
  machine back, as a Disk does. The server's export lists exactly the
  declaring machines' addresses on its networks, each `rw` or `ro`, and
  the node keeps it so over vsock as machines come and go; the server
  drops its client caches with each change, so a machine that stops
  declaring a share loses it at once, its mount included: its next
  operation fails (within a second on the testbed). Writes it had
  buffered on files already open never land, though its program may not
  learn so until it syncs or closes, and its opens and locks hold others
  back until its lease runs out, at most 15 s.
- **The declaration is the opening.** A declaring machine reaches its
  share's port 2049 whatever its security groups allow, as a
  `PublicEndpoint` is its own opening (`public-endpoints.md`); nothing
  else reaches the server, and security groups cannot open more to it.
  An egress-locked sandbox reaches a share only if it declares one.
- **`readOnly` is enforced by the server**, by the client's `ro` export,
  not by the client's mount options alone, which the client's root can
  change.

### Identity

- **AUTH_SYS, no squashing unless the share asks** (`spec.squash`:
  `None`, the default, `Root`, or `All` with `anonUid` and `anonGid`).
  Every client of a share is a machine its own tenant declared, and a
  guest's root is the tenant's (`guest-agent.md`); entrypoints that
  `chown` their data directory as root, as many images' do, work as
  they do on a Docker NFS volume. Filestore's default is the same; `All`
  gives what an EFS access point does.
- **Containers mount a share `nosuid,nodev`**, always, so a setuid file
  one machine's root writes is never one another machine's user runs. A
  VM mounts its shares itself; the console's mount command carries the
  same options, and `timeo=50`.

### The API

- `FileShare` is a project-scoped kind of the `storage` group: `spec`
  holds `networks`, `size` (whole Gi, at least 1Gi, grown and never
  shrunk, as a Disk's), `squash`, `anonUid` and `anonGid`; `status` holds
  `phase` (`Pending`, `Available`, `InUse`), `node`, `size`, `addresses`
  and `users`, each the kind and name of what declared it, its machine,
  the network it mounts from, `readOnly` and, for a container, its
  target. `users` is plural where a Disk's `attachment` is singular.
- **Networks can be added and removed.** The server starts again on its
  new interfaces; NFS mounts are `hard`, so its clients pause for the
  seconds that takes and carry on. A network a declaring machine mounts
  from cannot be removed, nor can a network with a share on it be
  deleted, as one with a machine on it cannot (`networking.md`). A
  project being deleted takes its shares with it.
- **Growing a share starts its server again** on the grown zvol, which it
  grows its filesystem to, as a grown Disk restarts its machine.
- **Deleting a share any machine declares is refused**; a deleted share
  waits for its server to be gone and its node to report its zvol
  released, as a Disk does.
- **A share is never filled from an image.** A container's new Disk
  volume takes what the image has at its target (`container-volumes.md`);
  a share does not, as Kubernetes' NFS volumes do not: replicas starting
  together would race to fill it. `volume.subpath` mounts a directory of
  it, which must exist.
- **A VM mounts its shares itself**: the platform authorizes it and gives
  the address, and writes nothing into the VM's operating system, as EFS
  and Filestore leave mounting to the instance.
- Changing a container's shares restarts it, as changing its volumes does;
  changing a VM's changes only the export.

### Quota

- **A share's size counts toward the tenant's `disk` limit**, and the
  Plan gains `fileShares`, how many shares a tenant may hold (3 on the
  default plan). The server counts toward no tenant limit: it is the
  platform's, as a network's resolver is, bounded by `fileShares`; the
  scheduler still reserves its memory on the node.

### Console

- Shares are listed beside Disks. Their pages make the networks the
  first thing seen: a share is created by choosing its networks as cards,
  each saying how many machines are on it, and its page gives each
  network's address and mount command, which machines use it from which
  network, and which machines cannot and why.
- **Where a container picks a share it cannot reach**, the share is shown
  disabled, with the reason beside it and two ways out, in this order:
  add the container's network to the share, which opens no path between
  networks but changes a share others use (and needs the right to apply
  shares); or give the container an interface on the share's network,
  which changes the container alone but makes it a bridge.
- Every inline list of machines shows at most two names and the rest
  behind "+N more"; tables show the first rows and "Show all N".

## Consequences

- A share costs a small VM's memory on its node, the platform's to pay,
  and a share's server is a machine the node runs, restarts and sweeps
  like any other.
- The platform kernel carries `nfsd` and the NFSv4.1 and 4.2 client
  (`guest-kernel.md`), and the server's PID 1 answers `nfsd`'s upcalls
  and tracks its clients itself; the PoC on the testbed needed no C
  userspace for either.
- A share is crash-consistent only, as a zvol is; its snapshots and
  backups come with the roadmap's, and the server, being the platform's,
  can freeze its own filesystem for them.
- NFS is fine for large files and slow for small-file metadata and heavy
  fsync (`storage-zfs.md`); a share is for sharing data, not a working
  directory. On the testbed, large reads and writes ran as fast as on a
  Disk, while creating small files ran about 50 a second (a Disk without
  fsync: 24,000), since each create is committed to the zvol before it
  is answered.
- Isolation inside one network now rests on each tap receiving only its
  own unicast (`networking.md`), not on addresses trusting each other.
