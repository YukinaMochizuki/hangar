# Testbed: real-host tests (L4)

The L4 tests (`docs/decisions/testing-strategy.md`) run what no workspace can:
Cloud Hypervisor, KVM, taps, guests. They live under `test/testbed/`, behind
the `testbed` build tag so `make test` and `make check` never build them, and
`make e2e-host` runs them. Run them only on a disposable VM: they run as root,
create taps and an nftables table, and turn on IPv4 forwarding.

| Path | What it tests |
|---|---|
| `test/testbed/guest/` | The platform kernel, initramfs and hangar-init under Cloud Hypervisor, driven as the node drives them: the boot disk (`internal/guest/bootdisk`), the framed console (`internal/guest/console`) and the guest agent over hybrid vsock (`internal/node/driver/chguest`). No node or driver code |
| `test/testbed/disks/` | The image and storage drivers (`internal/node/driver/registry`, `zfsstore`) on the `hangar` pool: the conformance suite (`drivertest`) with decoy datasets about, Docker Hub pulls by tag and digest compared with the reference flattening, and a guest booting from a root disk larger than its image, its filesystem grown by hangar-init |
| `test/testbed/internal/vmtest/` | Not tests: the harness that boots those guests, for any L4 package. A package calls `vmtest.Main` from its `TestMain`, then `vmtest.New`, `Start`, `Ready`, `Exec` |
| `test/testbed/host-bootstrap.sh` | `deploy/host` on a VM made to look like a fresh OVH install (`docs/runbook/host-bootstrap.md`) |
| `test/testbed/setup.sh` | Not tests: makes the pool `hangar` on a sparse file and `hangar/images`, as a host has them, for `test/testbed/disks` |

## Prepare a testbed VM

A bare Ubuntu 24.04 VM with nested KVM (`/dev/kvm`), 8 or more vCPUs, 8 GB
of memory and 20 GB of disk is enough. As a user with sudo:

1. Packages for the kernel build, and nftables:

   ```sh
   sudo apt-get install build-essential flex bison bc libelf-dev libssl-dev curl xz-utils git nftables zfsutils-linux e2fsprogs
   ```

2. Go at the version in `go.mod`, from <https://go.dev/dl/>, on `PATH`.
3. Cloud Hypervisor at the version pinned in `deploy/host/versions.env`,
   checked against its sha256 there, installed as `cloud-hypervisor` on `PATH`.
4. A checkout of the branch under test, then `make guest-kernel` (a few
   minutes; `docs/runbook/guest-kernel.md`).
5. `sudo test/testbed/setup.sh`: the ZFS pool `hangar`, 24 GB on a sparse
   file in `/var/tmp`, `hangar/images` with an 8 GB quota, and the udev
   rules for zvols the `zfs` step of `deploy/host` installs. It is
   idempotent; a VM that ran `test/testbed/host-bootstrap.sh` has the pool
   already.

## Run

```sh
make e2e-host
```

It rebuilds the initramfs, makes the root disk image on first use, and runs
`go test -exec 'sudo -E' -tags testbed ./test/testbed/...`: the tests are
built as you and run as root. The root disk image,
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
