# A container joins a tailnet natively, through hangar-init

Status: accepted, 2026-11.

## Context

Any image, with one spec field, should put a container on a tailnet as a
node of its own, with nothing of tailscale in the image and no proxy: its
processes see an ordinary interface, `tailscale0`, where `ssh`, `curl` and
`git` just work. Tower passes the field through for workstream's boxes, so
its shape (`ContainerSpec.tailscale`, the `TailnetJoined` condition and its
reasons) is a contract shared with tower. An agent may find mid-work that
its box needs the tailnet, or no longer does, so a container joins and
leaves without restarting (decided with the owner, 2026-11-08). VMs are
out: they keep their cloud-init, and tower joins them itself.

The guest kernel has TUN built in, `/dev` is devtmpfs, and hangar-init drops
no capabilities, so tailscaled runs in TUN mode with no extra privilege. A
hand test on prod found the rest: tailscaled picks iptables-legacy by itself
and fails, as guests have nftables alone (`guest-kernel.md`); with
`--accept-dns=true` it rewrites `/etc/resolv.conf` and `*.hangar.internal`
stops resolving; its state on the root disk survives a stop and start.

## Decision

- **hangar-init runs tailscaled, not the image.** Images are the user's,
  and nothing is installed into them (`guest-agent.md`). The boot disk says
  whether to join as the guest boots, and as what (hostname, accept
  routes), never with which key; then hangar-init starts tailscaled, after
  it has written the container's `/etc` files and before its process, in a
  session of its own outside the process's group, starts it again with a
  backoff when it dies, and stops it, SIGTERM then SIGKILL, before the
  guest powers off. Nothing of it holds the process back: a guest that
  cannot join says why, and runs all the same. tailscaled ships nothing to
  Tailscale's log service (`--no-logs-no-support`): the node is the
  tenant's, not a third party's to watch; its log files stay in its state
  directory (`TS_LOGS_DIR`), never in the image's `/var/lib/tailscale`.
- **Joining and leaving while it runs.** None of `tailscale` is part of
  what the VM runs (its spec's hash): the node tells the running guest
  through the agent's `SetTailnet`, on its first sight of each boot and on
  each change, and rewrites its boot disk to match first, so a boot of the
  restart policy comes up as last told. Told a tailnet, hangar-init mounts
  the disk, switches the DNS split on and starts tailscaled, which joins as
  at boot. Told none, it runs `tailscale logout`, stops tailscaled, deletes
  its state, so a later join is a new node with a fresh key, and restores
  the container's own resolvers. A new hostname or `accept_routes` is
  `tailscale set` on the running node. Retagging a node is not hangar's: it
  is done through Tailscale's API.
- **tailscaled and the CLI come from a disk of the platform's**, not the
  initramfs: `tailscale.img`, read-only ext4, built from the Tailscale
  release `guest/tailscale/tailscale.env` pins by version and sha256, and
  shipped in the release beside the kernel. The two binaries are some 80 MB;
  every guest loads its initramfs into memory, so in it they would cost
  every container, those with 128Mi included, that much at each boot.
  As a disk, one file on the host is shared read-only by every container
  VM, and paged in as read. Every container gets it, joining or not, so it
  can join later without a restart; a file missing as a VM starts leaves
  that VM without it, never unable to boot. It is attached last, after the
  container's volumes, so their device names do not move, and the boot disk
  names its device. hangar-init mounts it at `/run/hangar/tailscale`, under
  the platform's tmpfs, only once the container joins, so the image's files
  are never written. The CLI is at `/run/hangar/tailscale/bin/tailscale`;
  hangar-init appends that directory to `PATH` for execs once it is
  mounted, and for the container's process when it joins as it boots. A
  login shell started otherwise, as by an sshd in the image, sets its own.
- **nftables.** tailscaled runs with `TS_DEBUG_FIREWALL_MODE=nftables`, in
  TUN mode (`--tun=tailscale0`), its socket at
  `/var/run/tailscale/tailscaled.sock`, where the CLI looks by default.
- **tailscaled never touches DNS** (`--accept-dns=false`). hangar-init
  splits it instead: a small resolver of its own on `127.0.0.11:53` (the
  address Docker's embedded resolver takes, which images leave free) is
  the one nameserver of the container's `/etc/resolv.conf` while it is on
  a tailnet. The file is the platform's, mounted over the image's (or
  written under `/run`), so it is rewritten in place, at a join or a
  leave, and the mount shows it at once. A name under the tailnet's
  MagicDNS suffix, as tailscaled reports it once joined, goes to
  `100.100.100.100`; every other name to the resolvers the guest would
  have had without the tailnet, in order, as before. Search domains do not
  change. A guest without `tailscale` has none of it: its DNS is as it
  was, and no resolver of hangar-init's listens; nor does one without the
  disk, and the resolver stops as a guest leaves its tailnet. Rewriting
  the file in place leaves a reader between the truncate and the write
  an empty file for that one lookup; a rename cannot be used, as the
  mount holds the inode. Not a rewrite of `resolv.conf` with search domains for the tailnet:
  it would change how every other name resolves.
- **State lives on the root disk**, in `/var/lib/hangar/tailscale`. A stop
  and start, or a restart, comes up as the same node with no key. It goes
  with the root disk, as when the image changes, and as the container
  leaves its tailnet; the next join is a new node. The guest's root can
  read it: the container is the node.
- **The key's lifecycle.** The key is in a `tailscale-auth` Secret
  (`secrets.md`), one key, `authkey`, sealed at rest as every Secret. It
  reaches the guest over the guest agent, never the boot disk, which is a
  file on the host for as long as the VM lives and is read again at every
  restart: the node asks the apiserver for it through
  `TailscaleAuthKeyService`, which answers only the node the machine is
  placed on, each time, and hands it to the guest's `JoinTailnet`, keeping
it nowhere. The node gives it only when the guest asks for one
  (`TailnetStatus.wants_key`): when it has no node state (the state file
  names no login profile), and again, five minutes after a refusal or a
  failed join, so a fixed Secret is picked up. The guest keeps that time,
  across its tailscaled's restarts, so neither a node agent's restart nor
  a tailscaled's hurries a retry. A Secret that gives no key (`NoKey`) is
  asked for again 10 seconds on, then twice as long each time, up to five
  minutes. In the guest, the CLI reads the key from a pipe
  (`--auth-key=file:/proc/self/fd/3`), so it is in no argument list, on no
  disk, in no log. A guest with state refuses a key: once joined, the key
  is never used again, and a node whose login has gone says so as
  `Failed` rather than joining again as another.
- **Status.** The node asks each running guest where its join stands, and
  reports `TailnetJoined` on the machine, which the container copies: True,
  `Joined`; or False, `Starting`, `NoKey` (the Secret, or its `authkey`, is
  missing), `KeyRejected` or `Failed`, with a message saying why.
A container that joins none has no such condition.

## Consequences

- A host without `tailscale.img` boots such containers all the same; they
  report `TailnetJoined=False Failed`.
- A guest booted by a hangar-init from before this cannot join, nor be
  told to; it reports `Failed` until the container starts again. Neither
  has a container started before every container got the disk.
- tailscaled's own output goes to `/run/hangar/tailscaled.log` in the guest,
  capped at 1 MiB with one file before it, not to the container's log.
- Bumping tailscale is a release of hangar (`docs/runbook/guest-kernel.md`).
