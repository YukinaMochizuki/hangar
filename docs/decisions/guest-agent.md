# hangar-init: PID 1 and agent of every container's guest

Status: accepted, 2026-10.

## Context

A container is an OCI image booted as the root disk of a microVM (see
`machine-model.md`). Something in the guest must mount that disk, set up the
network, run the image's process, and answer the node: whether the guest is
up, and later exec, logs, and snapshot hooks. Images are the user's, so
nothing may be installed into them, and the node is stateless and may be
down at any moment (`node-agent.md`), so a guest must not need it to boot or
to boot again.

## Decision

- **hangar-init comes in the platform's initramfs**, built and shipped with
  the guest kernel, holding nothing else. It mounts the root disk
  (`/dev/vda`), switches to it, and stays PID 1. Images are never modified;
  upgrading the agent means a new initramfs.
- **What to run comes on a boot disk** (`/dev/vdb`, `internal/guest/bootdisk`):
  a `hangar.guest.v1.BootConfig` with the process (args, env, working
  directory, user), the interfaces by MAC, the hostname and the resolvers.
  The guest needs nothing else, so it boots again, after a crash or a
  restore, without the node. Not the kernel command line, which is short and
  readable by any process in the guest; not files written into the image.
- **The agent speaks Connect over vsock**, port 1024, with the service in
  `proto/hangar/guest/v1`. It is internal but versioned with buf breaking,
  since a guest keeps the hangar-init it booted with while the node is
  upgraded. The node reaches it through Cloud Hypervisor's hybrid vsock
  socket (`internal/node/driver/chguest`). An exec lasts until the process
  ends and the caller has closed its side; if the caller goes away first,
  the process group is killed. Stdin is queued in the guest, so a signal
  or a resize is not held up behind input the process has yet to read. Past
  4 MiB queued the caller waits, its signals with it, as over ssh: stdin is
  never dropped, since a pipe's must arrive whole.
- **Output goes to the virtio console, framed** (`internal/guest/console`):
  every write of the container's stdout and stderr, hangar-init's own
  messages, and, last, how the process ended. Cloud Hypervisor hands the
  console to the VM's launcher, which keeps it in a file on the host
  (`node-agent.md`), so the logs and the exit code are there however the
  guest ended and whether or not the node was up. The console
  is a tty, so hangar-init sets it raw; nothing is written after the exit
  record. The kernel's own console stays on the serial port, quiet.
- **When the container's process ends, the guest powers off.** hangar-init
  records the exit, stops what is left, leaves the disk clean, and powers
  off; the host's restart policy decides whether it boots again. A restart
  is a fresh boot on the same root disk, as restarting a Docker container
  keeps its filesystem.
- **Stopping a container is docker stop** (`Stop`): hangar-init sends
  SIGTERM to the process group of the container's process, and SIGKILL to
  it once the grace period the node gives has passed, unless the process
  has ended; it then records the exit and powers off as when the process
  ends by itself. It answers once SIGTERM is sent; a stop asked for before
  the process starts applies as it starts. The node gives 10 seconds for
  now, and kills the VM if the guest is not off a few seconds after; a
  `stopGracePeriod` field may come later. A VM's launcher asks the same
  when the host shuts down (`node-agent.md`).
- **The platform provides `/etc/hostname`, `/etc/hosts` and
  `/etc/resolv.conf`** as Docker does: written to `/run/hangar` and mounted
  over the image's files, so the image's own are never written. The
  resolvers are those of the machine's networks, at their gateways
  (`networking.md`), as the boot disk lists them.
- **Cgroups are laid out for Docker in the guest**: hangar-init in `/agent`,
  everything it starts in `/workload`, with every controller enabled, since
  the root cgroup has no limits to give.
- **One reaper does all the reaping.** As PID 1, hangar-init inherits every
  orphan; one reaper collects every exit and hands each to whoever started
  that process (`internal/guest/supervise`). Nothing in hangar-init uses
  `os/exec`'s `Wait`, which would race it. Signals go to a process through
  its pidfd, never to a later one given the same pid.
- **No signal ends hangar-init.** Go's runtime exits on SIGTERM and its
  like, which would panic the kernel as PID 1, so hangar-init catches every
  signal and ignores it. Stopping a container is the node's to do, through
  the agent or the hypervisor.

## Consequences

- Everything a guest needs is on its two disks and in the initramfs, which
  is what makes a snapshot restore or a node restart harmless to it.
- Exit codes that never came from the process: 127 when its program is not
  found, 126 when it cannot be run, 255 when the guest could
  not boot it; hangar-init's console messages, or the exec's stderr, say
  why.
- **The guest is not trusted by the host.** Its root is the tenant's, who
  can replace the agent's answers or write their own frames to the console,
  forged exit records included. So the node bounds what it reads from a
  guest (each agent message to 1 MiB, the vsock handshake to a line and a
  deadline), reads the console as the tenant's data, and bounds the console
  file's size itself: Cloud Hypervisor does not. The exit code and the log
  are whatever the guest asserts: a guest forging records, or a header
  whose payload takes in hangar-init's exit record, sets its own exit code
  and garbles its own log, which harms only itself (`node-agent.md`).
- What needs root in the guest (mounting, switching root, the network) is
  exercised on the testbed (`docs/decisions/testing-strategy.md`, L4); the
  rest is tested in any workspace.
