# Host bootstrap

`deploy/host/` brings a freshly installed host to the state hangar expects:
packages and a pinned Cloud Hypervisor, swap mirrored on md RAID1, the `hangar`
pool's properties and datasets, netplan owned by us with IPv6 off the
prefix's `::` address, proxy NDP, forwarding, and vsock, unattended security
upgrades in a fixed window, a hangar release (the binaries, the guest kernel
and the initramfs), and `hangar-apiserver` and `hangar-node` as systemd
services. It is safe to re-run.

## Before the first run

The OS install itself is done through the OVH API with a custom partition
layout: `/boot` 1 GiB, `/` 100 GiB and swap 8 GiB on software RAID1, and the
rest as a ZFS partition with `raidLevel: 1` and pool name `hangar`, which the
installer turns into a mirrored pool. The installer does not mirror swap and
gives the host its prefix's `::` address; the bootstrap fixes both.

Add `deploy/host/hosts/<hostname>.env` for the host. Every value in it is
explicit (uplink name, public IPv4, IPv6 address and gateway, the two swap
partitions, the dataset quotas, the memory kept from VMs, the Node's name);
the scripts never guess a disk or an interface.

One value stays on the host and out of the repo: the bootstrap admin, the
email of the person's Cloudflare Access identity, who is granted
`platform.root` while no one holds it. Write it once, by hand, as root's,
mode 0600 (`<email>` is the address itself):

```sh
sudo install -D -m 0600 /dev/null /etc/hangar/local.env
echo 'HANGAR_BOOTSTRAP_ADMIN=<email>' | sudo tee /etc/hangar/local.env
```

The `services` step refuses to run without it, and `verify.sh` checks it.
The apiserver reads it through `EnvironmentFile=`, and `verify.sh` asks the
API as that identity.

Build a release, `make release`, on a machine with the guest kernel's
toolchain (`docs/runbook/guest-kernel.md`; the committed kernel config is
Ubuntu 24.04's GCC's, so a testbed VM, not a workspace). It writes
`bin/release/hangar-<revision>/`: `hangar-apiserver`, `hangar-node` and
`hangar` (static), `vmlinux`, `initramfs.cpio`, `VERSION` (the full
revision, as the binaries report it) and `SHA256SUMS` over all of them. It
refuses a tree with uncommitted changes (`ALLOW_DIRTY=1` for a test build).
Built twice from one commit with one toolchain, it gives the same
`SHA256SUMS`. Nothing it builds is committed.

## Run

Copy `deploy/` and the release directory to the host, then as root:

```sh
deploy/host/bootstrap.sh --release DIR --dry-run   # diffs and "would:" lines, changes nothing
systemd-run --unit=hangar-bootstrap --wait --collect deploy/host/bootstrap.sh --release DIR
journalctl -u hangar-bootstrap       # its output; ends with "changes: N"
deploy/host/verify.sh                # one PASS/FAIL per check, exit 1 on any FAIL
```

Run the real thing detached from the SSH session, as above. `netplan apply` and
`networkctl reconfigure` renew the uplink's DHCPv4 lease, which can drop the
session; a run killed with it is safe to re-run, but it should not be killed.

A second `bootstrap.sh`, with the same `--release` or none, must print
`changes: 0`. Without `--release` the `release` step keeps what is installed
(and refuses when nothing is). `--only <step>` runs a single step
(`packages`, `swap-raid`, `zfs`, `network`, `sysctl`, `upgrades`, `release`,
`services`).

## Datasets, upgrades, the release and the services

- `zfs` also makes `hangar/images` (`/var/lib/hangar/images`,
  `setuid=off exec=off devices=off`, quota `HANGAR_IMAGES_QUOTA`) and
  `hangar/vms` (`/var/lib/hangar/vms`, `setuid=off exec=off`, quota
  `HANGAR_VMS_QUOTA`), both mount points `0700` and root's, mounted by
  `zfs-mount.service` at boot. `hangar/vms` must outlive a reboot: the node
  reads each VM's `spec.json` and `exit.json` after one.
- `upgrades` keeps Ubuntu's unattended upgrades to their default origins
  (security only), never reboots, and moves `apt-daily.timer` to 03:30 and
  `apt-daily-upgrade.timer` to 04:00 Asia/Singapore, with no random delay
  and nothing made up at boot: an upgrade can re-exec systemd and trigger
  udev on every device. It also tells needrestart to leave every `hangar-*`
  unit alone: after a hangar release every VM's launcher looks stale to it.
- `release` checks every file of `--release DIR` against its `SHA256SUMS`
  and refuses the whole release on a mismatch or a missing file, before
  installing anything. It installs the binaries in `/usr/local/bin` and the
  kernel and initramfs in `/usr/local/lib/hangar/`, each by a rename, then
  records `VERSION` and the checksums by installed path in
  `/usr/local/lib/hangar/`. `verify.sh` checks them with `sha256sum -c`.
- `services` makes the system user `hangar`, gives it
  `/var/lib/hangar/state` (mode 700), and writes, enables and starts
  `hangar-apiserver.service` and `hangar-node.service`:
  - the apiserver as `hangar`, sandboxed (`systemd-analyze security
    hangar-apiserver` shows what is left), its database on `hangar/state`,
    the public API on `127.0.0.1:8080` with `--auth-mode=dev` until the
    Cloudflare tunnel lands, and the node socket `/run/hangar/node.sock` in
    its runtime directory, accepting uid 0 as the Node `HANGAR_NODE_NAME`;
  - the agent as root: `--drivers=host --name HANGAR_NODE_NAME --uplink
    HANGAR_UPLINK --public-ipv4 HANGAR_PUBLIC_IPV4 --reserved-disk` the sum
    of both quotas, `--reserved-memory HANGAR_RESERVED_MEMORY` (the ARC cap
    plus the host's own overhead), every path its default,
    `Restart=always`. Its `KillMode=control-group` kills only what the
    agent runs itself: each VM is a transient unit of its own and outlives
    a stop or restart of it.

  It restarts a service whose unit it changed, or whose binary the `release`
  step replaced (the running process's `/proc/PID/exe` is then a deleted
  file), and nothing else. A unit written by a run that died before the
  restart leaves `/var/lib/hangar-bootstrap/restart-<unit>`, so the next run
  restarts it.

## Deploying a new hangar version

VMs keep running across it: restarting `hangar-node` leaves their units
alone. A VM's unit runs `/usr/local/bin/hangar-node vmm` as its launcher, so
a VM started again after the upgrade (a restart policy, a disk change, a
reboot) runs the new launcher on the `spec.json` the old agent wrote, and
boots the new guest kernel and initramfs. A running VM keeps its own until
then.

1. Build it from the commit to deploy, on a testbed VM: `make release`.
   Note its directory name, `hangar-<revision>`.
2. Copy `deploy/` from that commit to `/var/tmp/hangar/deploy` on the host,
   and the release directory to `/var/tmp/hangar/release/hangar-<revision>`.
   Keep the previous release's directory there: going back is the same
   sequence with it.
3. On the host, one command per line:

   ```sh
   sudo /var/tmp/hangar/deploy/host/bootstrap.sh --release /var/tmp/hangar/release/hangar-<revision> --dry-run
   sudo systemd-run --unit=hangar-bootstrap --wait --collect /var/tmp/hangar/deploy/host/bootstrap.sh --release /var/tmp/hangar/release/hangar-<revision>
   sudo journalctl -u hangar-bootstrap --no-pager
   sudo /var/tmp/hangar/deploy/host/verify.sh
   sudo /var/tmp/hangar/deploy/host/bootstrap.sh
   cat /usr/local/lib/hangar/VERSION
   systemctl list-units --no-pager 'hangar-*'
   ```

   The dry run lists `would: install` for each changed file and `would:
   restart` for each service to restart, and nothing else for an upgrade
   alone. The real run ends with `changes: N`. `verify.sh` must pass, the
   run after it must print `changes: 0`, `VERSION` must be the new
   revision, and the VMs' units must still be there.

A release with a file that does not match its `SHA256SUMS` stops the run
before anything is installed; copy it again.

To use the CLI on the host, as the bootstrap admin over dev auth:
`hangar config set-context local --endpoint http://127.0.0.1:8080 --auth dev
--user <email>`.

## When the network step fails

It refuses to take over when the installer's stanza for the uplink sets
anything the new config would not carry over (only the IPv6 address and route
are replaced; IPv4 must be plain `dhcp4: true`). The new config matches the
uplink by MAC and names it, so the name survives a reboot.

It validates the netplan config in a scratch root, refuses to start if the IPv6
gateway does not answer under the current config, and arms a timer
(`hangar-netplan-rollback.timer`, 180 s) before applying. If IPv6 or IPv4 is not
healthy within about 60 s, it rolls back at once; if the script dies, the timer
rolls back. While the timer is pending, `bootstrap.sh` refuses to run.
Backups and the rollback script are under `/var/lib/hangar-bootstrap/netplan-*`.

## What the node agent needs from the pool

The node's image and storage drivers (`docs/decisions/storage-zfs.md`) need
these before `hangar-node` starts; the `packages` and `zfs` steps make them:

- `e2fsprogs` (for `mkfs.ext4 -d`) and `zfsutils-linux` installed.
- The pool `hangar`, as the installer and the `zfs` step leave it.
- `hangar/images`, mounted at `/var/lib/hangar/images` with
  `setuid=off exec=off devices=off`, the mount point `0700` and root's, and a
  `quota`, so pulling and unpacking never take the room root disks need: a
  few times the largest image the host should take, such as 100G on the
  current host. The image cache and the directory images are unpacked in
  both go under it, on that one dataset: both hold tenant content. Pass the
  quota, and what bases may take, to `hangar-node --reserved-disk`.
- Nothing else: the driver makes `hangar/bases` and `hangar/roots`
  (`mountpoint=none`) under the pool when they are missing, and owns them.
  Nothing else may be created under them.
- udev's zvol rules (`/dev/zvol/...`), which `zfsutils-linux` installs. The
  `zfs` step adds Hangar's own, which keep udev from probing or watching
  zvols (`/etc/udev/rules.d/59-hangar-zvol.rules` and `99-hangar-zvol.rules`).

`test/testbed/setup.sh` makes the same on a testbed VM.

## What the node agent needs to run VMs

The hypervisor driver (`internal/node/driver/chvm`,
`docs/decisions/node-agent.md`), with `hangar-node --drivers=host`, needs:

- Cloud Hypervisor at `/usr/local/bin/cloud-hypervisor`, which the
  `packages` step installs at the pinned version; `--cloud-hypervisor`
  names another.
- The platform guest kernel and initramfs (`docs/runbook/guest-kernel.md`)
  at `/usr/local/lib/hangar/vmlinux` and
  `/usr/local/lib/hangar/initramfs.cpio`, or where `--guest-kernel` and
  `--guest-initramfs` say. The `release` step installs them. A VM
  keeps the ones it was started with until it is started again.
- `/var/lib/hangar/vms` (`--vm-dir`), the `hangar/vms` dataset the `zfs`
  step makes, `0700` and
  root's: each VM's spec, boot disk, sockets and console logs. Its path must
  stay short enough for a unix socket under it (the driver refuses one too
  long). The logs are tenant data, at most 34 MiB a VM.
- systemd 254 or later, for `RestartSteps=` and `RestartMaxDelaySec=` in
  the VMs' units (Ubuntu 24.04 has 255), and its D-Bus API: the system bus,
  or, as root, systemd's private socket.
- `/dev/kvm`. Not `vhost_vsock`: Cloud Hypervisor's hybrid vsock is its own
  device, reached through a unix socket.
- The binary `hangar-node` stays where it was when VMs were started: their
  units run it as their launcher, by the path the agent ran from. Replacing
  it in place upgrades every VM's launcher at its next start.

## Things that are easy to get wrong

- Proxy NDP is set through a networkd drop-in, not sysctl.d: networkd resets
  `proxy_ndp` whenever it reconfigures the link.
- Forwarding must not be enabled while the host holds `<prefix>::`; the kernel
  then claims that address as Subnet-Router anycast. The sysctl step refuses.
- The swap step refuses any listed partition that does not currently hold swap,
  because building the array wipes it. It also sets `RESUME=none` and refuses
  while the kernel command line or GRUB carries `resume=`: a resume device that
  no longer exists stalls boot. Only initramfs-tools is supported.

## Testing a change

`test/testbed/host-bootstrap.sh` runs the whole flow on a throwaway Ubuntu VM
made to look like a fresh OVH install (loop-device swap, file-backed pool, an
upstream router in a network namespace): dry run changes nothing, first run
converges and counts its changes correctly, second run changes nothing, verify
passes, verify catches drift and a re-run repairs it, a pending rollback blocks
a run, unknown installer netplan keys are refused, a failing generator writes
nothing, and a dead IPv6 config is rolled back. It needs a release
(`HANGAR_TESTBED_RELEASE=DIR`, from `make release`), installs the services
from it, and checks: the node is Ready; a tampered release, or one whose
`SHA256SUMS` lacks a file, is refused with nothing installed; the services
step refuses without `/etc/hangar/local.env` or with it readable by others;
a new release restarts both services and not a stand-in VM unit, and so
does going back; a changed unit restarts its service only, also when a run
died before the restart; and drift in the datasets, the installed files,
the apt timers and the services is reported and repaired. It does not
reboot. Run it as root on a disposable VM only. `make lint-sh` runs
shellcheck over every script.

`HANGAR_TESTBED_FLOW_INSTALLED=1` runs the L4 flow (`docs/runbook/testbed.md`)
against the services the steps installed instead of processes of its own.
