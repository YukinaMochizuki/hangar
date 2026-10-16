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
  interfaces of machines that are not on this node any more, volumes of Disks
  that are not bound to it any more, then networks and image bases nothing on
  the host uses or is being built from; a disk image's base also stays while
  an Image names it (`docs/decisions/vm-images.md`). It runs at start and
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
  with Machines placed on it and their Networks and Disks as the watched
  kinds. What the host notices on its own (a VM exiting, networkd removing a
  link) checks every machine again. A second controller keeps the volumes of
  the Disks bound to the node, at the size their status says, and removes a
  deleted one's once no machine lists it, reporting `Released`.
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
  digest the host has pulled is read from its cache until its base goes, so
  a running machine never depends on the registry. The cache answers a
  digest only for the repositories it was fetched from in full; a machine
  naming it from another fetches it in full from there first. A machine
  with image pull secrets resolves its reference with its own login, in a
  pull of its own; only machines without credentials share a pull, which
  resolves anonymously, and a digest in a machine's status needs resolving
  no more (`secrets.md`).
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
environment. Cloud Hypervisor keeps its own seccomp filter.

**The VM's directory** is root's and the VM's group's, `0710`: Cloud
Hypervisor reads `boot.img` (`0640`), opens `disk` and `serial.fifo`
through the group, and writes only in `ch/` (`0770`), made afresh on each
run, where it makes its socket. It can neither read `spec.json` or the
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
| `MemoryMax` (the guest's memory, 256 MiB, and 2% of the guest's), `TasksMax` (64 and two a vCPU) | Taking the host's memory or tasks |

`MemoryMax` was measured (2026-10, `TestMemoryOverhead` in
`test/testbed/vms`) with guests of 1, 4 and 16 GiB filling half their
memory: past what the guest used and what its kernel keeps out of its
`MemTotal`, the unit took at most 13, 9 and 36 MiB, mostly KVM's page
tables, which grow with the guest; a guest that barely ran peaked at 113,
239 and 463 MiB, its kernel touching its memory map. 256 MiB and 2% (584
MiB past a 16 GiB guest) leave room for that many times over, and for the
unit's page cache, which is reclaimed before the limit kills anything.

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
reserves a machine's whole memory, so this frees the host's memory for
its page cache and the ARC, not for more machines.

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

## Consequences

- Recovering from a crash, a reinstall of the agent, or a machine deleted while
  the agent was down needs no special code: the next reconcile and sweep do it.
- Each driver must be able to list what it made by uid, cheaply enough to do
  every minute.
- Drivers are used concurrently: images are prepared while machines are
  built.
