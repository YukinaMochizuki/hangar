# Host bootstrap

`deploy/host/` brings a freshly installed host to the state hangar expects:
packages and a pinned Cloud Hypervisor, swap mirrored on md RAID1, the `hangar`
pool's properties and base datasets, netplan owned by us with IPv6 off the
prefix's `::` address, proxy NDP, forwarding, and vsock. It is safe to re-run.

## Before the first run

The OS install itself is done through the OVH API with a custom partition
layout: `/boot` 1 GiB, `/` 100 GiB and swap 8 GiB on software RAID1, and the
rest as a ZFS partition with `raidLevel: 1` and pool name `hangar`, which the
installer turns into a mirrored pool. The installer does not mirror swap and
gives the host its prefix's `::` address; the bootstrap fixes both.

Add `deploy/host/hosts/<hostname>.env` for the host. Every value in it is
explicit (uplink name, IPv6 address and gateway, the two swap partitions); the
scripts never guess a disk or an interface.

## Run

Copy `deploy/` to the host, then as root:

```sh
deploy/host/bootstrap.sh --dry-run   # shows diffs and "would:" lines, changes nothing
systemd-run --unit=hangar-bootstrap --wait --collect deploy/host/bootstrap.sh
journalctl -u hangar-bootstrap       # its output; ends with "changes: N"
deploy/host/verify.sh                # one PASS/FAIL per check, exit 1 on any FAIL
```

Run the real thing detached from the SSH session, as above. `netplan apply` and
`networkctl reconfigure` renew the uplink's DHCPv4 lease, which can drop the
session; a run killed with it is safe to re-run, but it should not be killed.

A second `bootstrap.sh` must print `changes: 0`. `--only <step>` runs a single
step (`packages`, `swap-raid`, `zfs`, `network`, `sysctl`).

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
these before `hangar-node` starts; `deploy/host` does not make them:

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
nothing, and a dead IPv6 config is rolled back. It does not reboot. Run it as root on a disposable VM
only. `make lint-sh` runs shellcheck over every script.
