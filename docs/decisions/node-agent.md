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
  and VM; a network's namespace), and every host driver lists what it made. A
  reconcile compares what the API wants with what the host has.
- **A sweep removes what nothing accounts for**: VMs, root disks and
  interfaces of machines that are not on this node any more, then networks and
  image bases nothing on the host uses or is being built from. It runs at
  start and every minute, under the same lock as the reconciles. A list it
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
  writes can wedge it or grow the file past its cap; the kernel's serial
  console goes to `serial.log` the same way, past 1 MiB. A log that fails,
  as on a full disk, drops what it cannot take and is tried again, so the
  guest is never held up by it. When the guest
  powers off, the launcher records how its process ended in `exit.json`
  and exits with the process's exit code (253 when the guest recorded
  none, as after a crash; 252 when the launcher could not start the VM),
  so systemd's `Restart=` and its restart count see the container. A
  container exiting with 252 or 253 itself reads the same; `exit.json`
  says which (`Error` for the launcher's failure).
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
- **Stopping is docker stop** (`guest-agent.md`): the agent asks the guest
  to stop its process, waits up to the 10 s grace and 5 s more for the
  guest to power off, then stops the unit, which kills Cloud Hypervisor; a
  guest that does not take the stop within 3 s is killed at once. Before
  asking, it marks the VM as being stopped, and a launcher whose guest
  powers off while marked waits to be stopped instead of exiting, so a
  stop never restarts a VM, whatever its policy. A stop removes the unit
  and the VM's directory but its logs. A stop cut short leaves the mark;
  starting the VM's spec again while it runs clears it, so its next exit
  is its policy's again.
- **A VM's logs outlive it**: its restarts, a stop and a start again. They
  go when the machine is released, or when the sweep finds them of no
  machine on the node.
- **The driver touches only its own**: units named `hangar-vm-<uid>` for a
  uid it takes, and its directory.
- **The node follows the API like a controller** (`controller.APISource`),
  with Machines placed on it and their Networks as the watched kinds. What the
  host notices on its own (a VM exiting, networkd removing a link) checks every
  machine again.
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
  fixed and pushed under the same tag is pulled. Asking a guest agent
  whether it answers is bounded to
  seconds. One worker then serves every machine in turn. Stopping a VM
  still runs under the lock, and takes as long as its guest takes to stop:
  about 20 s at worst (3 s to ask the guest, the 10 s grace and 5 s of
  slack, 2 s for the launcher to kill Cloud Hypervisor); it is the next
  step to move off the lock the same way.
- **An image reference is resolved once**: the reference and its digest
  are kept in the machine's status, and resolved again only when the spec
  names another image. A tag moving upstream never rebuilds a container's
  disk, whatever else changes. A new image rebuilds the root disk (see
  `machine-model.md`); any other change restarts the VM on the disk it has. A
  digest the host has pulled is read from its cache until its base goes, so
  a running machine never depends on the registry.
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

## Consequences

- Recovering from a crash, a reinstall of the agent, or a machine deleted while
  the agent was down needs no special code: the next reconcile and sweep do it.
- Each driver must be able to list what it made by uid, cheaply enough to do
  every minute.
- Drivers are used concurrently: images are prepared while machines are
  built.
