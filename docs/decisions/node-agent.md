# The node agent keeps no state; the host is read back

Status: accepted, 2026-10.

## Context

`hangar-node` runs as root and changes the host: disks, namespaces, taps, VM
processes. It can crash or be restarted between any two steps, and a machine
can be deleted while it is down. A local record of what it made could disagree
with the host after any of these, and nothing would notice.

## Decision

- **No state of its own.** Every object the agent makes on the host is named
  after the uid of the API object it serves (a machine's root disk, interfaces
  and VM; a network's namespace; a disk's volume), and every host driver
  lists what it made. A reconcile compares what the API wants with what the
  host has.
- **A sweep removes what nothing accounts for**: VMs, root disks and
  interfaces of machines that are not on this node any more, volumes of
  Disks and FileShares that are not bound to it any more, then networks
  nothing on the host uses, disk images' bases nothing uses or is being
  built from, unless an Image names them (`docs/decisions/vm-images.md`),
  and container images past the image cache's bounds (below). It runs at
  start and
  every minute, under the same lock as the reconciles. A list it
  could not read, from the API or from a driver, stops whatever depends on
  it: a failed list must never read as "nothing is wanted", since a driver
  may delete what it is told to even while it is in use.
- **The host sits behind five drivers**: images, storage, network, hypervisor
  and guest agent (`internal/node/driver`), whose contract is the conformance
  suite `driver/drivertest`. A fake of all five simulates one host with fault
  injection (`driver/fake`), which the hermetic end-to-end tests run on and
  which the suite holds to the contract; systemd units are a detail of the
  real hypervisor driver. `hangar-node --drivers=host` runs on this host's
  drivers: `registry`, `zfsstore`, `hostnet`, `chvm` and `chguest`.
- **Each VM is a transient systemd unit, `hangar-vm-<machine uid>.service`,
  whose main process is the launcher, `hangar-node vmm <uid>`**
  (`internal/node/driver/chvm`), so a VM outlives the agent: the agent
  crashing, restarting or being upgraded leaves it running, and a new
  agent reads it back. Each VM has a directory, `/var/lib/hangar/vms/<uid>`,
  with what the launcher runs: the spec, written whole by `Start`, and the
  boot disk. On each start the launcher checks with `BLKZNAME` that the
  root disk's link leads to the machine's own zvol (a stale link can lead
  to another's, `storage-zfs.md`), opens the taps, and runs Cloud
  Hypervisor on them with the platform kernel and initramfs, hybrid vsock
  at `vsock` in the directory, and the console on its stdout. It keeps the
  console in `console.log`, rotated at record boundaries to `console.log.1`
  past 16 MiB, writing only well-formed records, so nothing the guest
  writes can wedge it or grow the file past its cap, and numbers the boot
  in it, at its start and at the head of each file it rotates to
  (`node-tunnel.md`); the kernel's serial
  console goes to `serial.log` the same way, past 1 MiB. A log that fails,
  as on a full disk, drops what it cannot take and is tried again, so the
  guest is never held up by it. When the guest
  powers off, the launcher records how its process ended in `exit.json`
  and exits with the process's exit code (253 when the guest recorded
  none, as after a crash; 252 when the launcher could not start the VM),
  so systemd's `Restart=` and its restart count see the container. A
  container exiting with 252 or 253 itself reads the same; `exit.json`
  says which (`Error` for the launcher's failure).
- **Cloud Hypervisor runs confined to its VM** (decided 2026-10), as
  Firecracker's jailer confines Firecracker: as a uid of the VM's own,
  with no capability, in a unit with an empty root that holds its VM's
  directory and little else. "A VM's confinement" below says what each
  measure stops, and what is left.
- **The exit code and the log are the guest's claim.** The guest's root
  is the tenant's, who can write any records on the console: a forged
  exit record, or a header of a legal length that swallows what follows,
  hangar-init's own exit record included. The launcher reads them as they
  are, so a guest can set its own exit code and garble its own log, and so
  its own restart policy's decision; nothing it writes reaches another VM,
  stops the launcher, or grows the log past its cap.
- **The restart policy is systemd's**: `always`, `on-failure` or `no`, the
  first restart after a second, then longer, up to a minute, and never
  given up. A VM is listed running while its unit is active or waiting to
  restart, and exited (with its code) when its exit is recorded and its
  unit is gone.
- **A VM interrupted is started again by its policy** (decided 2026-10):
  one whose unit is gone with no exit recorded and no stop asked, as at a
  host reboot (units are transient) or with its launcher killed outright,
  is not listed under `always` and `on-failure`, so the agent starts it
  again, and is listed exited with 253 under `no`.
- **A host shutdown stops each guest as the agent would** (decided
  2026-10): systemd stopping a VM's unit by itself, with no stop mark,
  makes the launcher ask the guest to stop with the grace and slack the
  driver started the VM with (kept in the spec), wait for it to power off,
  and only then stop Cloud Hypervisor, so the container gets its SIGTERM
  and grace and its handler's output is in the log. A unit's stop timeout
  covers each step of a stop at its longest (25 s by default), and it
  stops before `network.target`, so the guest has its network meanwhile;
  the default dependencies keep the mounts (its directory, the networks'
  namespaces) until it has stopped. It is ordered `Before=hangar-node.service`
  (the name `deploy/host` installs), so the agent has stopped before any VM
  does and never starts a VM the shutdown stopped; the order binds only
  jobs in one transaction, so restarting the agent or starting a VM is not
  held up by it. The launcher records no exit for such
  a stop, whatever the container exited with: the VM reads as interrupted
  above, never as a container that ended by itself.
- **Stopping is docker stop** (`guest-agent.md`): the agent asks the guest
  to stop its process, waits up to the 10 s grace and 5 s more for the
  guest to power off, then stops the unit, which kills Cloud Hypervisor; a
  guest that does not take the stop within 3 s is killed at once. Before
  asking, it marks the VM as being stopped, and a launcher whose guest
  powers off while marked waits to be stopped instead of exiting, so a
  stop never restarts a VM, whatever its policy; a launcher stopped while
  marked kills Cloud Hypervisor at once, the guest having been asked
  already. A stop removes the unit
  and the VM's directory but its logs. A stop cut short leaves the mark;
  starting the VM's spec again while it runs clears it, so its next exit
  is its policy's again.
- **A VM's logs outlive it**: its restarts, a stop and a start again. They
  go when the machine is released, or when the sweep finds them of no
  machine on the node. The hypervisor driver reads them back, both files
  as one stream, for the apiserver, which asks for them over the agent's
  tunnel (`node-tunnel.md`).
- **The driver touches only its own**: units named `hangar-vm-<uid>` for a
  uid it takes, and its directory.
- **The node follows the API like a controller** (`controller.APISource`),
  with Machines placed on it and their Networks, Disks and FileShares as
  the watched kinds. What the host notices on its own (a VM exiting,
  networkd removing a link) checks every machine again. A second
  controller keeps the volumes of the Disks bound to the node, at the size
  their status says, and removes a deleted one's once no machine lists it,
  reporting `Released`. A FileShare's volume is its server's machine's to
  make and grow, as the server boots on it (`file-shares.md`); a third
  controller, `fileshares`, destroys a deleted share's once its server is
  gone (its `dependents` finalizer), and reports `Released`.
- **Nothing slow runs under the host lock.** Reconciles and the sweep take
  turns under one lock, so each step under it must be short. Preparing an
  image (resolving, pulling, making its base) can take minutes, so a puller
  does it off the lock, once per image however many machines want it; a
  machine waiting on its image says it is pulling, while the others are
  built, stopped and deleted meanwhile. Fetching an image into the cache
  takes no turn: a registry that stalls is given up within about half a
  minute (no answer, or a body slower than 30 KiB in 30 s), and holds up no
  other image. Unpacking it, the host's disk and CPU work, takes a turn: at
  most `--max-pulls` images (two by default) are unpacked at once, the
  others waiting in the order they came, saying they are pulling, and
  giving up their turn if no machine wants them any more. Unpacking one
  image takes at most `--max-unpack-work` steps of work (15M by default,
  about 680,000 ordinary files; `storage-zfs.md`), past which it is too
  large. A job that fails is tried again within a minute; one that timed
  out (its own half hour, or a registry's stall) from a minute, twice as
  long each time, up to half an hour. An image at fault (too large, or
  holding what the host refuses to unpack, but not a cached blob the disk
  failed to read, which is dropped and fetched again) is never unpacked
  again: a pinned digest is not tried again until the machine names
  another image, and a tag is resolved again, with the backoff, so an image
  fixed and pushed under the same tag is pulled. A disk image's base
  (`vm-images.md`) is made the same way, off the lock, once per digest
  however many disks want it, by the disk controller, its disks saying
  `Pulling` meanwhile; an image at fault is not fetched again until its
  Image's generation changes. Asking a guest agent
  whether it answers is bounded to two seconds; a guest that once lets
  that run out is asked off the lock from then on, one ask at a time, until
  it answers (every machine is then checked again) or its VM starts again,
  its machine saying `Booting` meanwhile, so a hung guest
  holds up the others once per boot, not at each of its reconciles. One
  worker then serves every machine in turn.
- **A VM is stopped off the host lock.** A stop takes as long as its guest
  takes: about 20 s at worst (3 s to ask the guest, the 10 s grace and 5 s
  of slack, 2 s for the launcher to kill Cloud Hypervisor). Whatever needs
  a VM stopped (its machine stopped, deleted or changed, or the sweep)
  begins the stop under the lock and lets it run in the background; when it
  succeeds, every machine is checked again, and the step that wanted it
  goes on. A machine with no VM listed is not stopped at all. A stop that
  fails is reported on its machine and begun again after a second, twice as
  long after each failure in a row, up to a minute, however often the
  machine is checked meanwhile; it checks no other machine. While a
  machine's stop runs, nothing else touches it: its reconciles only wait,
  so it is neither started again nor stopped twice, and the
  sweep passes by its VM, disk, interfaces and logs, which go at the first
  sweep after the stop. What the agent keeps of a stop under way is lost
  with it; a stop cut short is found again, as the VM still listed or the
  driver's stopping mark, and done again.
- **An image reference is resolved once**: the reference and its digest
  are kept in the machine's status, and resolved again only when the spec
  names another image. A tag moving upstream never rebuilds a container's
  disk, whatever else changes. A new image rebuilds the root disk (see
  `machine-model.md`); any other change restarts the VM on the disk it has. A
  digest the host has pulled is read from its cache until it is evicted
  (below), so a running machine never depends on the registry. The cache
  answers a digest only for the repositories it was fetched from in full; a machine
  naming it from another fetches it in full from there first. A machine
  with image pull secrets resolves its reference with its own login, in a
  pull of its own; only machines without credentials share a pull, which
  resolves anonymously, and a digest in a machine's status needs resolving
  no more (`secrets.md`).
- **A container image outlives its last machine** (decided 2026-10-20), as
  kubelet's image garbage collection keeps images: deleting a container,
  rebuilding it with a changed spec, or making it again later finds its
  image's base and cache record still there, and fetches no blob again
  (the reference is still resolved, as a new machine's always is). Each
  digest has a last use, kept on the host so the agent's restart keeps it:
  the base's snapshot records when it was made, last cloned from, and last
  had a root disk cloned from it deleted, so an image a machine ran on for
  months is not the first to go once it stops (`hangar:used-at`), and the
  cache record's modification time when
  `Config` or `Unpack` last read it; the later of the two counts. A digest
  a root disk, a pull under way or a machine waiting for it uses is never
  evicted, judged at the eviction, not as the sweep began (a finished pull
  of a machine gone uses nothing); nor is one the cache says is being
  pulled, which eviction passes by, base and record, never waits for, and
  leaves to the next sweep. The sweep evicts the others, base first and
  then cache record, so a base that will not go keeps its image whole,
  least recently used first,
  while they take more than `--image-cache-high` (20 GiB by default; their
  blobs, those no digest in use holds, and their bases' `used`), down to
  `--image-cache-low` (four fifths of it by default, so one sweep frees a
  few images, not one at each sweep); and while the cache's blobs take
  more than 85% of the image dataset, down to 80%, kubelet's thresholds.
  Disk images downloaded and images being unpacked, which share the
  dataset, do not count: they go by themselves. A step of a pull with too
  little room evicts in the same order, then is tried once more: fetching
  the blobs into the cache, and unpacking beside them, both in the image
  dataset, and making the base, on the pool (decided 2026-10-20). What is
  freed is counted by what the images evicted take as the drivers list
  them, their blobs or their bases' `used`, never by what is free read
  again: ZFS shows freed blocks free some seconds later, and counts the
  bytes compression leaves, which no measure of an unpack foretells.
  Fetching the blobs and making the base know what they lack, and evict
  until that much is freed. An unpack that runs out is known to need only
  more than it had: each try evicts what frees its image's compressed
  size, and one still short evicts as much again at the next. If what a
  step is known to need is more than its room holds empty, or evicting
  every image no machine uses would not free what it is known to lack,
  none is evicted, and the pull fails as the disk's. An unpack is never
  known to need more than its room holds, so one that could never fit
  evicts its compressed size of the images no machine uses at each try,
  until none is left. A digest
  evicted and asked for again is pulled again, as any digest the host
  lacks. The bases kept sit on the pool, so `deploy/host` counts
  `--image-cache-high` in `--reserved-disk`
  (`docs/runbook/host-bootstrap.md`).
- **What runs keeps running.** A step the agent cannot check again (the
  registry, a driver list, the network's object) leaves the status of a
  machine running its current spec as it is, with one exception: once the
  network driver has signalled a change, a machine whose networking cannot
  be checked is reported cut off until it is found whole. Stopping needs no
  image and no network. A tap gone from under a running VM (networkd
  removing a link) is made again and the VM started again on it, since the
  tap it held is dead.
- **The node owns the machine's status except `Scheduled`**, and writes it on
  the copy it read, conditional on its resource version. Its `Ready` condition
  says what stands in the way, and the container passes that on.

## A VM's confinement

The threat: the guest is the tenant's, and a bug in Cloud Hypervisor can
give the guest Cloud Hypervisor's process. From there it must reach
nothing of the host and nothing of another VM: not its files, disk,
socket or process.

**The VM's user.** The driver gives each VM a uid from 200000 on, 65536 of
them (`DefaultVMUsers`), which is also its gid: the lowest that no VM's
`spec.json` holds and no process runs as. `spec.json` records it, so a VM
keeps it across its restarts, the agent's and the host's; `Start` takes the
one its `spec.json` already holds. A stop removes `spec.json` last, after
everything else of the VM's but its logs, which are root's, and only once
the unit has stopped: an id is given out again only when no process, file
or socket of its last VM is left. A VM could write nowhere but its own
directory, so there is nowhere else for one to be. The agent refuses to
start on a host where a user, a group, or a subordinate range other than
`hangar-vms`'s in `/etc/subuid` and `/etc/subgid` has one of these ids;
`deploy/host` keeps them there under that name.

**What the launcher does as root, and why.** The launcher, the unit's
main process, is root, with only the capabilities these need: it opens
the root disk (checking it by `BLKZNAME`) and holds it open, `O_EXCL`, for
the VM's life, so no other zvol can take its device number; makes a node
of that device in the VM's directory (`CAP_MKNOD`); enters the networks'
namespaces and opens the taps (`CAP_SYS_ADMIN`, `CAP_NET_ADMIN`), then
unmounts `/run/netns` from the unit; gives the VM's directory and what
Cloud Hypervisor reads to the VM's group (`CAP_CHOWN`); runs Cloud
Hypervisor through `hangar-node vmm-exec` (`CAP_SETUID`, `CAP_SETGID`,
`CAP_SETPCAP`). Once Cloud Hypervisor runs, the launcher drops all of them
but `CAP_DAC_OVERRIDE`, to reach its socket, the VM user's, and
`CAP_KILL`, to stop it, from the bounding, permitted and effective sets of
every thread, and checks that it did; if it cannot, it kills Cloud
Hypervisor and fails. A restart under the VM's policy starts a new
launcher, with the unit's set again. Cloud Hypervisor re-opens its disks by
path, `/dev/fd/N` included, which checks the file's permissions: a node
of the VM's group is what a fd would be to root. `vmm-exec` marks every
fd but the taps' close-on-exec, sets `no_new_privs`, empties the
bounding, inheritable and ambient sets, takes the VM's uid and gid with
no group but the KVM device's, and checks all of it in
`/proc/thread-self/status` before it execs Cloud Hypervisor, with no
environment. Cloud Hypervisor keeps its own seccomp filter. A firmware
VM's console helper, `hangar-node vmm-console`, is run the same way, before
the capabilities go, with no group but the VM's (`serial-console.md`). So
is a TPM VM's swtpm, before Cloud Hypervisor, from Hangar's bundle under
its own static loader, which the unit binds read-only, with its control
socket, its key on a pipe and its log on fds 3 to 5, and the host's
`nr_open` as its RLIMIT_NOFILE (`vm-tpm.md`). That unit's hard
`LimitNOFILE` is `nr_open` for this, so its launcher and Cloud Hypervisor
may raise their soft limit that far too. The launcher reads the key
from the unit's credential, `LoadCredential=vtpm:/run/hangar/vtpm/<uid>`,
which the agent writes before it starts the VM and removes once it has
stopped it.

**The VM's directory** is root's and the VM's group's, `0710`: Cloud
Hypervisor reads `boot.img` (`0640`), opens `disk` and a container's
`serial.fifo` through the group, and writes only in `ch/` (`0770`), made
afresh on each run, where it makes its sockets. A firmware VM's
`console.sock` is the launcher's, `0600`, listened on by it and served by
the console helper. It can neither read `spec.json` or the
logs, nor replace them. The agent and the launcher connect to the socket
only as a socket, never through a link Cloud Hypervisor put in its place.

| Measure | What it stops |
|---|---|
| A uid and gid of the VM's own | Signalling, tracing, or reading the files and sockets of another VM's Cloud Hypervisor, which is another user; every host file not open to all |
| No capability, `no_new_privs`, empty bounding set | Getting root's powers back by any exec, setuid file or file capability |
| Empty root (`TemporaryFileSystem=/:ro`, `MountAPIVFS`), only the launcher, Cloud Hypervisor, the kernel, the initramfs and `/run/netns` bound read-only, and the VM's directory read-write; `/run` read-only | Reading any host file, even one open to all (`/etc`, `/home`, `/var/lib/hangar`), and seeing other VMs' directories; writing anywhere but `ch/` |
| `DevicePolicy=closed` with `/dev/kvm`, `/dev/net/tun` and the root disk alone | Opening another zvol, a host disk or any other device, even as root in the unit, or as the KVM group (`/dev/vhost-*`) |
| `/dev/shm`, `/dev/mqueue`, `/dev/pts`, `/dev/ptmx` inaccessible; `PrivateIPC` | Leaving shared memory or queues that outlive the VM; using up the host's ptys |
| The host's `/dev`, which `MountAPIVFS` binds, read-only (`ReadOnlyPaths`) | Making, removing or renaming anything in the host's `/dev`, even as root in the unit; its devices still open for writing |
| `PrivateNetwork`, `IPAddressDeny=any`, `RestrictAddressFamilies=AF_UNIX AF_NETLINK` | Any network but its taps' fds, and the host's abstract sockets |
| `ProtectProc=invisible`, `ProcSubset=pid` | Seeing the host's other processes and its kernel settings |
| `ProtectKernel*`, `ProtectControlGroups`, `ProtectClock`, `ProtectHostname`, `LockPersonality`, `MemoryDenyWriteExecute`, `RestrictRealtime`, `RestrictSUIDSGID`, `RestrictNamespaces=net`, a system call deny list, `LimitCORE=0`, `KeyringMode=private` | Changing the kernel, the clock or the hostname; keys a later VM of the same uid could read; a core holding the guest's memory |
| `MemoryMax` (the guest's memory, 256 MiB, and 2% of the guest's, 16 MiB more for a TPM VM's swtpm), `MemorySwapMax=0`, `TasksMax` (64 and two a vCPU, 128 a disk Cloud Hypervisor serves for its io_uring's workers, 16 more for a firmware VM's console helper, 8 more for swtpm) | Taking the host's memory or tasks |

`MemoryMax` was measured (2026-10, `TestMemoryOverhead` in
`test/testbed/vms`) with guests of 1, 4 and 16 GiB filling half their
memory: past what the guest used and what its kernel keeps out of its
`MemTotal`, the unit took at most 13, 9 and 36 MiB, mostly KVM's page
tables, which grow with the guest; a guest that barely ran peaked at 113,
239 and 463 MiB, its kernel touching its memory map. 256 MiB and 2% (584
MiB past a 16 GiB guest) leave room for that many times over.

That room holds no page cache of the guest's disks. Cloud Hypervisor opens
every zvol with `direct=on` (O_DIRECT); opened buffered, the guest's I/O
went through the host's page cache of the zvol, which is charged to the
VM's unit, and the guest's memory once touched leaves the cache no room
but past the limit. At `MemoryMax` the kernel reclaimed the guest's memory
with the cache, into the host's swap: in 2026-11, 18 of the host's 28 VM
units sat at their limit, the host's swap was 5.6 of 8 GiB, nearly all of
it `hangar.slice`'s, and 767 GiB had been swapped in over 16 days, each
page a stall of the guest's. On a testbed (kernel 6.8, OpenZFS 2.3.9),
`TestAGuestsDiskStaysOutOfItsUnitsMemory` in `test/testbed/vms`, a 2 GiB
guest with 1.5 GiB of its memory in use reading 2 GiB from its disk for a
minute, saw its unit's page cache reach 1287 MiB and 1014 MiB of its
memory swapped out with the zvols opened buffered; with `direct=on` and
`MemorySwapMax=0`, the cache stayed at 23 MiB at most, nothing swapped and
the unit stayed under its limit. A first experiment of the same kind
(OpenZFS 2.2.2, a 3 GiB file read for three minutes) swapped out 1 GiB
under the multi-generational LRU and under the classic one alike. The
cache bought nothing: the ARC caches the zvol already, and OpenZFS 2.3's
Direct I/O does not apply to zvols, so O_DIRECT reads still hit the ARC
(no ARC miss in the runs below). Measured with fio on the testbed, on a
zvol made as the host makes them (16K blocks, lz4, OpenZFS 2.3.9,
io_uring, the ARC warm) but in a pool on a loop file in a nested VM, not
the host's NVMe mirror, O_DIRECT against buffered: 1 MiB reads 9.1
against 3.7 GiB/s, 4 KiB random reads at depth 32 349k against 232k IOPS,
1 MiB writes 657 against 540 MiB/s but their p99 latency 426 against 80
ms, 16 KiB random reads and writes 33k against 29k IOPS each, and 4 KiB
random writes at depth 32 38k against 49k IOPS: the page cache no longer
gathers small writes, or absorbs bursts of large ones, before the zvol
takes them. The files a VM reads, its boot disk, a converter's file and
the tailscale disk, stay buffered: they are read-only, small but for a
converter's file, and their cache is reclaimed without swapping.

`MemorySwapMax=0` keeps the guest's memory out of the host's swap
whatever else fills the unit: at its limit the kernel reclaims the unit's
page cache alone, and what cannot be reclaimed is an OOM kill of the VM
rather than a guest stalling on memory the host swapped out. What the unit
holds past the guest's memory was measured above, far under the room it
has. A starting agent gives each running confined VM unit
`MemorySwapMax=0` at runtime, as it does `TasksMax` below; what such a unit
had swapped out comes back as the guest touches it, and a VM whose
launcher started it without `direct=on` keeps its zvols buffered until it
is started again.

`TasksMax` has room for every disk's I/O workers. Cloud Hypervisor serves
each disk from one queue, an io_uring as deep as the queue (128), and a
zvol takes no I/O without blocking, so the kernel hands each request to an
io-wq worker, a thread of Cloud Hypervisor's in the unit, up to
`min(ring depth, 4 × host CPUs)` of them per ring. A ring without a worker
that cannot make one within `TasksMax` fails every request queued on it
with `ECANCELED`, which the guest reads as an I/O error. In 2026-11, under
a limit of 64 and two a vCPU, a container's `/workspace` failed so for 5 s
and ext4 went read-only (hangar#259); 22 of the host's 26 VM units had
reached their limit. A probe on kernel 6.8, filling one ring's workers to
the limit and then queueing on another, saw every request of the second
fail so; with room for both rings' workers, none did. A starting agent
gives each running confined VM unit the `TasksMax` its launch has now, at
runtime, so a VM started under a smaller limit takes the new one without
restarting.

A VM's task ceiling therefore grows with its disks, which its owner
chooses: a container mounts at most 8 volumes (`MaxVolumes`), so it has at
most 11 disks and about 1,500 tasks; a VM's disks have no count limit, and
nothing caps the tasks of `hangar.slice` as a whole. Each worker's kernel
stack (16 KiB) and task are charged to the unit's memory: about 4 MiB a
disk, 44 MiB for an 11-disk container, computed rather than measured
(`TestMemoryOverhead` ran under the old limit), within `MemoryMax`'s 256
MiB.

Every VM has a balloon that is never inflated, `--balloon
size=0,free_page_reporting=on`: the guest reports the pages it frees and
Cloud Hypervisor gives them back to the host, so a running VM holds what
its guest uses, not the most it ever touched. Without it, guest memory is
private anonymous memory faulted in on first touch and held until Cloud
Hypervisor exits. Measured (2026-10, `TestFreedMemoryReturns` in
`test/testbed/vms`), a 2 GiB container that filled and freed 1 GiB fell
from 1114 to 175 MiB within 30 s, where without the balloon it stayed at
1115 MiB. What the guest keeps in its page cache is in use and stays: it
returns only once the guest drops it. The platform kernel has
`CONFIG_VIRTIO_BALLOON` and `CONFIG_PAGE_REPORTING`; a firmware VM's
operating system needs its own, as Ubuntu 24.04's has: a 2 GiB Ubuntu VM
that filled and freed its 983 MiB `/dev/shm` fell from 1782 to 859 MiB
within 60 s, and without the balloon stayed at 1837 MiB (measured once,
by hand, with a unit planted in the cloud image). Placement still
reserves a running machine's whole memory, so this frees the host's memory
for its page cache and the ARC, not for more machines; a machine its node
has stopped reserves none (`machine-model.md`).

Not `ProtectSystem=strict`, which binds the host's root again over the
empty one; not `PrivateTmp` or `ProtectHome=tmpfs`, which add directories
Cloud Hypervisor could write; not `ProtectHome=yes`, which drops binds
under `/home`. The launcher and Cloud Hypervisor must therefore be static:
the unit has no dynamic loader. `TestCHIsConfined` in
`test/testbed/vms` runs a probe in Cloud Hypervisor's place, in a real VM's
unit, and checks each of these from there.

**What is left.**

- The launcher runs as uid 0 for the VM's life, with `CAP_DAC_OVERRIDE`
  and `CAP_KILL` once Cloud Hypervisor runs, and reads what the guest
  writes on its console (`console.log`'s records, see above). A bug in
  that reading is a way from the guest to that launcher: it could read
  and write any file the unit sees, the VM's directory and the binds
  (read-only), and signal any process of the host by its pid (the unit
  has no pid namespace of its own), but not mount, enter a namespace, make
  a device, or get a capability back.
- Cloud Hypervisor still reaches the kernel through KVM, its system calls
  (under its filter and the deny list) and `/dev/net/tun` in an empty
  network namespace; a kernel bug there is a way out.
- It can fill `ch/`, on the `hangar/vms` dataset, up to that dataset's
  quota, and its own memory up to `MemoryMax`.
- A VM started before VM users runs as root, unconfined, until it is
  started again (stopped and started, its spec changed, or the host
  rebooted). Restarting under its policy keeps its unit, which says root;
  the launcher then runs Cloud Hypervisor as root as before.
- `/proc/sys` and `/sys` are read-only but readable, and the unit sees
  the host's devices' names in its read-only `/dev`.

## CPU shares

A VM's vCPUs are shared, never reserved: the scheduler holds a machine's
memory and disk, not its CPUs. So that no mix of shapes changes how far the
host's CPUs are shared, each VM's share of them under load follows its
memory, or, in a memory pool, its guarantee (#177):

```
 -.slice
 ├─ system.slice   CPUWeight 100   every CPU
 ├─ user.slice     CPUWeight 100   every CPU
 └─ hangar.slice   CPUWeight 1000  AllowedCPUs: all but --reserved-cpu
     ├─ hangar-vm-<uid>.service    CPUWeight: memory / 256 MiB
     └─ hangar-pool_<pool>.slice   CPUWeight: its VMs' together
         └─ hangar-vm-<uid>.service  CPUWeight: guarantee / 256 MiB
```

- **Every VM's unit runs in `hangar.slice`** (`Slice=`), beside
  `system.slice`, so the VMs share what the slice gets among themselves
  alone. The name has no dash: systemd reads `hangar-vms.slice` as a slice
  inside `hangar.slice`, and a weight counts only against its siblings.
- **A VM's `CPUWeight` is one per 256 MiB of its memory**, within the 1 to
  10000 systemd takes: 2 for 512 MiB, 16 for 4 GiB. A VM in a memory pool
  is weighed by its guarantee instead (below), so a pool letting its
  machines' memory add up to twice its size does not double its tenant's
  share of the CPUs; a pool's slice weighs what its VMs do together, so
  each VM has the share it would have beside the others in
  `hangar.slice`. Under full load the
  slice's CPUs are split by memory, each VM taking no more than its vCPUs
  and the rest going to the others; with CPUs idle, a VM uses them up to
  its vCPUs, as before. On a host of 30 CPUs for VMs and about 100 GiB of
  memory for them, a VM is sure of about 0.3 of a CPU per GiB.
- **A FileShare's server has a weight of 100**, a 25 GiB machine's, not
  its 256 MiB's 1: it serves every client of its share, which would wait on
  it under load (`hard` mounts stall rather than fail). With its one vCPU
  it takes at most one CPU.
- **The slice's `CPUWeight` is 1000**, against the 100 of `system.slice`
  and `user.slice`: under full load the VMs take most of the CPUs they may
  use, and the host's services still get some of them.
- **`--reserved-cpu` CPUs are the host's alone**: the slice's `AllowedCPUs`
  is every online CPU but those, taken whole cores first, a core's SMT
  siblings together (`MachineCPUs`, from `/sys/devices/system/cpu`), so no
  vCPU shares a core with the agent, the apiserver or ZFS. The host's
  services may still use every CPU, at their weight. The flag also comes
  off the node's allocatable cpu, as before. Without it the slice has
  every CPU. `deploy/host` passes `HANGAR_RESERVED_CPU` when the host env
  file sets it.
- **The agent owns the slice.** As the hypervisor driver starts it makes
  `hangar.slice` a transient unit with its weight and CPUs, or sets them on
  the one loaded already, at runtime, so a changed `--reserved-cpu` takes
  effect when the agent starts again, and no file of the deploy's holds a
  second copy. A VM's own weight is set when its unit starts: a VM
  started before the slice existed runs in `system.slice`, unweighted,
  until it is started again.

`TestTheVMsShareTheirSliceByMemory` in `test/testbed/vms` reads, on a real
host, the slice's `cpu.weight` and `cpuset.cpus.effective`, each unit's
`cpu.weight`, and the CPUs its launcher may run on: systemd enables the
cpuset controller no further down than the slice, whose cpuset binds the
units' processes.

## Memory pools

A tenant's machines may share its memory as a pool
(`docs/decisions/memory-pool.md`): together they may be given up to
twice, or more, the memory the pool holds on the node, and the node keeps
what they hold within it. A machine's guarantee (`memory_guaranteed`)
splits them: one guaranteed all its memory is **Dedicated**, never
squeezed, never slowed; one guaranteed less is **Shared**, and is given
only its guarantee for sure.

- **A pool's VMs run in a slice of their own**,
  `hangar.slice/hangar-pool_<pool>.slice`, the pool's name with its dashes
  as underscores, since a dash would nest the slice a level deeper. The
  agent owns it as it owns `hangar.slice`: transient, made as the first of
  the pool's VMs starts and stopped by the sweep once none runs in it.
  Its `MemoryMax` is the limit the squeeze sets for its guests and each
  unit's overhead past its guest's memory (256 MiB and 2%, and 16 MiB for
  swtpm): the backstop, never what keeps the pool within its size. The
  limit is the pool's effective size less the memory of its members
  running outside the slice, but never below what the guests in it have
  now: a size lowered brings the limit down only as the balloons bring
  them under it. A VM starting never lowers it; a pool none of whose
  slice's VMs is of it any more has none (`infinity`) until the sweep
  stops the slice. Its `CPUWeight` is its members' together, each
  member's its cgroup's, so each has the share of the CPUs it would have
  in `hangar.slice` itself. The driver reads the slice's `memory.max` and
  `cpu.weight` first and sets them only when they differ. A VM joins a
  pool, or leaves one, as it starts: one running when its machine's pool
  changes stays where it is until its next start, though its guarantee's
  weight and OOM score follow at once. Changing a machine's pool or
  guarantee restarts nothing: neither is part of what its VM runs. A file
  share's server is in no pool: the scheduler holds its memory on its own.
- **A pool's members are what runs in its slice**, read from the units in
  it, a stopping one included, and the specs they were launched with: a
  member counts at the memory it was launched with until its unit is
  gone, whatever its machine's spec says meanwhile (stopping it, not read
  yet since the agent started, or a memory shrink waiting for its
  restart). The specs say only what changes while it runs: the pool's
  size and its guarantee. A member whose spec now names another pool, or
  none, is held as Dedicated, at its whole memory, and the pool's size
  gains that much, since the scheduler now holds it apart. A running VM
  whose spec names the pool but that started before it joined, outside
  the slice, is held as Dedicated too, its memory outside the slice's
  limit. A running VM in no pool's slice whose balloon holds anything has
  it given back whole by the sweep. A pool's **effective size** is the
  least size its members' specs name, or the one they were launched with
  if none is read, but never less than what its running members are
  guaranteed together, which a plan lowered while they run can leave past
  it; the scheduler holds the same (`memory-pool.md`).
- **Each Shared guest is held at its allowance by its balloon**, so the
  pool cannot be overrun at all: what the Shared guests have of their
  memory, their memory less what their balloons hold, and the Dedicated
  machines' whole memory, used or not, never pass the pool's size. Guest
  memory, page cache included, is anonymous memory on the host, which the
  host cannot reclaim without swap, and a guest fills it at disk speed: on
  a testbed two guests filled a pool of 3 GiB in 6 s, between two polls of
  a squeeze that only reacted to what they held, and the kernel killed one
  whole at the slice's limit. A balloon caps what a guest can touch, so a
  guest at its allowance reclaims its own page cache, as on any machine
  of that size, and the host holds no more. A Dedicated machine's idle
  memory is never lent: it may take it at memory speed, faster than any
  balloon gives it back.
- **The allowances** (`internal/node/squeeze`, polled every 5 s, every
  second under pressure; a poll that cannot read a member's memory on the
  host does nothing), out of the effective size: each Shared guest holds its floor, what it uses
  (MemTotal less MemAvailable, from Cloud Hypervisor's
  `vm.balloon-stats`, a report older than 30 s none) and the larger of
  256 MiB and a tenth of its memory, or its guarantee if more; a guest
  that reports nothing holds its size. A guest whose MemAvailable is under
  the larger of 256 MiB and a tenth of its size, or whose counts of
  allocation stalls and direct and background reclaims moved since the
  poll before, needs more; one whose kernel reports no such counts, or no
  statistics at all, once its unit's `memory.current` reaches 90% of its
  size. Not MemAvailable alone, nor `memory.current`: a guest full of
  clean page cache has plenty available, and its unit holds all its size,
  whether it reads on or sits idle; its reclaims tell the two apart. While
  some guest needs more, what the floors leave goes first to those in
  need, by how far each could grow, up to all their memory; the others
  give up only what that takes, kept at their sizes as far as what is
  left allows and at their floors at the least, and anything left past
  that is handed out as below. Holding the others at their floors whatever
  was left made two guests, each in need once cut, trade the pool's room
  back and forth every poll (found in `test/e2e`, 2026-11-26). While none
  does, each keeps its size, nobody gives any back, and what the
  Dedicated memory and their sizes leave is handed out, by how far each
  could grow, so a guest has room at once. When the floors themselves are
  past what the Dedicated memory leaves, what they are past is taken from
  them by how far each is above its guarantee, never below it: the
  guarantee stage, where a guest may kill a process of its own, the Shared
  promise. The agent keeps only each guest's last count of its reclaims.
- **A guest shrinks a step at a time, and grows only into what the
  balloons gave back.** A balloon grows by at most 512 MiB a poll, 1 GiB a
  poll under pressure, about what it takes back in a second (440 MiB/s
  measured); a guest grows by as much as there is room for at once, read
  from what the balloons hold now (`balloon_actual`), never from what they
  were asked: a balloon gives memory back in 0.2 s. Those in need are
  given room first. Within 32 MiB of its allowance a guest is left as it
  is. A Dedicated machine's balloon, as one made Dedicated while squeezed,
  is given back whole, but as any guest grows: only into room the others'
  balloons freed, before anyone else.
- **A Shared VM boots at its guarantee**: its launcher gives Cloud
  Hypervisor `--balloon size=` its memory less its guarantee, which its
  pool always has room for, since the guarantees of a pool's machines
  never add up past it, without the squeeze knowing what the others have.
  Its guest's balloon driver inflates that as it boots, in some seconds:
  4.2 s after a 4 GiB container's guest answered, for 3 GiB of balloon,
  and until then the guest may use all its memory. A container's guest
  waits for it before it starts anything of the container's
  (`guest-agent.md`), so its start is put off by those seconds (3.9 to
  5.7 s measured). For 15 s after its VM starts, hangar-init's wait for
  it, while its guest has more than its guarantee and 64 MiB and has not
  yet been seen at it, the squeeze leaves its balloon to it, counts it at
  its guarantee and polls soon; then gives it its allowance. A container
  whose wait ran out runs on, counted at what it has.
  A VM started into a pool its others filled waits, at its guarantee,
  while their balloons take back its share. **A Shared firmware VM has a
  boot window**: its operating system is its own and does not wait, so for
  the few seconds its balloon takes (4.2 s measured), a fill fast enough
  can reach the pool's slice's limit, where the backstop kills a Shared VM
  whole, by score.
- **A balloon that does not follow is given up.** A member's balloon that
  for a minute is neither within 32 MiB of what it was last asked nor
  moving 16 MiB a poll toward it, as a guest with no balloon driver or a
  hung one, is asked nothing more until its VM starts again, counted at
  what its guest has, and keeps its pool hot no more. The agent logs it at
  Warn, counts it in `hangar_node_memory_pool_balloons_unresponsive_total`,
  and the machine says `BalloonResponsive` False, `BalloonUnresponsive`,
  while so.
- **Pressure wakes the squeeze.** A pool's VM starting, and the pool's
  slice's `memory.events` moving (watched with inotify), poll at once,
  never more often than every 200 ms. While a guest needs more, or a
  balloon has further to go, the next poll is a second later.
- **Not `memory.high`** (tried 2026-11-26): a Shared VM's unit throttled
  past its allowance by `MemoryHigh` throttles every thread of the unit,
  not only the vCPU that faults: Cloud Hypervisor's API thread, and the
  guest's balloon driver and reclaim with its vCPUs. On a testbed, two
  Shared guests reading at disk speed into their brakes were throttled
  some 25,000 times each in 10 minutes, waited up to 10.2 s and 11.5 s,
  and did not finish their reads; the squeeze's polls, every second on
  paper, came 20 to 30 s apart, and the balloons grew some 17 MiB a poll
  against the 1 GiB asked. The brake stalls what would release it. Nor on
  the pool's slice, which throttles whichever member allocates, a
  Dedicated one too.
- **The backstop kills a Shared VM whole.** Every VM's unit has
  `OOMPolicy=kill`, so systemd sets `memory.oom.group` and the kernel's
  OOM killer takes all of its processes or none; its result is then
  `oom-kill`, a failure to `Restart=on-failure`. A Shared member's
  processes have `OOMScoreAdjust` 1000, the most, a Dedicated one's 0: the
  kernel weighs a process by its memory and that many thousandths of the
  limit it hit, so past the slice's limit a Shared VM goes first however
  large the Dedicated ones. A starting agent sets `memory.oom.group` on a
  running unit started without `OOMPolicy=kill`, which systemd cannot set
  at runtime; systemd clears it again whenever it sets one of the unit's
  properties, so the driver sets it after each. The unit's cgroup goes
  with the unit, so its `ExecStopPost`, the launcher (`hangar-node vmm
  --stopped`), records the kill as the VM's exit when systemd's result and
  the cgroup's `oom_group_kill` both say so, and when, in a file of its
  own that its next start keeps. The machine then says `OOMKilled` True,
  `OutOfMemory`, with the time, whatever its restart policy, and reads
  `OutOfMemory` while its restart policy keeps it down.
- **What is measured.** `hangar_node_memory_pool_squeezes_total` counts
  the polls that changed a balloon, by `result` (`safe`, `guarantee`,
  `deflate`), `hangar_node_memory_pool_balloons_unresponsive_total` the
  balloons given up, and `hangar_node_vm_oom_group_kills_total` the VMs killed
  whole, `hangar.slice`'s `memory.events` count. A machine's usage says
  what its balloon holds (`machine-usage.md`).

The balloon was measured on a testbed (CH v53, 2026-11): a 2 GiB
container guest with 1.5 GiB of page cache gave it back to a balloon
leaving it 512 MiB in 3.8 s, at about 440 MiB/s, with no wake of a ticker
in it later than 3 ms; a balloon deflated in 0.2 s. The balloon never stops
short: inflated past what its guest frees, the guest's OOM killer kills
its largest process, so the safe floor leaves a margin. An Ubuntu 24.04
firmware VM gave back its cache as fast. A slice's `MemoryMax` with
`memory.oom.group` killed a whole unit in 0.3 to 0.4 s, the victim chosen
by `oom_score_adj`.

`TestAMemoryPoolIsKeptWithinItsSize` in `test/testbed/vms` runs a pool of
5 GiB with two Shared VMs of 4 GiB, guaranteed 1 GiB, and a Dedicated one
of 2 GiB, squeezed as the agent does (2026-11-26, kernel 6.8, CH v54). The
two Shared guests wrote and read 3.5 GiB each, twice, at disk speed, in 8
to 9 s, while the Dedicated one took 1.5 GiB: no VM and no guest process
was killed, the slice never reached its limit (its peak 4.8 GiB of 5.95),
the Shared sizes and the Dedicated memory never passed 5 GiB, and the
worst wake of a ticker on each vCPU came 16 to 41 ms late in the Shared
guests, 13 to 26 ms in the Dedicated one. One Shared guest reading on,
the other idle, had the other's room past its guarantee, 1.5 to 2 GiB, in
5.4 to 12.9 s; one left alone in the pool had all its memory back in 2.6
to 3.6 s; two started again into a pool the third filled were made room
for in 9.1 s. A Shared guest filling 3.6 GiB of anonymous memory past
its room was held there by its balloon, and its own OOM killer killed the
process, the others untouched. `TestASharedContainerReadingAsItBootsIsNotKilled`
starts two Shared containers that read 3.5 GiB at once as they boot,
three times: none was killed, their starts put off 3.9 to 5.7 s. Before
the wait, the same start reached the slice's limit 7 s in, and the kernel
killed one whole.

## Consequences

- Recovering from a crash, a reinstall of the agent, or a machine deleted while
  the agent was down needs no special code: the next reconcile and sweep do it.
- Each driver must be able to list what it made by uid, cheaply enough to do
  every minute.
- Drivers are used concurrently: images are prepared while machines are
  built.
