# Testbed: real-host tests (L4)

The L4 tests (`docs/decisions/testing-strategy.md`) run what no workspace can:
Cloud Hypervisor, KVM, taps, guests. They live under `test/testbed/`, behind
the `testbed` build tag so `make test` and `make check` never build them, and
`make e2e-host` runs them. Run them only on a disposable VM: they run as root,
create taps and an nftables table, and turn on IPv4 forwarding.

| Path | What it tests |
|---|---|
| `test/testbed/guest/` | The platform kernel, initramfs and hangar-init under Cloud Hypervisor, driven as the node drives them: the boot disk (`internal/guest/bootdisk`), the framed console (`internal/guest/console`) and the guest agent over hybrid vsock (`internal/node/driver/chguest`). No node or driver code |
| `test/testbed/disks/` | The image and storage drivers (`internal/node/driver/registry`, `zfsstore`) on the `hangar` pool: the conformance suite (`drivertest`), volumes included, with decoy datasets about, Docker Hub pulls by tag and digest compared with the reference flattening, and a guest booting from a root disk larger than its image, its filesystem grown by hangar-init |
| `test/testbed/vms/` | The real hypervisor driver (`internal/node/driver/chvm`) and its launcher, `hangar-node vmm`, under real systemd and Cloud Hypervisor, with every other real driver: the conformance suite (L3) with guests booting from root disks the storage driver made, their logs read and followed across a restart, execs in them (output, stdin, a terminal and its resize, signals, an exec's group killed with its context), graceful and forced stops (`Stop`, a process ignoring SIGTERM, a frozen guest), restart counts, a VM outliving its agent killed outright, root disk links refused by `BLKZNAME`, volumes a guest sees as its next disks (`vdc` on, at their sizes, what one machine wrote read by the next, a volume's link to another zvol refused), the console log under a 40 MiB flood and forged records (one VM's harming no other), a serial log failing under a `chattr +i` directory without freezing the guest, VMs interrupted as by a host shutdown under each policy (their units stopped with `systemctl stop`: the SIGTERM handler's output in the log, a process ignoring SIGTERM killed within the grace and slack, nothing recorded), the driver reconnecting to systemd, nothing left after the sweep with decoy units and files about, `hangar-node --drivers=host` starting, the confinement (`TestCHIsConfined`: a probe, `chprobe`, run in Cloud Hypervisor's place in a real VM's unit, tries the host's files, another VM's directory, socket, zvol and process, devices, privileges and the network; a link it plants at its own socket's path, to the other VM's socket, refused by the driver; the other VM's real Cloud Hypervisor checked from the host; both launchers left with `CAP_DAC_OVERRIDE` and `CAP_KILL` alone once Cloud Hypervisor runs; the unit's `/dev` read-only to root in it; the device policy from a root process in the unit's cgroup; the VM's uid outside any unit; `systemd-analyze security`), and a VM started without VM users running on across an upgrade of the launcher and confined once started again |
| `test/testbed/network/` | The real network driver (`internal/node/driver/hostnet`): the drivers' conformance suite (L3), then stand-in guests on taps through both NATs, isolation, forged sources, egress only by the uplink, drift, failing watches, collisions and cleanup. Needs root, not KVM |
| `test/testbed/flow/` | The whole system, as N5 puts it on a host: the real `hangar-apiserver` (as `nobody`) and `hangar-node --drivers=host` (as root, over the apiserver's node socket) as processes, driven by the real `hangar` CLI. One test, `TestFlow`, in ordered scenarios, each checked from the API and from the host: apply and Ready; online through both NATs; stop and start with `runState`, the disk kept; restart under `on-failure`; growing the disk; changing the image; the agent killed mid-pull and started again; a host shutdown (the agent, then each VM's unit stopped with `systemctl stop`), after which `always` and `on-failure` boot again, their Cloud Hypervisor as the VM user it had, and `never` is Exited with 253; `hangar logs` across a restart, with each option, and `logs -f` killed ending the agent's read of the log; `hangar exec` in a busybox container (the container's environment and overrides, stdout and stderr apart, exit codes, `-i` with piped stdin, `-it` on a pty for its size, a resize and Ctrl-C as a key, the terminal put back, SIGINT passed on without `-t`, a CLI killed leaving nothing running in the guest, the apiserver's audit line); delete |
| `test/testbed/internal/vmtest/` | Not tests: the harness that boots those guests, for any L4 package. A package calls `vmtest.Main` from its `TestMain`, then `vmtest.New`, `Start`, `Ready`, `Exec` |
| `test/testbed/host-bootstrap.sh` | `deploy/host` on a VM made to look like a fresh OVH install (`docs/runbook/host-bootstrap.md`) |
| `test/testbed/setup.sh` | Not tests: makes the pool `hangar` on a sparse file and `hangar/images`, as a host has them, for `test/testbed/disks` |

## Prepare a testbed VM

A bare Ubuntu 24.04 VM with nested KVM (`/dev/kvm`), 8 or more vCPUs, 8 GB
of memory and 20 GB of disk is enough. As a user with sudo:

1. Packages for the kernel build, and nftables:

   ```sh
   sudo apt-get install build-essential flex bison bc libelf-dev libssl-dev curl xz-utils git nftables zfsutils-linux e2fsprogs qemu-utils
   ```

2. Go at the version in `go.mod`, from <https://go.dev/dl/>, on `PATH`.
3. Cloud Hypervisor at the version pinned in `deploy/host/versions.env`,
   checked against its sha256 there, installed as `cloud-hypervisor` on `PATH`.
4. A checkout of the branch under test, then `make guest-kernel` (a few
   minutes; `docs/runbook/guest-kernel.md`).
5. `sudo test/testbed/setup.sh`: `qemu-img` if it is missing, the ZFS
   pool `hangar`, 24 GB on a sparse file in `/var/tmp`, `hangar/images`
   with an 8 GB quota, and the udev rules for zvols the `zfs` step of
   `deploy/host` installs. It is
   idempotent; a VM that ran `test/testbed/host-bootstrap.sh` has the pool
   already.

## Run

```sh
make e2e-host
```

It rebuilds the initramfs, makes the root disk image on first use, builds
`bin/hangar-node` (the VMs' launcher, `HANGAR_TESTBED_NODE` names another),
`bin/hangar-apiserver` and `bin/hangar` (`HANGAR_TESTBED_APISERVER`,
`HANGAR_TESTBED_CLI`), and `bin/ch-probe` (`HANGAR_TESTBED_CHPROBE`), all
static (`CGO_ENABLED=0`): a VM's unit has an empty root with no dynamic
loader, and the hypervisor driver refuses a dynamically linked launcher.
The unit binds the launcher, Cloud Hypervisor, the kernel and the
initramfs from wherever they are, a checkout in a home directory
included. Then it runs `go test -exec 'sudo -E' -tags testbed ./test/testbed/...`: the
tests are built as you and run as root. The root disk image,
`.cache/testbed/dind-<digest>.ext4`, comes from the `docker:dind` image
pinned by digest in the `Makefile` (`test/testbed/guest-rootfs.sh` makes it,
with the pinned `crane`, and hands it to you); a new pin makes a new image,
and old ones can be deleted. `HANGAR_TESTBED_KERNEL`,
`HANGAR_TESTBED_INITRAMFS`, `HANGAR_TESTBED_ROOTFS` and
`HANGAR_TESTBED_CMDLINE` point the guest tests at other artifacts or another
kernel command line.

The guest tests need outbound internet access: Docker in a guest pulls
`hello-world` and `busybox`, pinned by digest in the test. Each guest gets
172.31.N.2/24 behind its own tap, `hgtN`, at 172.31.N.1, NATed out by the
nftables table `hangar_testbed_guest`; its disks and sockets are in
`/var/tmp/hangar-testbed-guests/`. IPv4 forwarding is turned on and left on.

`HANGAR_TESTBED_WORST_CASE=1` also runs `TestWorstCaseWork`, which streams
layers built to cost the most into the image dataset at the default limits
and logs how long and how much CPU each takes to end (minutes);
`docs/decisions/storage-zfs.md` records what it measured.

The disk tests pull `nginx`, `debian` and `busybox` from Docker Hub, by tag
and by pinned digest. What the image driver unpacks is compared, entry by
entry, with go-containerregistry's `mutate.Extract` (what `crane export`
writes), except for device nodes, which the host never makes, and symlinks
climbing out of the root, which Extract drops. Each test works in a
dataset of its own under `hangar/testbed-disks`, with decoy datasets the drivers must leave alone,
and in `/var/lib/hangar/images/testbed-disks`; both are removed as the test
ends, and swept at the start of the next run if a run was killed.

A run leaves nothing else behind. Guests die with the test binary
(`Pdeathsig`), and taps with their guests. The table and the directories
are removed at the end, on SIGINT or SIGTERM, and shortly before `go test`'s
`-timeout` would kill the binary. A run killed outright leaves the table
and the directories, and the next run sweeps them first, with any stale
`cloud-hypervisor` on an `hgt` tap. One test binary at a time per testbed:
`make e2e-host` runs the packages one after another (`-p 1`), and
`vmtest.Main` holds `/run/lock/hangar-testbed.lock`, so a second run waits
for the first.

`make e2e-host-vms` runs the hypervisor tests alone (`RUN=regexp` picks
some). They pull `busybox` from Docker Hub by digest, publish it to a
registry in the test with a layer of each image's own, and run the guests
as transient units `hangar-vm-<machine>.service` from directories under
`/var/tmp/hangar-testbed-vms`, with datasets under `hangar/testbed-vms`
and image files under `/var/lib/hangar/images/testbed-vms`. Their decoys
are the units `hangar-vmdecoy.service`, `hangar-decoy-vm-m1.service` and
`decoy-hangar-vm-m1.service` (each a `sleep`) and
`/var/tmp/hangar-testbed-vms-decoy`. `TestAnUpgradeKeepsVMsRunning` keeps
the launcher it replaces in `/var/tmp/hangar-testbed-vms-upgrade`; it
starts with the launcher under test, or with `HANGAR_TESTBED_OLD_NODE`, a
`hangar-node` built (static) from a release without VM users. All of
these, and every `hangar-vm-*` unit, are swept at the start of a run and
at its end. The VMs run as the ids from 200000 on
(`docs/decisions/node-agent.md`); the driver refuses to start if a user,
group or subordinate range of the testbed has one, as a second user made
with `useradd` would. `HANGAR_TESTBED_MEASURE_MEMORY=1` also runs
`TestMemoryOverhead`, which fills guests of 1, 4 and 16 GiB and logs how
much memory their units take; it needs a testbed of 24 GiB
(`docs/decisions/node-agent.md` records what it measured).
`hangar-testbed-reset` does not know them yet: after a run killed outright,
the next run's sweep, or `systemctl stop 'hangar-vm-*'`, removes them.

`make e2e-host-flow` runs the full flow alone (`RUN=regexp` picks
scenarios, such as `RUN=TestFlow/1-`, though each stands on the ones
before it). It pulls `busybox`, `debian` and `nginx` from Docker Hub by
pinned digest, and its first container fetches
`http://cloudflare.com/cdn-cgi/trace`, so it needs outbound internet access
and DNS. The apiserver listens on a free loopback port in dev auth, its
database, node socket and a copy of its binary in
`/var/tmp/hangar-testbed-flow/api` (`nobody` cannot read a checkout in a home
directory); the agent runs with `--zfs-dataset hangar/testbed-flow`,
`--image-dir /var/lib/hangar/images/testbed-flow` and `--vm-dir
/var/tmp/hangar-testbed-flow/vms`, and the uplink of the default route.
Their logs, `apiserver.log` and `node-N.log` (one per start of the agent),
are in `/var/tmp/hangar-testbed-flow` and are logged when a scenario fails.
The agent and the apiserver die with the test binary (`Pdeathsig`); the VMs,
which outlive the agent by design, do not. The start of a run and its end
sweep every `hangar-vm-*` unit, the network driver's namespaces and links,
the dataset and both directories, and kill an agent or apiserver a run
killed outright left. `hangar-testbed-reset` removes the dataset but not
the units or `/var/tmp/hangar-testbed-flow`; the next run's sweep does.

`HANGAR_TESTBED_FLOW_INSTALLED=1` runs the same flow against the services
`deploy/host` installed instead (`docs/runbook/host-bootstrap.md`), on the
default dataset `hangar` and `/var/lib/hangar/{images,vms}`, with
`HANGAR_TESTBED_FLOW_NODE` the host's `HANGAR_NODE_NAME`, the admin of
`/etc/hangar/local.env`, and `HANGAR_TESTBED_CLI=/usr/local/bin/hangar`.
The agent is killed with `systemctl kill --kill-whom=main` and must come
back by its unit's `Restart=always`; `systemctl restart hangar-node` must
then leave every VM as it was. The run needs a host with no machines, and
deletes, through the API, only the containers it makes, first and last;
image bases stay. On a `hangar-testbed` VM, install the services with
`bootstrap.sh --env FILE --only` for the `packages`, `zfs`, `upgrades`,
`release` and `services` steps (never `network` or `swap-raid`), from an
env file naming the VM's uplink and address.

`make e2e-host-network` runs the network tests alone, which need no guest
artifacts, no KVM and no Cloud Hypervisor: a bare Ubuntu 24.04 VM with Go,
`nftables`, `curl` and `iputils-ping` is enough. Each stand-in guest is a
namespace `tbg-<machine>-<nic>` whose own tap the test joins to the
interface's tap by copying frames, as a VMM does. The tests make an upstream
in namespace `tb-upstream` behind veth `tbup0` (198.51.100.0/24), which they
use as the uplink so they can take it down, and decoys named `tbdecoy*` the
driver must leave alone, and count in the upstream, with nft counters, what
arrives from a source other than the host. Built with the `testbed` tag,
`hostnet` also exports `BreakWatch`, `BreakNftWatch` and `FailSubscriptions`,
which the tests use to make the driver's subscriptions fail; `hangar-node` is never
built with that tag. `TestInternet` goes out of the testbed's real
uplink to 1.1.1.1, and runs `networkctl reconfigure` on it when networkd
manages it. The tests take `/run/lock/hangar-testbed.lock` too, turn IPv4
forwarding on and leave it on, and remove everything they made, the
driver's `hn*` objects included, at the start and the end.

A failing guest test logs what the host saw: Cloud Hypervisor's log, the
serial console (the kernel's, quiet unless something went wrong), and every
record on the framed console, including hangar-init's own messages.

## Boot time

`TestBootTime` logs the median of ten boots from starting Cloud Hypervisor
to the agent's first Ping answer, and for each when the kernel ran `/init`
by the guest's clock. It fails only past 2 s; compare the median with the
last one recorded when something on the boot path changes. 2026-10, nested
KVM on a 16-vCPU VM (i7-12700H host), hardened kernel: median 168 ms,
kernel at `/init` at about 110 ms.

Turning the hardening on (`docs/decisions/guest-kernel.md`) was measured
there against the same kernel without it: median boot to Ping 165.5 to
167.6 ms; in two rounds of seven, 4M one-byte `dd` syscalls 311 and 349 ms
to 351 and 396 ms (about 10 ns, 13%, per syscall) and 1000 fork and exec
200 and 217 ms to 208 and 213 ms; `docker run hello-world` with the image pulled, 313 to
294 ms (noise). That CPU is not affected by Meltdown, so page table
isolation cost nothing; on one that is, syscalls cost more.

Each guest boots from a fresh copy of the root image, flushed before the
boot: without the flush, writeback of the copy races the guest's first
reads, and the root mount alone takes 300 ms or more.
