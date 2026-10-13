# Host bootstrap

`deploy/host/` brings a freshly installed host to the state hangar expects:
packages and a pinned Cloud Hypervisor, swap mirrored on md RAID1, the `hangar`
pool's properties and datasets, netplan owned by us with IPv6 off the
prefix's `::` address, proxy NDP, forwarding, and vsock, unattended security
upgrades in a fixed window, a hangar release (the binaries, the guest kernel
and the initramfs), `hangar-apiserver` and `hangar-node` as systemd
services, and, once it has a token, the Cloudflare tunnel. It is safe to
re-run.

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
The apiserver reads it through `EnvironmentFile=`. The same file later
sets the auth mode ("Switching to Cloudflare Access" below); without that,
the public API is in dev auth.

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
`changes: 0`. Without `--release` the `release` step keeps what is installed.
It refuses when nothing is, and when the installed files do not match the
checksums recorded for them (a run that died half-way, or a file replaced
by hand), so that no service restarts onto files of two releases; rerun
with `--release DIR`. `--only <step>` runs a single step
(`packages`, `swap-raid`, `zfs`, `network`, `sysctl`, `upgrades`, `release`,
`services`, `cloudflared`).

## Datasets, upgrades, the release and the services

- `zfs` also makes `hangar/images` (`/var/lib/hangar/images`,
  `setuid=off exec=off devices=off`, quota `HANGAR_IMAGES_QUOTA`) and
  `hangar/vms` (`/var/lib/hangar/vms`, `setuid=off exec=off`, quota
  `HANGAR_VMS_QUOTA`), both mount points `0700` and root's, mounted by
  `zfs-mount.service` at boot. `hangar/vms` must outlive a reboot: the node
  reads each VM's `spec.json` and `exit.json` after one. It also keeps
  `hangar/state` at `/var/lib/hangar/state` (the `services` step owns its
  mount point) and `hangar/poc`, for throwaway experiments, never mounted:
  volumes under it are reached as `/dev/zvol` paths.
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
    the public API on `127.0.0.1:8080` in the auth mode `local.env` asks
    for (dev when unset), and two unix sockets that systemd makes and
    passes it (`hangar-apiserver-node.socket` and
    `hangar-apiserver-admin.socket`, `PartOf=` the apiserver, so they stop
    and restart with it): the node socket `/run/hangar/node.sock`
    (`hangar`'s, 0660), accepting uid 0 as the Node `HANGAR_NODE_NAME`, and
    the admin socket `/run/hangar/admin.sock` (root's, 0600), where root on
    the host is `platform.root` whatever the bindings say
    (`docs/decisions/authentication.md`). `/run/hangar` is root's, 0755,
    from `/etc/tmpfiles.d/hangar.conf`, so the apiserver can remove or
    replace neither socket;
  - the agent as root: `--drivers=host --name HANGAR_NODE_NAME --uplink
    HANGAR_UPLINK --public-ipv4 HANGAR_PUBLIC_IPV4 --reserved-disk` the sum
    of both quotas, `--reserved-memory HANGAR_RESERVED_MEMORY` (the ARC cap
    plus the host's own overhead), every path its default,
    `Restart=always`. Its `KillMode=control-group` kills only what the
    agent runs itself: each VM is a transient unit of its own and outlives
    a stop or restart of it.

  It restarts a service whose unit it changed, or whose binary the `release`
  step replaced (the running process's `/proc/PID/exe` is then a deleted
  file), and nothing else. A changed socket unit, or one that is down,
  restarts the apiserver: systemd refuses to start a socket alone while its
  service runs, so `systemctl restart hangar-apiserver-node.socket` leaves
  the socket unit failed (the apiserver keeps serving it) until the next
  run restarts the apiserver. A unit written by a run that died before the
  restart leaves `/var/lib/hangar-bootstrap/restart-<unit>`, so the next run
  restarts it.

  The units are generated in full, so a drop-in for either
  (`hangar-node.service.d/`, or anything in `systemctl show -p DropInPaths`)
  makes `verify.sh` FAIL and the step refuse until it is removed: change
  the step instead.

  Both units `Requires=zfs-mount.service` and refuse to start while their
  datasets are not mounted. Restarting `zfs-mount.service` by hand restarts
  them too, and stopping it stops them: start them again yourself after
  starting it.
- `cloudflared` runs the Cloudflare tunnel, and only when
  `/etc/hangar/cloudflared.env` exists ("Switching to Cloudflare Access"
  below); without it the step installs nothing, stops and disables a
  tunnel that is up, and `verify.sh` reports its checks as SKIP. With it,
  it installs `cloudflared` at the version and checksum pinned in
  `versions.env` as `/usr/local/bin/cloudflared`, and writes, enables and
  starts `hangar-cloudflared.service`: `cloudflared tunnel --no-autoupdate
  --metrics 127.0.0.1:20241 run`, the token from the env file through
  `EnvironmentFile=`, `Restart=always`, as a `DynamicUser=` with no unix
  sockets (`RestrictAddressFamilies=AF_INET AF_INET6 AF_NETLINK`) and
  `/run/hangar`, `/var/lib/hangar` and `/etc/hangar` inaccessible
  (`systemd-analyze security hangar-cloudflared` shows the rest). It
  refuses, and stops a tunnel that is up, unless the apiserver runs in
  cloudflare auth, as `local.env` asks and as it was started: a tunnel
  must never reach an apiserver in dev auth, which trusts a header. The
  unit holds to that itself, however it is started (by hand, by
  `Restart=always` retrying, at boot): its `ExecStartPre=+` runs
  `/usr/local/libexec/hangar-cloudflared-guard`, which refuses unless the
  running apiserver's command line has `--auth-mode cloudflare` and so does
  its loaded unit, which a restart on failure would run, each set exactly
  once as `--auth-mode cloudflare`. It is `PartOf=` the apiserver, so it
  stops with it and its guard looks again when the apiserver restarts.
  `PartOf=` does not start it again: after a manual `systemctl stop
  hangar-apiserver` and `start`, the tunnel stays down (closed, not open)
  until `sudo systemctl start hangar-cloudflared`, a run of the
  `cloudflared` step, or a reboot; `systemctl restart hangar-apiserver`
  brings both back. A refused start keeps retrying every 5 s, so it
  comes up on its own once the apiserver is in cloudflare auth. The step
  and `verify.sh` count any state but `inactive` and `failed` as up,
  retrying included: such a unit with no `cloudflared.env`, or with the
  apiserver not in cloudflare auth, fails `verify.sh` and is stopped and
  disabled by the step. The `services` step, for the same reason, stops
  the tunnel before it would restart the apiserver into dev auth. A
  changed unit, a replaced binary or a new token restarts the tunnel. The
  tunnel's routes, DNS and Access application are set on Cloudflare's
  side (`docs/runbook/cloudflare.md`), not here.

## Deploying a new hangar version

VMs keep running across it: restarting `hangar-node` leaves their units
alone. A VM's unit runs `/usr/local/bin/hangar-node vmm` as its launcher, so
a VM started again after the upgrade (a restart policy, a disk change, a
reboot) runs the new launcher on the `spec.json` the old agent wrote, and
boots the new guest kernel and initramfs. A running VM keeps its own until
then.

1. Build it from the commit to deploy, on a testbed VM:
   `HANGAR_KERNEL_CACHE=/var/cache/hangar/guest-kernel/prebuilt make release`.
   The variable lets it reuse the kernel the testbed image prebuilt when the
   kernel's inputs are unchanged (`kernel: cache hit <key>`) instead of
   spending minutes building it; it builds and files the kernel otherwise
   (`kernel: built, cached under <key>`). The release is the same either way
   (`docs/runbook/guest-kernel.md`). Note its directory name,
   `hangar-<revision>`, and the last line it prints,
   `sha256sum SHA256SUMS: <digest>  SHA256SUMS`.
2. Copy `deploy/` from that commit to `/var/tmp/hangar/deploy` on the host,
   and the release directory to `/var/tmp/hangar/release/hangar-<revision>`.
   Keep the previous release's directory there: going back is the same
   sequence with it.
3. On the host, `sha256sum /var/tmp/hangar/release/hangar-<revision>/SHA256SUMS`
   must print the digest `make release` printed on the build machine. The
   release step checks every file against the `SHA256SUMS` in the
   directory, which only catches a copy corrupted on the way; this
   comparison is what ties the directory to the build.
4. On the host, one command per line:

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

The first deploy with the admin socket rewrites the apiserver's unit (the
sockets move to socket units, `/run/hangar` becomes root's), so it
restarts the apiserver once even with the auth mode unset; the agent
reconnects on its own and the VMs are not touched. Going back to a release
from before it leaves the two socket units behind and `admin.sock` owned by
`hangar`, unused; both are harmless, and the next upgrade puts them right.

Do not go back past the release that confines each VM's Cloud Hypervisor
to a uid of its own (`docs/decisions/node-agent.md`) while VMs run: the
older launcher refuses the `--vm-user` its VMs' units pass, so a VM that
restarts fails to start, and the older agent looks for each VM's socket at
`<dir>/vsock`, not `<dir>/ch/vsock`, so it cannot stop or exec into them.
Stop every VM first (`hangar stop` each container), or do not go back that
far. The ids it keeps in `/etc/subuid` and `/etc/subgid` stay, harmless.

To use the CLI on the host, as root on the host through the admin socket,
in any auth mode, once:
`sudo hangar config set-context local --endpoint unix:///run/hangar/admin.sock`
(the auth is `local`, and no credential is sent). Then
`sudo hangar --context local get nodes`, and so on: it holds
`platform.root` without a binding, which is the way back in when no one
else does (`sudo hangar --context local apply -f` a RoleBinding). Each
request on the socket is in the apiserver's log as `admin socket request`,
with the caller's pid, as `local:root@<node>`.

## Switching to Cloudflare Access

The public API moves from dev auth to Access tokens before any tunnel
exists, so a tunnel never reaches a header-trusting apiserver; local
administration goes through the admin socket throughout. Both values in
this section stay on the host only.

1. Add the Access application's settings to `/etc/hangar/local.env`, next
   to `HANGAR_BOOTSTRAP_ADMIN` (the last two are the `access.env`
   `cmd/hangar-cloudflare` writes, `docs/runbook/cloudflare.md`):

   ```sh
   HANGAR_AUTH_MODE=cloudflare
   HANGAR_CF_TEAM_DOMAIN=https://<team>.cloudflareaccess.com
   HANGAR_CF_AUDIENCE=<the Access application's AUD tag>
   ```

   Then `sudo deploy/host/bootstrap.sh --only services`. It refuses, before
   changing anything, when the mode is neither `dev` nor `cloudflare` or
   either value is missing. Otherwise the unit gets `--auth-mode cloudflare
   --cf-team-domain … --cf-audience …` and the apiserver restarts, still on
   `127.0.0.1:8080`. From here on a dev header on the public listener is
   refused; use the admin socket.
2. Put the tunnel's token in `/etc/hangar/cloudflared.env`, root's, mode
   0600. `cmd/hangar-cloudflare` writes the file, ready to copy, and
   `docs/runbook/cloudflare.md` says how to copy it without it passing
   through a terminal; by hand it is:

   ```sh
   sudo install -m 0600 /dev/null /etc/hangar/cloudflared.env
   sudo sh -c 'echo "TUNNEL_TOKEN=<token>" >/etc/hangar/cloudflared.env'
   ```

   Then `sudo deploy/host/bootstrap.sh --only cloudflared`.
3. Verify: `sudo deploy/host/verify.sh` passes, with `PASS apiserver runs
   cloudflare auth …` and the cloudflared lines instead of SKIP;
   `journalctl -u hangar-cloudflared` shows `Registered tunnel connection`;
   from a workstation, `hangar login` then `hangar get nodes` against
   `https://api.hangarlab.cloud`; and `curl -s -o /dev/null -w '%{http_code}'
   https://api.hangarlab.cloud/healthz` without a session is a redirect to
   Access, not 200. A full `bootstrap.sh` then prints `changes: 0`.

A new token is the same step 2: the run restarts the tunnel.

To roll back to dev auth, in the reverse order:

1. `sudo rm /etc/hangar/cloudflared.env`, then `sudo
   deploy/host/bootstrap.sh --only cloudflared`: it stops and disables
   `hangar-cloudflared` (the binary and unit stay, unused).
2. Remove the three `HANGAR_AUTH_MODE` and `HANGAR_CF_*` lines from
   `local.env` (or set `HANGAR_AUTH_MODE=dev`), then `sudo
   deploy/host/bootstrap.sh --only services`: the apiserver restarts in
   dev auth. `verify.sh` passes and reports
   cloudflared as SKIP.

Getting the order wrong is caught, not just documented: with
`cloudflared.env` present and dev auth asked for, a full run stops the
tunnel before the apiserver restarts and then refuses, and `verify.sh`
fails until one of the two files is fixed. Dev auth also refuses any
request carrying a header Cloudflare adds: `Cf-Warp-Tag-Id`, which
cloudflared sets on every request it proxies, and `Cf-Connecting-Ip`,
`Cdn-Loop` and `Cf-Ray` from its edge.

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
  long). The logs are tenant data, at most 34 MiB a VM. It keeps
  `devices=on`: each VM's directory holds a node of its root disk, which
  its Cloud Hypervisor opens.
- The ids 200000 to 265535 for the VMs, one each as its uid and gid
  (`docs/decisions/node-agent.md`): no user or group may have one, nor any
  subordinate range in `/etc/subuid` or `/etc/subgid` overlap them, or
  `hangar-node` refuses to start. The `services` step keeps them there as
  `hangar-vms:200000:65536`, so `useradd` gives them to no one. While
  anything else has one, a run refuses before any step, with nothing
  installed: move it to other ids (`usermod`, `groupmod`, or its line in
  `/etc/subuid` or `/etc/subgid`) and run again.
- `hangar-node` and Cloud Hypervisor statically linked (a release builds
  `hangar-node` so, and the pinned Cloud Hypervisor is the static build):
  each VM's unit has an empty root, with no dynamic loader.
- systemd 254 or later, for `RestartSteps=` and `RestartMaxDelaySec=` in
  the VMs' units (Ubuntu 24.04 has 255), and its D-Bus API: the system bus,
  or, as root, systemd's private socket.
- `/dev/kvm`, open to its group or to everyone; a VM's Cloud Hypervisor
  gets its group. Not `vhost_vsock`: Cloud Hypervisor's hybrid vsock is its
  own device, reached through a unix socket.
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
a subordinate range among the VMs' ids (`old:165536:65536` in
`/etc/subuid`) is refused before any step on a fresh host, with nothing
installed, the release step alone included, and on an installed one is
reported by `verify.sh` and refused with nothing changed;
a new release restarts both services, and so does going back; a changed
unit restarts its service only, also when a run died before the restart; a
drop-in for a unit fails verify and is refused; the node does not start
while `hangar/images` is not mounted; and drift in the datasets, the
installed files, the apt timers and the services is reported and repaired.
For the admin socket and the tunnel it checks: `/run/hangar` and both
sockets have their owners and modes; root on the admin socket gets and
binds without a binding of its own and is logged as `local:root@testbed`;
other users are kept out by the socket's mode and, with the mode opened,
by the apiserver's uid check; dev auth refuses Cloudflare's edge headers;
the agent reconnects when the apiserver and its sockets restart, and a
socket restarted alone is reported and repaired; without `cloudflared.env`
nothing about cloudflared changes and verify reports SKIP; with it, in dev
auth, the step refuses and installs nothing; half a cloudflare mode is
refused before any step; the switch to cloudflare auth restarts the
apiserver with the flags, still on loopback; then, with a fake token,
cloudflared is installed, enabled and started, and a second run changes
nothing; `systemd-analyze security` rates the tunnel's unit at most 2.0,
and a probe under the same sandbox can neither open a unix socket nor
list hangar's directories, nor reach the sockets by path with unix
sockets allowed; a new token restarts the tunnel only; the rollback in
reverse order; and dev auth asked for with the tunnel up stops the tunnel
before the apiserver changes. The unit's guard: restarting the apiserver
restarts the tunnel; with the apiserver stopped a hand-started tunnel is
refused and retries, and comes up once the apiserver is back; a loaded
apiserver unit in dev auth is refused too. A tunnel started by hand
without its env file retries, fails `verify.sh`, is not let through by the
env file appearing while the apiserver is in dev auth, and is stopped by
the step, with the env file or without.
It does not reboot. Run it as root on a disposable VM only. `make lint-sh` runs
shellcheck over every script.

`HANGAR_TESTBED_FLOW_INSTALLED=1` runs the L4 flow (`docs/runbook/testbed.md`)
against the services the steps installed instead of processes of its own.
