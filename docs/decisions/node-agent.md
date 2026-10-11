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
  real hypervisor driver.
- **The node follows the API like a controller** (`controller.APISource`),
  with Machines placed on it and their Networks as the watched kinds. What the
  host notices on its own (a VM exiting, networkd removing a link) checks every
  machine again.
- **Nothing slow runs under the host lock.** Reconciles and the sweep take
  turns under one lock, so each step under it must be short. Preparing an
  image (resolving, pulling, making its base) can take minutes, so a puller
  does it off the lock, once per image however many machines want it; a
  machine waiting on its image says it is pulling, while the others are
  built, stopped and deleted meanwhile. A job that stalls fails after a
  timeout and is tried again, and asking a guest agent whether it answers is
  bounded to seconds. One worker then serves every machine in turn; a step
  that turns slow later (such as a graceful shutdown) moves off the lock the
  same way.
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
