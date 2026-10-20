# Host bootstrap

`deploy/host/` brings a freshly installed host to the state hangar expects:
packages and a pinned Cloud Hypervisor, swap mirrored on md RAID1, the `hangar`
pool's properties and datasets, netplan owned by us with IPv6 off the
prefix's `::` address, the host's firewall, sshd for keys only, proxy NDP,
forwarding, and vsock, unattended security
upgrades in a fixed window, a hangar release (the binaries, the guest kernel
and the initramfs), `hangar-apiserver` and `hangar-node` as systemd
services, the front door, `hangar-edge`, once it has its settings, and the
host's monitoring: Prometheus, Alertmanager sending to Telegram, and their
exporters. It is safe to re-run.

## Before the first run

The OS install itself is done through the OVH API with a custom partition
layout: `/boot` 1 GiB, `/` 100 GiB and swap 8 GiB on software RAID1, and the
rest as a ZFS partition with `raidLevel: 1` and pool name `hangar`, which the
installer turns into a mirrored pool. The installer does not mirror swap and
gives the host its prefix's `::` address; the bootstrap fixes both.

Write the host env file, `/etc/hangar/host.env`, root's, mode 0600.
`deploy/host/host.env.example` lists every variable it takes, with example
values only. Every value in it is explicit (uplink name, public IPv4, IPv6
address and gateway, the two swap partitions, the dataset quotas, the
memory kept from VMs, the Node's name, how many tenants one person may own
unless their User says otherwise); the scripts never guess a disk or an
interface. The real file is the operator's and is kept with them, never in
this repository:

```sh
sudo install -D -m 0600 -o root -g root host.env /etc/hangar/host.env
```

`bootstrap.sh` and `verify.sh` read it unless `--env FILE` names another
file (a testbed does that). They source it as root, so they refuse a file
that is not root's or that anyone else can write, before reading it;
`verify.sh` also checks it is mode 0600.

Two values stay on the host and out of the repo: the bootstrap admin, the
email of the Google account the person signs in with, who is granted
`platform.root` while no one holds it, and the auth mode. Write them once,
by hand, as root's, mode 0600 (`<email>` is the address itself):

```sh
sudo install -D -m 0600 /dev/null /etc/hangar/local.env
printf 'HANGAR_BOOTSTRAP_ADMIN=<email>\nHANGAR_AUTH_MODE=dev\n' | sudo tee /etc/hangar/local.env
```

The `services` step refuses to run without the bootstrap admin, and
`verify.sh` checks it. The apiserver reads the file through
`EnvironmentFile=`. `HANGAR_AUTH_MODE` is `dev` (a header names the
caller, on loopback only) until the host signs people in through Google,
when it becomes `oidc` ("Switching to sign-in through Google" below).
Nothing defaults it: `bootstrap.sh` refuses, before any step, a
`local.env` that sets no `HANGAR_AUTH_MODE`, and `verify.sh` fails it.
A host set up before it had to be named, still in dev auth, needs
`HANGAR_AUTH_MODE=dev` added before its next `bootstrap.sh`.

The grant only invites the bootstrap admin: like everyone, they sign up
once after the first start, before any other call works
(`docs/decisions/tenancy-and-iam.md`). From a workstation whose context
signs in as that email, `hangar signup --username <name>`; `hangar whoami`
shows where they stand. Signing up also opens a tenant and a project for
them, which their context then uses. Root on the host, through the admin
socket, needs no User.

Everyone else is invited by platform root: `hangar admin invite EMAIL`,
with `--plan PLAN` for a paying user's bigger plan (the default plan is a
trial's) and `--max-tenants N` for more tenants than the platform default.
Their User starts with both when they sign up. Someone who already holds
a platform binding keeps it: inviting them adds only the Invitation.
`hangar admin invitations` lists everyone invited who has not signed up,
including people bound by hand, who start with the defaults, and `hangar
admin revoke EMAIL` withdraws an invitation: it deletes every platform
binding naming the email and its Invitation, and prints what it deleted.

A host installs only releases published on GitHub: a `v*` tag makes
`.github/workflows/release.yml` build one and attest every file of it
(`docs/runbook/release.md`). What a host runs of a release is one file of
it, `hangar-host_TAG_linux_amd64.tar.gz`, which holds `hangar-apiserver`,
`hangar-node`, `hangar-edge` and `hangar` (static), `vmlinux`,
`initramfs.cpio`, `console.tar` (the console's built files), `deploy.tar`
(`deploy/` itself), `VERSION` (the full revision, as the binaries report
it) and `SHA256SUMS` over all of them; `hangar-fetch-release` unpacks it
into the directory `bootstrap.sh --release` takes. The workflow builds it
with `deploy/release.sh`, which `make release` runs by hand too
on a machine with the guest kernel's toolchain
(`docs/runbook/guest-kernel.md`; the committed kernel config is Ubuntu
24.04's GCC's) and with Node and pnpm (`docs/runbook/dev-env.md`) for the
console, writing `bin/release/hangar-<revision>/`, whose `host/` is what a
testbed installs. It refuses a tree with uncommitted changes (`ALLOW_DIRTY=1` for a
test build). Built twice from one commit with one toolchain, it gives the
same `SHA256SUMS`. Nothing it builds is committed.

## Run

Fetch the release ("The first fetch on a new host" below, then
`hangar-fetch-release TAG DIGEST`), then as root, with `DEPLOY` its
`/var/tmp/hangar/deploy-TAG` and `DIR` its `/var/tmp/hangar/release/TAG`:

```sh
DEPLOY/host/bootstrap.sh --release DIR --dry-run   # diffs and "would:" lines, changes nothing
systemd-run --unit=hangar-bootstrap --wait --collect DEPLOY/host/bootstrap.sh --release DIR
journalctl -u hangar-bootstrap       # its output; ends with "changes: N"
DEPLOY/host/verify.sh                # one PASS/FAIL per check, exit 1 on any FAIL
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
(`packages`, `swap-raid`, `zfs`, `network`, `firewall`, `sshd`, `sysctl`, `upgrades`,
`release`, `services`, `edge`, `monitoring`).

## Datasets, upgrades, the release and the services

- `zfs` also makes `hangar/images` (`/var/lib/hangar/images`,
  `setuid=off exec=off devices=off`, quota `HANGAR_IMAGES_QUOTA`) and
  `hangar/vms` (`/var/lib/hangar/vms`, `setuid=off exec=off`, quota
  `HANGAR_VMS_QUOTA`) and `hangar/natlog` (`/var/log/hangar/nat`, the NAT
  log, `setuid=off exec=off devices=off compression=on`, refquota 20G, which
  the agent keeps under 90% of, so a snapshot cannot keep removed days
  counted), all mount points `0700` and root's,
  mounted by `zfs-mount.service` at boot; `hangar-node` does not start
  without the NAT log's mounted. `hangar/vms` must outlive a reboot: the node
  reads each VM's `spec.json` and `exit.json` after one. It also keeps
  `hangar/state` at `/var/lib/hangar/state` (the `services` step owns its
  mount point) and `hangar/poc`, for throwaway experiments, never mounted:
  volumes under it are reached as `/dev/zvol` paths.
- `firewall` writes `/etc/hangar/firewall.nft` (root's, 0600) and
  `hangar-firewall.service`, which loads it before the network comes up,
  and loads it itself whenever the file changed or the table is not what it
  loaded last (`/var/lib/hangar-bootstrap/firewall.live`). It is one nft
  table, `inet hangar-host`, whose input chain drops everything, IPv4 and
  IPv6, on every interface but loopback, except replies, ICMP and ICMPv6,
  the uplink's DHCPv4 replies, and TCP 22 (SSH), 80 and 443 (the front
  door; `docs/decisions/front-door.md`). It has no forward chain, so the
  guests' traffic is the node agent's tables' alone. Before it loads the
  table it warns of every listener it closes: anything listening on an
  address other than loopback, on a port other than those. It never uses
  `nftables.service`, whose stock `/etc/nftables.conf` flushes every
  table, the node agent's too: do not enable it. To open a port, change
  `FIREWALL_TCP_PORTS` in the step and re-run it.
- `sshd` writes `/etc/ssh/sshd_config.d/10-hangar.conf` (root's, 0644):
  password and keyboard-interactive logins off, no root login, keys read
  from `~/.ssh/authorized_keys` only (not `authorized_keys2`),
  `AllowUsers` the users `HANGAR_SSH_USERS` names (`ubuntu` when unset),
  no X11 forwarding, `MaxAuthTries 4` and `LoginGraceTime 30`. TCP
  forwarding is left on. sshd reads `sshd_config.d/*.conf` in name order
  and keeps the first value it reads, so the file sorts before the cloud
  image's `50-cloud-init.conf` and `60-cloudimg-settings.conf`, which stay
  as installed. Before it writes the file the step has `sshd -t` check a
  copy of the config with the new file in place of the old one (so a
  broken file of ours is still replaced), and refuses when sshd refuses.
  It also refuses, before writing, a list that would lock someone out:
  `root` in `HANGAR_SSH_USERS` (the host env file is refused with it), no
  listed user with a key in `~/.ssh/authorized_keys`, a listed user whose
  keys are all in `authorized_keys2`, or a user who can log in with a key
  today and could not after. Who can log in today is worked out from the
  config on disk at the start of the run, what a reload would load (with
  our drop-in left out if sshd refuses it): every user but root with a
  login shell (not `nologin` or `false`) and a key in a file its
  `AuthorizedKeysFile` names, whom its `AllowUsers`, `DenyUsers`,
  `AllowGroups` and `DenyGroups` do not keep out. Logins through
  `AuthorizedKeysCommand`, `TrustedUserCAKeys` or `AuthorizedPrincipals*`
  are not counted; a run warns when the host sets any of them. On a fresh
  host that is every user with a key, so list them all, or move or remove
  their keys first. To let some go, name them in `HANGAR_SSH_DROP_USERS`
  in the run's environment (`sudo HANGAR_SSH_DROP_USERS="alice bob" …`,
  or `systemd-run --setenv=HANGAR_SSH_DROP_USERS="alice bob" …`): the run
  names each one it shuts out. The host env file is refused if it sets
  it. After writing it runs `sshd -t` again and puts the old file back if
  that fails, which only happens if the host changed between the two
  checks. It then reloads `ssh.service` if it runs (a reload keeps every
  open session); when only `ssh.socket` does, sshd reads the file at the
  next connection. Every run that writes the file reloads, and so does a
  run that finds the file not what it last reloaded with
  (`/var/lib/hangar-bootstrap/sshd.loaded`, removed by a run that finds no
  drop-in), as after a run that died before the reload.
  `sshd -t` and `-T` need `/run/sshd`, which `ssh.service` makes only when
  it starts: a real run makes it, while a dry run skips those checks with a
  note and `verify.sh` prints a SKIP line for them. `verify.sh` checks
  the settings in `sshd -T -C` for each listed user logging in over
  loopback, so a drop-in sorting before ours
  or a `Match User` block that loosens one fails it, and fails on any
  `Match` block in `sshd_config` or `sshd_config.d`, which a check for
  loopback cannot see through. With `MaxAuthTries 4`, a client that
  offers more than three other keys first is cut off: name the key (`IdentitiesOnly yes` and
  `IdentityFile` in `~/.ssh/config`). After a run that changed the file,
  log in again from a second terminal before closing the first. To back
  out by hand: `sudo rm /etc/ssh/sshd_config.d/10-hangar.conf && sudo
  systemctl reload ssh` (the next run writes it again).
- `upgrades` keeps Ubuntu's unattended upgrades to their default origins
  (security only), never reboots, and moves `apt-daily.timer` to 03:30 and
  `apt-daily-upgrade.timer` to 04:00 in `HANGAR_UPGRADES_TZ` (UTC unless
  the host env file sets it; a zone the host does not know is refused before
  any step), with no random delay
  and nothing made up at boot: an upgrade can re-exec systemd and trigger
  udev on every device. It also tells needrestart to leave every `hangar-*`
  unit alone: after a hangar release every VM's launcher looks stale to it.
- `release` checks every file of `--release DIR` against its `SHA256SUMS`
  and refuses the whole release on a mismatch or a missing file, before
  installing anything. It installs the binaries in `/usr/local/bin` and the
  kernel, the initramfs and `console.tar` in `/usr/local/lib/hangar/`, each
  by a rename, then records `VERSION` and the checksums by installed path in
  `/usr/local/lib/hangar/`. `verify.sh` checks them with `sha256sum -c`. It
  unpacks the console into `/usr/local/share/hangar/console.d/<checksum>`,
  with the previous release's assets beside its own so pages opened before
  the switch still load, and points the link
  `/usr/local/share/hangar/console` at it in one rename. It keeps the
  previous release's console and removes older ones; going back to it
  copies the newer release's assets into it the same way. `verify.sh` checks the
  link is the installed tar's. A release from before the console carried
  none, and its record is whole without it. A run without `--release`
  repairs a console link that does not match the installed tar. With or
  without `--release`, it installs `deploy/host/fetch-release.sh` from the
  `deploy/` it runs from as `/usr/local/sbin/hangar-fetch-release` (root's,
  0755), which `verify.sh` compares with that file.
- `packages` also installs the GitHub CLI `hangar-fetch-release` verifies
  attestations with, as `/usr/local/bin/gh`: the version and checksums
  pinned in `deploy/host/versions.env`, from the GitHub release's tarball,
  as Cloud Hypervisor is pinned (the file says why not apt). `verify.sh`
  checks the binary's sha256.
- `services` makes the system users `hangar` and `hangar-edge`, gives
  `hangar` `/var/lib/hangar/state` (mode 700), and writes, enables and
  starts `hangar-apiserver.service` and `hangar-node.service`:
  - the apiserver as `hangar`, sandboxed (`systemd-analyze security
    hangar-apiserver` shows what is left), its database on `hangar/state`,
    the public API on `127.0.0.1:8080` in the auth mode `local.env` names
    (`HANGAR_AUTH_MODE`, never defaulted; in oidc auth with the OAuth
    client's secret from
    `/var/lib/hangar/state/oidc-client-secret`, "Switching to sign-in
    through Google" below), `--default-max-owned-tenants
    HANGAR_DEFAULT_MAX_OWNED_TENANTS`, and three unix sockets that systemd
    makes and passes it (`hangar-apiserver-node.socket`,
    `hangar-apiserver-admin.socket` and `hangar-apiserver-edge.socket`,
    `PartOf=` the apiserver, so they stop and restart with it): the node
    socket `/run/hangar/node.sock` (`hangar`'s, 0660), accepting uid 0 as
    the Node `HANGAR_NODE_NAME`, the admin socket `/run/hangar/admin.sock`
    (root's, 0600), where root on the host is `platform.root` whatever the
    bindings say, and the edge socket `/run/hangar/edge.sock` (root's and
    group `hangar-edge`'s, 0660), accepting `hangar-edge`'s uid as the Edge
    principal (`docs/decisions/authentication.md`). With the front door on
    ("The front door" below), a fourth, `hangar-apiserver-public.socket`:
    `/run/hangar/public.sock` (root's and group `hangar-edge`'s, 0660),
    where the apiserver serves the public API to `hangar-edge`'s uid alone,
    and believes the client address it sends (`--public-socket`); turning
    the front door off stops and removes it. `/run/hangar` is
    root's, 0755, from `/etc/tmpfiles.d/hangar.conf`, so the apiserver can
    remove or replace no socket. With `HANGAR_EDGE_DOMAIN` in `local.env`,
    it gets `--endpoint-domain`. It no longer calls
    Cloudflare: the step warns while a token it once did with is left at
    `/var/lib/hangar/state/cloudflare-api-token` ("Retiring the endpoints'
    Access objects" in `docs/runbook/cloudflare.md`);
  - the agent as root: `--drivers=host --name HANGAR_NODE_NAME --uplink
    HANGAR_UPLINK --public-ipv4 HANGAR_PUBLIC_IPV4 --public-ipv6` the
    address of `HANGAR_IPV6_ADDR` (the host's two public addresses, where
    machines may reach the front door's :80 and :443 and nothing else of
    the host; `docs/decisions/networking.md`), `--public-ipv6-prefix` its
    /64, from which every machine interface gets its public IPv6 address,
    `--image-cache-high 20Gi` (what the container images no machine uses
    may keep, `docs/decisions/node-agent.md`), `--reserved-disk` the sum
    of both quotas, Prometheus's dataset's quota (`MON_TSDB_QUOTA` in
    `deploy/host/lib.sh`) and those 20 GiB, the bases of those images being on
    the pool, `--reserved-memory HANGAR_RESERVED_MEMORY` (the ARC cap
    plus the host's own overhead), `--edge-dial-socket
    /run/hangar/edge-dial.sock --edge-user hangar-edge` (the agent makes
    that socket, root's and group `hangar-edge`'s, 0660, and takes
    `hangar-edge`'s dials alone), `--region`, `--site`, `--chassis`,
    `--memory` and `--network` from `HANGAR_REGION`, `HANGAR_SITE`,
    `HANGAR_CHASSIS`, `HANGAR_MEMORY` and `HANGAR_NETWORK`, each only when
    the host env file sets it and each quoted, with `\`, `"`, `%` and `$`
    escaped as systemd reads them (such as `region-1`, `site-a`,
    `generic-1u-2x-u2`, `DDR4-3200 ECC` and `2 × 10 GbE`; the step refuses
    the run unless `hangar-node check-placement` accepts them, the agent's
    own rule: at most 64 printable characters each, the chassis a DNS-1123
    label. It runs the hangar-node the run installs: with `--release`, the
    release's, as a private executable copy that matches the release's
    `SHA256SUMS` (a fetched release's files are 0644), made in a directory
    of its own, 0700, under `/usr/local/lib` (where the release's binaries
    run from, so never on a noexec `/tmp`) and removed when the check ends,
    on SIGINT or SIGTERM too; without, the
    installed one. Only a hangar-node whose own answer to `-h` is a usage
    without `check-placement`, one from before it, makes it warn that the
    values were not checked instead; one that cannot run is a problem, and
    the run refuses. Unset, the agent reads memory and
    network from the host as it starts; every tenant on the node reads
    them, so never put load or capacity in them,
    `docs/decisions/machine-placement.md`), every path its default,
    `Restart=always`. Its `KillMode=control-group` kills only what the
    agent runs itself: each VM is a transient unit of its own and outlives
    a stop or restart of it.

  Each serves its metrics on loopback for the `monitoring` step's
  Prometheus ("Monitoring and alerts" below): the apiserver with
  `--metrics-listen 127.0.0.1:9101`, the agent with `--metrics-listen
  127.0.0.1:9102`. The step passes the flag only when the binary the run
  installs takes it, which it tells as it tells an old hangar-node for
  `check-placement` (its answer to `-h`, the release's copy with
  `--release`); a binary from before the flag runs without it, the step
  warns, and Prometheus does not scrape it. `verify.sh` checks each serves
  its own metrics there, or says SKIP.

  It restarts a service whose unit it changed, or whose binary the `release`
  step replaced (the running process's `/proc/PID/exe` is then a deleted
  file), and nothing else. A changed socket unit, or one that is down,
  restarts the apiserver: systemd refuses to start a socket alone while its
  service runs, so `systemctl restart hangar-apiserver-node.socket` leaves
  the socket unit failed (the apiserver keeps serving it) until the next
  run restarts the apiserver. A unit written by a run that died before the
  restart leaves `/var/lib/hangar-bootstrap/restart-<unit>`, so the next run
  restarts it.

  The units are generated in full, so a drop-in for any
  (`hangar-node.service.d/`, or anything in `systemctl show -p DropInPaths`)
  makes `verify.sh` FAIL and the step refuse until it is removed: change
  the step instead.

  Both units `Requires=zfs-mount.service` and refuse to start while their
  datasets are not mounted. Restarting `zfs-mount.service` by hand restarts
  them too, and stopping it stops them: start them again yourself after
  starting it.
- `edge` runs `hangar-edge.service` as `hangar-edge`, sandboxed as the
  apiserver is, only when `local.env` has `HANGAR_EDGE_DOMAIN`, which needs
  oidc auth and `HANGAR_CP_DOMAIN`, or the step refuses: it is the front
  door on :443 and :80 ("The front door" below), the only place public
  endpoints are served, with the apiserver's edge socket and the agent's
  dial socket, sending browsers not yet signed in to a private endpoint to
  the console, `HANGAR_PUBLIC_URL` (`--console-url`;
  `docs/decisions/public-endpoints.md`), and with the host's /64 as its own
  prefix (`--own-prefix`), whose addresses, its machines', the limits count
  one by one. Without `HANGAR_EDGE_DOMAIN` the step stops and disables it
  and its sockets, and `verify.sh` checks it is off. A changed unit or
  socket unit, or a replaced binary, restarts it, and the restart restarts
  its sockets (`PartOf=`): :443 and :80 are closed for that moment, and
  new connections are refused. So is `systemctl restart hangar-edge` by
  hand. A restart systemd does on its own after a crash (`Restart=always`)
  keeps the sockets, and new connections wait for the edge. `verify.sh`
  checks it runs as `hangar-edge` with no capability at all, listens on
  `*:443` and `*:80` and on nothing else but loopback (a testbed's edge
  answers DNS-01 there, and it serves its metrics on `127.0.0.1:9103`,
  `--metrics-listen`, when the release's hangar-edge takes the flag, as for
  the services above), that it serves its metrics there, and, with a TLS
  handshake to the
  host's own :443 for each name, that it serves a certificate the system's
  roots trust, valid for the name, with more than 14 days left, and that
  `api.`'s `/healthz` says `ok`.

  The step also retires the Cloudflare tunnel, which no step installs any
  more. While `/etc/hangar/cloudflared.env` is there, it leaves a tunnel
  an earlier release installed as it is, says so, and `verify.sh` reports
  it as SKIP; once the file is gone, it stops and disables
  `hangar-cloudflared` and removes its unit, its guard
  (`/usr/local/libexec/hangar-cloudflared-guard`) and
  `/usr/local/bin/cloudflared`, and `verify.sh` fails while any of them is
  left. The `services` step stops such a tunnel before it would restart
  the apiserver into dev auth, which trusts a header. The cutover is in
  `docs/runbook/cloudflare.md`.
- `monitoring` installs the host's monitoring
  (`docs/decisions/monitoring.md`; "Monitoring and alerts" below).

## The front door

With the front door on, `hangar-edge` takes :443 and :80 on every address,
IPv4 and IPv6, ends TLS with certificates it gets over ACME DNS-01, and
routes `api.<cp-domain>`, `console.<cp-domain>` and `*.<user-domain>`
(`docs/decisions/front-door.md`). DNS points the names at the host,
DNS-only (`docs/runbook/cloudflare.md`); on a host that ran the Cloudflare
tunnel, the cutover there says in which order. It needs oidc auth
("Switching to sign-in through Google" below).

1. Install the DNS token as `/etc/hangar/cloudflare-dns-token`, root's,
   0600 (`docs/runbook/cloudflare.md` says how to make it, and how to
   install it without it passing through a terminal). The `edge` step
   refuses the front door without it.
2. Add to `/etc/hangar/local.env`:

   ```sh
   HANGAR_EDGE_DOMAIN=hangarlab.dev      # public endpoints are under it
   HANGAR_CP_DOMAIN=hangarlab.cloud      # api. and console. are under it
   HANGAR_ACME_EMAIL=ops@example.com     # the operator's address: the ACME account's contact
   ```

   `HANGAR_ACME_EMAIL` is required; the `edge` step and `hangar-edge`
   refuse the front door without it. Without an email, certmagic's
   process-wide email discovery can register the account as
   `mailto:default` on a first start, which the CA refuses
   (`docs/decisions/front-door.md`).

   The two domains must not share a registrable domain
   (`docs/decisions/public-endpoints.md`), siblings such as
   `cp.example.com` and `user.example.com` included. Before any step,
   `bootstrap.sh` (a full run, or `--only edge`) refuses what the `edge`
   step would refuse, and runs `hangar-edge check-flags` on the very flags
   the unit's `ExecStart` gets: it refuses the run on any of the edge's
   checks of its flags, this one among them, so a refused run changes
   nothing, not the release's binaries, not the unit, not the running
   edge. The hangar-edge that checks is the one the unit runs after the
   run, found as the services step finds `hangar-node` for
   `check-placement` (above): in a full run with `--release`, a private
   copy of the release's that matches its `SHA256SUMS`; without
   `--release`, and with `--only edge` whether or not `--release` is
   given (the release step is skipped, so the unit keeps the installed
   binary), the installed one. The `edge` step checks again with the
   installed one before it changes the unit. `--only release`, which
   installs a hangar-edge the running unit takes up at its next restart,
   runs the release's `check-flags` before any step too. A hangar-edge
   from before `check-flags` makes it warn that the flags were not
   checked; one that cannot run is a problem.

   In oidc auth, `HANGAR_PUBLIC_URL` must then be
   `https://console.<HANGAR_CP_DOMAIN>`: Google returns to its
   `/auth/callback`, which the front door hands the apiserver with the rest
   of sign-in (`docs/decisions/authentication.md`); the `edge` step
   refuses another.

   `HANGAR_ACME_CA` names another ACME directory, such as Let's Encrypt's
   staging (`https://acme-staging-v02.api.letsencrypt.org/directory`), and
   `HANGAR_ACME_CA_ROOT` a PEM file of the only roots its TLS is checked
   against. Both are for tests. So is `HANGAR_ACME_DNS`, which only an
   edge built with the `testbed` tag takes (`docs/runbook/testbed.md`).
3. Re-run `deploy/host`. The `services` step restarts the apiserver with
   its public socket, and the `edge` step writes
   `hangar-edge-https.socket` (`ListenStream=443`) and
   `hangar-edge-http.socket` (`ListenStream=80`), both
   `BindIPv6Only=both`, and restarts the edge with them,
   `StateDirectory=hangar-edge` and the token as a credential
   (`LoadCredential=cloudflare-dns-token:/etc/hangar/cloudflare-dns-token`).
4. Watch it get its certificates: `journalctl -u hangar-edge | grep
   'certificate obtained'`, one for each of the three names, within a
   minute or two. Until then a handshake for a name without one fails. They
   are kept in `/var/lib/hangar-edge` (the edge's, 0700) and renewed on
   their own; a restart serves them from there. `verify.sh` fails each name
   that has no certificate there with more than 14 days left, so it fails
   until they are in; later it means a renewal has been failing for two
   weeks, as certmagic renews with about 30 left (`journalctl -u
   hangar-edge` says why). `verify.sh` also asks the host's own :443 for
   each name, as a client from outside would, short of DNS. Check one from
   outside before DNS points at the host: `curl --resolve
   api.<cp-domain>:443:<host IPv4> https://api.<cp-domain>/healthz` prints
   `ok`.

The console's files are served from `/usr/local/share/hangar/console`, which
the `release` step keeps; until a release puts them there, the console's
pages answer 503 "the console is not installed", and its API paths work.
The limits (connections and requests per client, sizes, timeouts, HTTP/2)
are `hangar-edge`'s flags,
with the defaults in `docs/decisions/front-door.md`; change them in the
`edge` step. To turn the front door off, remove `HANGAR_EDGE_DOMAIN` and
`HANGAR_CP_DOMAIN` and re-run: the edge, its sockets and the apiserver's
public socket go, and :443 and :80 with them; the public endpoints with
them, as the edge serves them nowhere else.

## Deploying a new hangar version

Upgrading past the change that stopped defaulting the auth mode to dev:
`bootstrap.sh` refuses, before any step, a `/etc/hangar/local.env` with
no `HANGAR_AUTH_MODE`. A host whose `local.env` sets none ran in dev
auth: add `HANGAR_AUTH_MODE=dev` before the first run of that version
and its mode stays as it was (`oidc`, with the rest of "Switching to
sign-in through Google" below, changes it). A host that already names
its mode, as one switched to Google sign-in does, is untouched.

Upgrading past the change that moved the host env file out of this
repository: install the host's file as `/etc/hangar/host.env`, root's, mode
0600, from the operator's copy, before the first run of that version; it
refuses to start without it, and `--host` is gone. A file anyone but root
can write is refused too. The upgrade window's time zone, fixed in the
step before, is now `HANGAR_UPGRADES_TZ` in that file, UTC when unset: set
it to the zone the host's window was in, or the timers move (a dry run
shows them as `would:` lines). `deploy/host/host.env.example` lists every
variable, the placement ones (`HANGAR_REGION` and the rest) included.

The host has no `hangar-fetch-release` yet: fetch that release as "The
first fetch on a new host" below says. Before that, check
`/var/tmp/hangar` and `/var/tmp/hangar/release`: deployed the old way,
they were copied by hand and are likely the copying user's, and the fetch
refuses a directory that is not root's or that anyone else can write.
`stat -c '%U %a %n' /var/tmp/hangar /var/tmp/hangar/release` must say
`root` and no group or other write bit. If not, move the old copies out of
the way rather than trusting them, and let the fetch make both again:

```sh
sudo mv /var/tmp/hangar /var/tmp/hangar.old-$(date +%Y%m%d)
```

Keep the moved directory until the new release is verified: going back to
the release installed before runs from the copy in it. Then remove it.

Upgrading past the change that added quotas: add
`HANGAR_DEFAULT_MAX_OWNED_TENANTS` (1, one tenant per person) to the host
env file first; the run refuses to start
without it. The first start of that version makes the `default` Plan
with placeholder limits, and every tenant without a plan of its own,
existing ones included, gets them at once. Set the real limits right
after the deploy ("Quotas" below). A tenant already past a placeholder
limit keeps what it has, but nothing in it can grow until the limit is
raised. The per-person tenant cap counts
the tenants each person opened by the `hangarlab.cloud/opened-by`
annotation, which only tenants opened from that version on carry: tenants
opened before it count toward no one's cap.

Upgrading past the change that added `FileShare`: the `default` Plan
already in the store keeps its limits, which have no `fileShares`, so
tenants may make any number of shares until it is set ("Quotas" below).
A share's size counts toward `disk` from the start.

Upgrading past the change that added Invitations and the tenant signing
up opens: existing Users keep what they have, and only people who sign up
from then on get a tenant and a project made for them, which counts toward
their cap. It is one tenant per person: the apiserver's
default cap is 1, that tenant, and the host env file sets
`HANGAR_DEFAULT_MAX_OWNED_TENANTS=1` to match. People who already own more
tenants keep them all; only opening another is refused. Platform root
raises one person's cap with `spec.maxOwnedTenants` on their User ("Quotas"
below). The `default` Plan already in the store keeps its limits; set the
new ones by hand ("Quotas" below).

Upgrading past the change that added `User` (`hangar signup`): from the
first start of that version, every existing member, the operator included, is
refused on every call until they run `hangar signup --username NAME` once;
their bindings stay and count again as soon as they have. Plan the deploy
with them. Service tokens, such as the smoke token, and root on the host
through the admin socket are unaffected.

Upgrading past the change that added Hangar's own service tokens: a
RoleBinding's `service_token` subject now names a `ServiceToken` as
`<project>/<name>`, not a Cloudflare Access client ID, so a binding made
for an Access token grants nothing any more. Make the smoke token ("The
smoke token" below) and delete the old binding. Editors
(`project.editor`) can no longer write service tokens; owners can.

VMs keep running across it: restarting `hangar-node` leaves their units
alone. A VM's unit runs `/usr/local/bin/hangar-node vmm` as its launcher, so
a VM started again after the upgrade (a restart policy, a disk change, a
reboot) runs the new launcher on the `spec.json` the old agent wrote, and
boots the new guest kernel and initramfs. A running VM keeps its own until
then.

**Upgrading to the release that records image sources** (the first with
Secrets, `docs/decisions/secrets.md`): the node's image cache from before
has no record of which repository each image came from, and the new agent
trusts none of it. Running machines are not touched. The new agent fetches
each machine's image once more, in full, from that machine's own registry,
in the background soon after it starts, and starting a machine again (a
host reboot, a stop and start, or a change that restarts it) waits for that
fetch: a registry that is down blocks the start, the machine saying
`ImagePullFailed`, until it is back. Expect one download of each image, and
do not reboot the host or restart machines during the upgrade while a
registry they need is unreachable.

A version reaches a host as a tag, never as a copy from a build machine:

```
tag vX.Y.Z ──▶ release workflow ──▶ GitHub Release (files, attestation.jsonl,
                                     the SHA256SUMS digest in its notes)
                                          │
   host: hangar-fetch-release TAG DIGEST ◀┘
         ──▶ dry run ──▶ run ──▶ verify ──▶ changes: 0
```

1. Tag the commit and let the workflow publish the release
   (`docs/runbook/release.md`). Note `TAG` and the digest the release's
   notes state, `DIGEST`. A pre-release (`-rc.N`) goes to a testbed first.
2. On the host, fetch it:

   ```sh
   sudo hangar-fetch-release TAG DIGEST
   ```

   It downloads `SHA256SUMS`, `VERSION` and the host tar over HTTPS, and
   none of the CLI's files, and refuses the release unless
   `sha256sum SHA256SUMS` is `DIGEST`, the other two match `SHA256SUMS`,
   and the build-provenance attestation of each of the three verifies
   against this repository, its release workflow, the tag and the release's
   commit (`gh attestation verify` on the bundle that comes with the
   release, so the host needs no GitHub login; it reaches GitHub and the
   Sigstore and GitHub TUF repositories for their trust roots). Only then
   does it unpack the tar, which must hold plain files alone, each listed
   in and matching its own `SHA256SUMS`, with the release's `VERSION`. It
   puts the host's files in `/var/tmp/hangar/release/TAG/`, with the
   release's `SHA256SUMS` as `release.SHA256SUMS` and `attestation.jsonl`
   beside them, and `deploy/` in
   `/var/tmp/hangar/deploy-TAG/`, each under a temporary name first and
   renamed into place, so neither name ever holds half a release. A refused
   release leaves nothing under them, nor changes a fetch of the same tag
   made before. No flag skips a check. The attestations say this
   repository's workflow built the files for that tag; the digest says they
   are the very release you read the notes of. To check a release from a
   workstation as well, see `docs/runbook/release.md`.
3. On the host, one command per line:

   ```sh
   sudo /var/tmp/hangar/deploy-TAG/host/bootstrap.sh --release /var/tmp/hangar/release/TAG --dry-run
   sudo systemd-run --unit=hangar-bootstrap --wait --collect /var/tmp/hangar/deploy-TAG/host/bootstrap.sh --release /var/tmp/hangar/release/TAG
   sudo journalctl -u hangar-bootstrap --no-pager
   sudo /var/tmp/hangar/deploy-TAG/host/verify.sh
   sudo /var/tmp/hangar/deploy-TAG/host/bootstrap.sh
   cat /usr/local/lib/hangar/VERSION
   systemctl list-units --no-pager 'hangar-*'
   ```

   The dry run lists `would: install` for each changed file and `would:
   restart` for each service to restart, and nothing else for an upgrade
   alone. The real run ends with `changes: N`. `verify.sh` must pass, the
   run after it must print `changes: 0`, `VERSION` must be the release's
   commit (the notes name it), and the VMs' units must still be there.

A release whose files do not match its `SHA256SUMS` is refused by the fetch,
and again by the release step, before anything is installed.

### The first fetch on a new host

`hangar-fetch-release` comes with a release, and the packages step installs
the `gh` it verifies with, so a host that has neither (a new host, or one
last deployed by copying a release to it) fetches its first release by
hand, once: the pinned `gh`, then the host tar, checked the way the script
checks it, then the script from its `deploy.tar`. With `TAG` and `DIGEST`
as above, and the `HANGAR_GH_*` values read from
`deploy/host/versions.env` at that tag on GitHub:

```sh
sudo -i
TAG=vX.Y.Z DIGEST=<digest>
GH_VERSION=<HANGAR_GH_VERSION> GH_SHA256=<HANGAR_GH_SHA256> GH_BIN_SHA256=<HANGAR_GH_BIN_SHA256>
cd "$(mktemp -d)"
curl -fsSL -o gh.tar.gz "https://github.com/cli/cli/releases/download/v$GH_VERSION/gh_${GH_VERSION}_linux_amd64.tar.gz"
echo "$GH_SHA256  gh.tar.gz" | sha256sum -c
tar -xzf gh.tar.gz --strip-components=2 "gh_${GH_VERSION}_linux_amd64/bin/gh"
echo "$GH_BIN_SHA256  gh" | sha256sum -c
install -m 0755 gh /usr/local/bin/gh
base=https://github.com/YukinaMochizuki/hangar/releases/download/$TAG
host_tar=hangar-host_${TAG}_linux_amd64.tar.gz
for f in SHA256SUMS VERSION "$host_tar" attestation.jsonl; do curl -fsSLO "$base/$f"; done
sha256sum SHA256SUMS                       # must print DIGEST
sha256sum -c --ignore-missing SHA256SUMS   # VERSION: OK, and the host tar: OK
for f in SHA256SUMS VERSION "$host_tar"; do
  gh attestation verify "$f" --bundle attestation.jsonl --repo YukinaMochizuki/hangar \
    --signer-workflow YukinaMochizuki/hangar/.github/workflows/release.yml \
    --source-ref "refs/tags/$TAG" --source-digest "$(cat VERSION)" --deny-self-hosted-runners
done
tar -xzf "$host_tar" deploy.tar
tar -xf deploy.tar host/fetch-release.sh
host/fetch-release.sh "$TAG" "$DIGEST"
```

A release from before the host tar (`v0.1.0-rc.4` and earlier) has its
files loose, `VERSION` and `deploy.tar` among them, and only its own
`hangar-fetch-release` reads that layout; an installed
`hangar-fetch-release` reads only the layout of the release that installed
it. So crossing that change, going back to such a tag or forward from it,
fetches by hand like this, with the target tag's own script. For an older
tag, download `SHA256SUMS`, `VERSION`, `deploy.tar` and
`attestation.jsonl`, check `SHA256SUMS` against `DIGEST` and the other
files against it, verify the attestations of `SHA256SUMS`, `VERSION` and
`deploy.tar` as above, and take `host/fetch-release.sh` from `deploy.tar`.

Stop at the first command that fails or prints something else. The last
line is the whole fetch of step 2; carry on from step 3. The release step
then installs the script as `/usr/local/sbin/hangar-fetch-release`, and the
packages step finds the `gh` it pins already there.

### Going back to an earlier tag

Going back is the same sequence with the earlier tag and the digest its
release notes state: `sudo hangar-fetch-release OLD_TAG OLD_DIGEST`, which
checks it again (and replaces what an earlier fetch of it left), then step 3
with `deploy-OLD_TAG` and `release/OLD_TAG`. The run is the earlier
`deploy/`'s, so it also puts back that release's `hangar-fetch-release`.
The dry run lists the files and services that go back. Read the notes below
for releases that cannot be gone back past while VMs run.

The first deploy with the admin socket rewrites the apiserver's unit (the
sockets move to socket units, `/run/hangar` becomes root's), so it
restarts the apiserver once even in dev auth; the agent
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

## Quotas

The limits every tenant has unless it names a plan are the `default` Plan
(`docs/decisions/tenancy-and-iam.md`), a trial's. The apiserver makes it
once, on a new store, with 8 cpu, 16Gi memory, 100Gi disk, 10 machines, 5
projects, 5 networks, 5 addresses, 10 public endpoints, 10 service
tokens and 3 file shares. From then on the plan is data: the apiserver
never changes it again, so a store made before these limits keeps the ones
it was made with, and the operator sets them, as platform root, with apply; a field left
out has no limit:

```yaml
apiVersion: tenancy.hangarlab.cloud/v1alpha1
kind: Plan
metadata:
  name: default
spec:
  displayName: Default
  limits:
    cpu: 8
    memory: 16Gi
    disk: 100Gi
    machines: 10
    projects: 5
    networks: 5
    addresses: 5
    publicEndpoints: 10
    serviceTokens: 10
    fileShares: 3
```

`sudo hangar --context local apply -f default-plan.yaml` on the host, or
the same from a workstation as platform root; `hangar get plan default -o
yaml` shows what holds. Lowering a limit takes nothing away: a tenant past
it keeps what it has, and only what would grow it is refused. A paying
user's tenants get a bigger plan: apply another Plan, then name it in
their Invitation (`hangar admin invite EMAIL --plan PLAN`) or, once they
have signed up, in their User's `spec.plan`, which every tenant they open
from then on gets; set an open tenant's `spec.plan` to move it, and
`plan: default` to move it back (leaving `plan` out of an apply keeps the
plan it has).
`hangar get tenant NAME -o yaml` shows a tenant's plan, its limits and its
usage in `status`.

How many tenants one person or service token may own is
`HANGAR_DEFAULT_MAX_OWNED_TENANTS` in the host env file, which the
`services` step passes as `--default-max-owned-tenants`; changing it
restarts the apiserver. The tenant signing up opens counts. For one
person, pass `--max-tenants` when inviting them, or set
`spec.maxOwnedTenants` on their User once they have signed up (`hangar get
user NAME -o yaml`, edit, apply).

## The platform's SSH CA

`hangar ssh` logs in with a key the apiserver signs for minutes, with the
platform's one SSH CA (`docs/decisions/vm-ssh.md`). Its private key is a
file the apiserver alone reads, `/var/lib/hangar/state/ssh-ca`, kept in
the vault and installed by hand, as the OAuth client's secret is;
nothing in `deploy/host` makes one. Make it once, on a workstation, and put
the private key in the vault:

```sh
ssh-keygen -t ed25519 -N '' -C hangar-ssh-ca -f hangar-ssh-ca
```

Install it from the vault, the apiserver's, mode 0600, and restart the
apiserver, which reads it at start and logs its public key:

```sh
sudo install -m 0600 -o hangar -g hangar hangar-ssh-ca /var/lib/hangar/state/ssh-ca
sudo systemctl restart hangar-apiserver
shred -u hangar-ssh-ca
```

The `services` step passes `--ssh-ca-key` to the apiserver and gives the
file back to `hangar`, mode 0600, if it finds it otherwise, saying who it
was open to: if anyone else could read it, take the key as exposed and make
a new one; `verify.sh`
checks it, and skips it while there is none. Without it the apiserver runs,
signing fails saying the CA is not installed, and VMs' static keys still
work. A guest takes the CA on its first boot only: VMs made while there
was none, or under another CA, trust no CA, or the old one. A new key
therefore holds for VMs made after it; the old one stays in the vault until
no VM trusts it.

## The secrets key-encryption key

Secrets are stored sealed under a key-encryption key (KEK), a file the
apiserver alone reads, `/var/lib/hangar/state/secrets-kek`: one line, an id
and 32 random bytes in base64 (`docs/decisions/secrets.md`). Unlike the SSH
CA, the `services` step makes it when there is none, on the first run, and
says so:

```
note: copy /var/lib/hangar/state/secrets-kek to the vault now: ...
```

Do that at once. The database alone holds no usable Secret: a restore of it
without this file, on this host or another, is a store whose Secrets nobody
can open, and the apiserver refuses to start over them. Restoring a host
means putting the KEK back from the vault before the apiserver starts:

```sh
sudo install -m 0600 -o hangar -g hangar secrets-kek /var/lib/hangar/state/secrets-kek
```

The step never replaces a KEK that is there, and gives one it finds open to
others back to `hangar`, mode 0600, saying who it was open to; take that as
the key exposed, and rotate it. `verify.sh` checks it. The apiserver logs
`loaded the key-encryption key` with the key's id at start, never the key.

**Rotating it.** The apiserver opens Secrets with the previous key and seals
them again under the new one as it starts, so a rotation is one restart:

1. Move the key aside, keeping its owner and mode:
   `sudo mv /var/lib/hangar/state/secrets-kek /var/lib/hangar/state/secrets-kek.previous`
2. Run `deploy/host/bootstrap.sh`, which makes a new key as it would on a
   new host (no restart yet: the unit is unchanged). Copy the new key to
   the vault.
3. `sudo systemctl restart hangar-apiserver`. The unit always passes
   `--secrets-kek-previous /var/lib/hangar/state/secrets-kek.previous`,
   skipped when the file is not there. The log says
   `sealed secrets again under the current key-encryption key` with how
   many, and the new id.
4. `sudo shred -u /var/lib/hangar/state/secrets-kek.previous`. Keep the old
   key in the vault for as long as you keep backups of the database made
   before the rotation.

A wrong key, a missing previous key during a rotation, or a previous key
without a current one stops the apiserver at start, naming the secret it
could not open; put the right files back and start it again. Nothing is
written until every secret opens.

## Switching to sign-in through Google

The public API moves from dev auth to oidc auth, where the apiserver signs
people in through Google and issues its own sessions
(`docs/decisions/authentication.md`), before the front door exists, so it
never reaches a header-trusting apiserver; local administration goes
through the admin socket throughout. Everything in this section stays on
the host only.

1. In the Google Cloud console, make an OAuth client of type "Web
   application" with the authorized redirect URI
   `<public URL>/auth/callback`, such as
   `https://console.hangarlab.cloud/auth/callback`. The CLI and the
   console share it; the device login needs no client of its own.
2. Install its secret as `/var/lib/hangar/state/oidc-client-secret`,
   `hangar`'s, mode 0600, without it passing through a terminal or the
   shell history. From a box that holds it in its environment (the
   apiserver reads the file trimmed, so `printenv`'s newline does no
   harm):

   ```sh
   printenv HANGAR_OIDC_CLIENT_SECRET | ssh <host> 'sudo install -o hangar -g hangar -m 0600 /dev/stdin /var/lib/hangar/state/oidc-client-secret'
   ```

   or from a file copied to the host:

   ```sh
   sudo install -o hangar -g hangar -m 0600 /dev/null /var/lib/hangar/state/oidc-client-secret
   sudo sh -c 'cat client-secret.txt >/var/lib/hangar/state/oidc-client-secret' && shred -u client-secret.txt
   ```

3. Add the client's settings to `/etc/hangar/local.env`, next to
   `HANGAR_BOOTSTRAP_ADMIN`:

   ```sh
   HANGAR_AUTH_MODE=oidc
   HANGAR_OIDC_CLIENT_ID=<the client ID, such as 123-abc.apps.googleusercontent.com>
   HANGAR_PUBLIC_URL=https://console.hangarlab.cloud
   ```

   `HANGAR_OIDC_ISSUER` names another provider; Google is the default.
   Then `sudo deploy/host/bootstrap.sh --only services`. It refuses, before
   changing anything, when the mode is neither `dev` nor `oidc`, a value is
   missing, or the secret file is; a secret file anyone else could read is
   given back to `hangar`, mode 0600, with a warning to roll the secret.
   Otherwise the unit gets `--auth-mode oidc --oidc-client-id …
   --oidc-client-secret-file … --public-url …` and the apiserver restarts,
   still on `127.0.0.1:8080`. From here on a dev header on the public
   listener is refused; use the admin socket. The apiserver reaches Google
   only when someone signs in.
4. Verify: `sudo deploy/host/verify.sh` passes, with `PASS OAuth client
   secret …` and `PASS apiserver runs oidc auth …`. People sign in once the
   front door serves the console ("The front door" above): from a
   workstation, `hangar login` (or `hangar login --device` without a
   browser) then `hangar whoami` against `https://api.hangarlab.cloud`. A
   full `bootstrap.sh` then prints `changes: 0`.

A new client secret is step 2, then `sudo systemctl restart hangar-apiserver`.

To end every login of a person, for a lost laptop or a stolen token: they
run `hangar logout --all`, or root on the host revokes them through the
admin socket:

```sh
sudo curl -s --unix-socket /run/hangar/admin.sock -H 'Content-Type: application/json' \
  -d '{"email":"bob@example.com"}' http://local/hangar.iam.v1alpha1.SessionService/RevokeSessions
```

It answers how many credentials it revoked. Deleting their platform
binding also stops them at once, since every call checks membership, and
their next refresh ends the login.

To roll back to dev auth, turn the front door off first ("The front door"
above), then in `local.env` set `HANGAR_AUTH_MODE=dev` and remove the
`HANGAR_OIDC_*` and `HANGAR_PUBLIC_URL` lines (removing the mode's line
too is refused: nothing defaults it),
then `sudo deploy/host/bootstrap.sh`: the apiserver restarts in dev auth,
on loopback alone. The `services` and `edge` steps refuse public endpoints
in dev auth. Dev auth also refuses any request carrying a header
Cloudflare adds: `Cf-Warp-Tag-Id`, which cloudflared sets on every request
it proxies, and `Cf-Connecting-Ip`, `Cdn-Loop` and `Cf-Ray` from its edge.

A host whose `local.env` still says `HANGAR_AUTH_MODE=cloudflare` is
refused before any step: that mode is gone, and the cutover in
`docs/runbook/cloudflare.md` is the way on.

## The smoke token

Smoke tests act as the service token `smoke` in the `smoke` project of the
`smoke` tenant, bound `project.editor` there and nothing else
(`docs/decisions/authentication.md`). Its secret lives on the host only,
in `/etc/hangar/smoke-token`, root's, mode 0600. The Cloudflare Access
token `hangar-smoke` (`docs/runbook/cloudflare.md`) no longer
authenticates anything.

Make it once, as root through the admin socket, which needs no login.
The secret goes straight into the file and is never printed:

```sh
sudo hangar --context local get project smoke    # the tenant and project exist on prod already
sudo hangar --context local token create smoke --project smoke --role project.editor \
  --description "smoke tests" --secret-file /etc/hangar/smoke-token
```

Here `local` is a context with endpoint
`unix:///run/hangar/admin.sock`. On a host without them, apply the `smoke`
tenant and project first. On a host that had the Cloudflare-era smoke
binding (a RoleBinding whose subject is the `hangar-smoke` Access token's
client ID), delete it: it names no token and grants nothing.

`sudo deploy/host/verify.sh` then reports `PASS smoke token
/etc/hangar/smoke-token, root's, mode 600, is serviceToken:smoke/smoke and
lists the smoke project's containers`, and `SKIP smoke token` while the
file is missing. The check signs in with the token and lists containers in
`smoke`, so a token that authenticates but lost its binding fails it.
A smoke run reads it as `HANGAR_TOKEN=$(sudo cat /etc/hangar/smoke-token)`,
or through a context with `--auth token --token-file
/etc/hangar/smoke-token`.

The secret lasts a year. Before it ends, or whenever it may have leaked:

```sh
sudo hangar --context local token rotate smoke --project smoke --secret-file /etc/hangar/smoke-token.new &&
  sudo mv /etc/hangar/smoke-token.new /etc/hangar/smoke-token
```

The old secret stops working at once. `hangar token get --project smoke`
shows when the current one expires and when it was last used.

## Monitoring and alerts

The `monitoring` step runs five services, each as a system user of its
own name, sandboxed, listening on loopback alone, so the firewall stays at
22, 80 and 443 (`docs/decisions/monitoring.md`):

| Service | Listens on | Does |
|---|---|---|
| `hangar-prometheus` | `127.0.0.1:9090` | Scrapes the others every 30 s, evaluates the rules, keeps 30 days (at most 16 GB) on `hangar/prometheus` (`/var/lib/hangar/prometheus`, quota 20G, its user's, 0700) |
| `hangar-alertmanager` | `127.0.0.1:9093` | Sends alerts to Telegram; no cluster port. What it has sent and its silences are in `/var/lib/hangar-alertmanager` |
| `hangar-node-exporter` | `127.0.0.1:9100` | The host: CPU, memory and pressure, filesystems, ZFS (the pool's state, the ARC), md, hwmon, time sync, and the state and restart count of the platform's units and the monitoring stack's alone, never a VM's |
| `hangar-smartctl-exporter` | `127.0.0.1:9633` | Each NVMe controller's SMART: health, media errors, wear, temperature. The devices are named in its unit, so a disk added later takes a run of the step |
| `hangar-blackbox-exporter` | `127.0.0.1:9115` | With the front door on, `https://api.<cp-domain>/healthz` (must say `ok`) and `https://console.<cp-domain>/` over HTTPS, as a client resolves them, and the days left on their certificates |

`hangar-zpool-metrics.timer` runs `/usr/local/libexec/hangar-zpool-metrics`
each minute, as root, which writes each pool's size, allocation and error
counts for node_exporter (`/var/lib/hangar-node-exporter/textfile`). Every
binary is pinned in `deploy/host/versions.env` with its sha256, as Cloud
Hypervisor is, and installed in `/usr/local/bin` with `promtool` and
`amtool`; the configuration is written to `/etc/hangar/monitoring/`. A
changed unit, configuration or binary restarts that service, and nothing
else. Before writing the configuration the step has the installed
`promtool` and `amtool` check it, and refuses what they refuse.

Prometheus also scrapes the platform's own services, each on the loopback
address the `services` and `edge` steps give it with `--metrics-listen`,
as a job of its name, and only those whose binary takes the flag (a
release from before it is warned of and not scraped):

| Job | Target | What it serves |
|---|---|---|
| `hangar-apiserver` | `127.0.0.1:9101` | Connect calls by procedure and code, and their latency; each Node's `Ready`; machines by phase and reason, and in a crash loop, counted, never named |
| `hangar-node` | `127.0.0.1:9102` | The tunnel to the apiserver, failed reports, image pulls, the image cache |
| `hangar-edge` | `127.0.0.1:9103` | With the front door on: its requests by route (control plane, console, endpoints) and status class, what it answered itself, its certificates' expiry |

Their metrics are scraped as they are served: no relabeling adds a label,
and none of theirs names a tenant (`docs/decisions/monitoring.md`).
`verify.sh` checks each of these targets is up, as it checks every
target.

The rules are `deploy/host/monitoring/rules/host.yml`, the host's, and
`services.yml`, the services', with their tests in
`deploy/host/monitoring/tests/`; `make test-monitoring` (part of `make
check` and CI) runs `promtool check rules` and `promtool test rules` on
them and checks the configurations the step writes, with the same
versions. The step installs both, and removes any other file in
`/etc/hangar/monitoring/rules`. Each alert is `critical` or `warning`:

- critical, sent at once (one message an alert name, again every 4 hours
  while it fires, and when it resolves): a pool not ONLINE or with
  permanent data errors, an md array degraded, a disk failing SMART or
  with media errors, a filesystem or a pool past 90%, `hangar/images` past
  90% of its quota, memory short for 10 minutes, a watched unit not active
  for 2 minutes, a monitoring target down, an HTTPS probe failing for 5
  minutes, a certificate with under 7 days left; more than 5% of the
  API's calls ending in a server error (`unknown`, `internal`,
  `unavailable`, `data_loss`) for 10 minutes, a Node not `Ready` for 3
  minutes, the agent's tunnel to the apiserver down for 3 minutes, a
  machine blocked by `DiskFailed` or `NetworkFailed`, reasons only the
  platform causes, for 15 minutes;
- warning, held to one window a day, 09:00 to 10:00 in
  `HANGAR_ALERTS_TZ` (`HANGAR_UPGRADES_TZ`, else UTC, when unset), all in
  one message, never their resolution: a pool or filesystem past 80%, read,
  write or checksum errors on a pool's devices, SMART wear past 80%, a disk
  or sensor too hot, the clock not synchronised, a unit restarted more than
  3 times in an hour, the pools' metrics stale, a certificate with under 14
  days left; the API's p99 latency past 1 s for 15 minutes, more than 5%
  of the front door's control-plane or console requests ending in 5xx for
  15 minutes.

What a tenant's program does alerts no one: a machine `Exited`,
`BootFailed` or failing its image pull, an endpoint's 5xx, and machines
in a crash loop, which `hangar:machines_crash_looping:sum` records. An
alert that counts machines never names them: its message says to list
them on the admin socket, `sudo hangar --context local get machines -A`
(`-o yaml` for each one's reason); the Node alerts say `sudo hangar
--context local get nodes`.

`Watchdog` always fires and goes to no one, until a dead man's switch
outside the host is added as its receiver.

**The Telegram bot.** Alerts go to one chat: its id is
`HANGAR_ALERTS_TELEGRAM_CHAT_ID` in the host env file (a number; a
group's is negative), and the bot's token is
`/etc/hangar/alertmanager-telegram-token`, root's and
`hangar-alertmanager`'s group's, mode 0640, which Alertmanager reads as it
sends. The token is kept in the vault; nothing in `deploy/host` makes one.
After the step has run once (it makes the group), install it from a box
that holds it in its environment, without it passing through a terminal
or the shell history:

```sh
printenv HANGAR_TELEGRAM_BOT_TOKEN | ssh <host> 'sudo install -o root -g hangar-alertmanager -m 0640 /dev/stdin /etc/hangar/alertmanager-telegram-token && sudo systemctl restart hangar-alertmanager'
```

While the host env file names no chat, or the token is missing, the step
warns that alerts reach no one, and `verify.sh` prints a SKIP line, or
fails the token. A token file open to others is given back to root and
the group, 0640. A new token is the same command.

**Looking at it.** Nothing of it is reachable from outside: forward the
ports over SSH, then open `http://localhost:9090` (Prometheus: queries,
targets, rules, alerts; an alert's link in Telegram opens there) or
`http://localhost:9093` (Alertmanager: what is firing, silences):

```sh
ssh -N -L 9090:127.0.0.1:9090 -L 9093:127.0.0.1:9093 <host>
```

On the host, `amtool --alertmanager.url=http://127.0.0.1:9093 alert query`
lists what fires, and `amtool --alertmanager.url=http://127.0.0.1:9093
silence add alertname=<name> --duration=2h --comment=<why>` silences one
while it is dealt with.

**A test alert.** To check the path to Telegram, send a critical alert by
hand; it arrives within a minute, and resolves after 5:

```sh
amtool --alertmanager.url=http://127.0.0.1:9093 alert add alertname=HangarTestAlert severity=critical \
  --annotation="summary=\"A test alert from $(hostname)\""
```

With `severity=warning` instead it waits for the day's window. `verify.sh`
checks the path from a rule to Alertmanager (Alertmanager has `Watchdog`),
not Telegram itself.

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

- `e2fsprogs` (for `mkfs.ext4 -d`), `zfsutils-linux` and `qemu-utils`
  installed. `qemu-img` writes disk images, such as distributions' cloud
  images, onto their bases (`docs/decisions/vm-images.md`); `verify.sh`
  checks it is `HANGAR_QEMU_IMG_MIN` (`deploy/host/versions.env`) or newer.
  The disk images are downloaded to `disk-images` under the image dataset,
  whose quota bounds them, and removed once their base is made.
- The pool `hangar`, as the installer and the `zfs` step leave it.
- `hangar/images`, mounted at `/var/lib/hangar/images` with
  `setuid=off exec=off devices=off`, the mount point `0700` and root's, and a
  `quota`, so pulling and unpacking never take the room root disks need: a
  few times the largest image the host should take, such as 100G on the
  current host. The image cache and the directory images are unpacked in
  both go under it, on that one dataset: both hold tenant content. The
  `services` step passes the sum of this quota, `hangar/vms`'s and the
  agent's `--image-cache-high` (20Gi, which it sets too) to
  `hangar-node --reserved-disk`: the bases of container images no machine
  uses, kept for their next use (`docs/decisions/node-agent.md`), sit on
  the pool under `hangar/bases`, and are bounded by it. The bases of
  images machines use, and kept disk-image bases (3 to 10 GiB each,
  `docs/runbook/vm-images.md`, one per Image used on the host, under
  `hangar/vmbases`), sit on the pool outside
  that reservation, so the pool's free space must leave room for them.
- Nothing else: the driver makes `hangar/bases`, `hangar/roots`,
  `hangar/disks` and `hangar/vmbases` (`mountpoint=none`) under the pool
  when they are missing, and owns them.
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
- For VirtualMachines, the firmware they boot their own kernel through
  (`docs/decisions/vm-boot.md`): Cloud Hypervisor's EDK2 build at
  `/usr/local/lib/hangar/CLOUDHV.fd`, `0644`, which the `packages` step
  installs at the release and sha256 pinned in `deploy/host/versions.env`
  and `verify.sh` checks; `--vm-firmware` names another. Without the file
  the agent starts, warns, and refuses VirtualMachines.
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

## Finding the machine behind an abuse report

An abuse report about the host's IPv4 address names a source port and a
time. Every machine leaves as that address, so only the NAT log tells which
one it was (`docs/decisions/networking.md`, "Abuse protection"). As root:

```sh
sudo hangar-node natlog --port 40123 --at 2026-10-17T09:30:00Z
```

It lists every connection from that public port that started in the 24 hours
before `--at` (`--before`) up to 5 minutes after it (`--after`, for a
reporter's clock running behind), with its project, network, machine (name
and uid) and the machine's own address and port. The one marked `*` is the
last to start by `--at`: the one the report names, unless it had ended by
then, which the log does not record. A `warning` line names a window in
which connections may be missing from the log (the agent was stopped, or it
fell behind, or the log could not be written), or says the window starts
before the oldest day the log holds; a report there cannot be answered from
the log alone. A `note` line shows a `retention` line in the window: the log
removed days early, as it filled its dataset. `--ip` narrows to one public
address, `--json` prints the records as the log holds them. A machine
deleted since keeps the name and uid it had in the log.

The log is `/var/log/hangar/nat` (`hangar-node --nat-log-dir`), the
`hangar/natlog` dataset (above), root's alone: a file a UTC hour,
`nat-YYYY-MM-DD-HH.jsonl`, compressed (`.jsonl.gz`) once the hour is over;
`zcat` reads them. The agent removes a day 30 days after it ended, or
sooner, oldest first and never today, when the dataset passes 90% of its 20
GiB refquota; a `retention` line in the log then says how many days it holds
(`zgrep '"retention"'`). If the space of removed days does not come back
within a minute, the agent says so once in its own log and counts it as free
until it does, so the dataset may stay past the cap meanwhile: see what
holds the dataset (`zfs list -o space hangar/natlog`, `lsof +L1`). A line is
about 400 bytes per new IPv4 connection out of a network, about 32
compressed; the rate limit keeps one network to 100 a second at most. It
needs conntrack events, which the kernel turns on for any listener by
default (`net.netfilter.nf_conntrack_events` 1 or 2); with them off the
agent logs nothing but gaps, and says so in its own log, as it does once
when writing the log fails and once when it works again.

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

`test/testbed/fetch-release.sh` runs `hangar-fetch-release` against a
stand-in for GitHub on loopback (`docs/runbook/testbed.md` says what it
checks); run it as root on a throwaway VM.

`test/testbed/host-bootstrap.sh` runs the whole flow on a throwaway Ubuntu VM
made to look like a fresh OVH install (loop-device swap, file-backed pool, an
upstream router in a network namespace): dry run changes nothing, first run
converges and counts its changes correctly, second run changes nothing, verify
passes, verify catches drift and a re-run repairs it, a pending rollback blocks
a run, unknown installer netplan keys are refused, a failing generator writes
nothing, and a dead IPv6 config is rolled back. Its settings are in
`/etc/hangar/host.env`, which the scripts read by default: verify fails it
at mode 644, a run refuses without it, `--env FILE` reads another, a file
another user can write or own is refused before it is read, an unknown
`HANGAR_UPGRADES_TZ` is refused, the upgrade timers are in the one it sets,
and `--host` is refused. sshd lets in the user who ran it with sudo
(`ubuntu` without sudo), with a key in `~/.ssh/authorized_keys`: log in
to the VM that way, or keep a console. For sshd it checks: the settings
in `sshd -T`; a drop-in sorting before ours and a `Match User` block each
fail verify; a config `sshd -t` refuses with the new drop-in is refused
with nothing written; a failing `sshd -t` once the file is in place puts
the old file back with no reload; a hand-changed drop-in fails verify and
is written back and sshd reloaded, and so is one backed out as this
runbook says; a broken one of ours is replaced; a file sshd was not
reloaded with is reloaded; and the run refuses `root` in the list or
`HANGAR_SSH_DROP_USERS` in the host env file, a user whose keys are all
in `authorized_keys2`, and, with no `SUDO_USER` as under `systemd-run`,
a list leaving out a user who can log in today, which its dry run says a
real run refuses, `HANGAR_SSH_DROP_USERS` naming that user overrides,
naming them, and naming someone else does not. The env file lists every
user with a key in `~/.ssh/authorized_keys` on the VM, as the step
refuses to leave one out. It needs a release
(`HANGAR_TESTBED_RELEASE=DIR`, the `host/` of what `make release` writes), installs the services
from it, and checks: the node is Ready; the release step installs
`hangar-fetch-release` and verify catches a changed one; that release,
fetched with it from a stand-in for GitHub (a stand-in `gh` for the fetch
alone), its files 0644, has the placement checked with its own
`hangar-node` by the fetched `deploy/`, a run changing nothing, and an
invalid `HANGAR_CHASSIS` refused with nothing changed; and the packages
step the pinned `gh`; a tampered release, or one whose
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
For the admin socket and auth it checks: `/run/hangar` and both
sockets have their owners and modes; root on the admin socket gets and
binds without a binding of its own and is logged as `local:root@testbed`;
other users are kept out by the socket's mode and, with the mode opened,
by the apiserver's uid check; dev auth refuses Cloudflare's edge headers;
the agent reconnects when the apiserver and its sockets restart, and a
socket restarted alone is reported and repaired; no step installs
cloudflared, and verify finds nothing of it; the agent is given the host's
public addresses; half an oidc mode, and the retired cloudflare mode, are
refused before any step; the switch to oidc auth restarts the apiserver
with the flags and the client secret, still on loopback. For the tunnel an
earlier release left: while its `cloudflared.env` is there it runs on, a
run changes nothing and says so, and verify skips it; dev auth asked for
stops it before the apiserver restarts; with the env file gone a run
retires it, nothing of it is left, and a second run changes nothing; put
back by hand without the env file, it fails verify and the next run
removes it; then back to dev auth.
The front door (`HANGAR_TESTBED_FRONT_DOOR=DIR`, from
`test/testbed/front-door-kit.sh DIR`: a `hangar-edge` built with the
`testbed` tag, and pebble): with pebble as the CA and the edge answering
DNS-01 itself, the edge runs as `hangar-edge` with no capability on the
:443 and :80 systemd binds, and a second run changes nothing; the API, the
console and the endpoints answer with pebble's certificates on IPv4 and
IPv6, from the host and through the uplink, and :80 redirects; verify
checks every certificate in storage and what :443 serves for each name
(with pebble's root among the system's, and failing each without it);
from outside, 22 is open and a listener on another port is not, on either
address; a restarted edge serves its certificates from its storage without
asking the CA; public endpoints without the front door are refused, with
nothing changed; and turning the front door off stops the edge, frees :443
and :80 and removes the public socket.
It does not reboot. Run it as root on a disposable VM only. `make lint-sh` runs
shellcheck over every script.

`HANGAR_TESTBED_FLOW_INSTALLED=1` runs the L4 flow (`docs/runbook/testbed.md`)
against the services the steps installed instead of processes of its own.

The `monitoring` step is tried on a `hangar-testbed` VM with the services
installed as `docs/runbook/testbed.md` says, then `--only monitoring`. An
env file with `HANGAR_ALERTS_TELEGRAM_API_URL=http://127.0.0.1:PORT` points
Alertmanager at a stand-in for the Telegram API on loopback, which answers
`{"ok": true, "result": {...}}` and logs what it is sent, with any token in
the token file: stopping `hangar-node` must bring a critical message within
about three minutes, and a warning (`amtool alert add`) none until
`HANGAR_ALERTS_TZ` puts the window around the present and the step runs
again. A VM has no NVMe disk, so `smartctl_exporter`'s reading of one is
not tried there.
