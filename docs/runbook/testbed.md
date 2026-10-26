# Testbed: real-host tests (L4)

The L4 tests (`docs/decisions/testing-strategy.md`) run what no workspace can:
Cloud Hypervisor, KVM, taps, guests. They live under `test/testbed/`, behind
the `testbed` build tag so `make test` and `make check` never build them, and
`make e2e-host` runs them. Run them only on a disposable VM: they run as root,
create taps and an nftables table, and turn on IPv4 forwarding.

| Path | What it tests |
|---|---|
| `test/testbed/guest/` | The platform kernel, initramfs and hangar-init under Cloud Hypervisor, driven as the node drives them: the boot disk (`internal/guest/bootdisk`), the framed console (`internal/guest/console`) and the guest agent over hybrid vsock (`internal/node/driver/chguest`), including its `SetIPv6`, which changes an interface's public IPv6 and default route by netlink while the process runs and refuses what is not a /128, and that it serves the host alone (`TestOnlyTheHostReachesTheAgent`: a container running as `USER nobody` runs `vsockprobe`, which runs as uid 65534 and tries the agent's port at CID 1, the guest's own CID and CID 2: each connect must fail for want of a peer, by its errno, and a vsock socket the probe cannot make fails the test; as a control, it must reach a port the host listens on at CID 2; the node's Exec, the container's log and Stop work on that guest), and FileShares (`fileshare_test.go`): a server, booted from its boot disk on a blank disk of its own at 1 vCPU and 256 MiB, and container guests that read each other's writes, one through a subpath over a target the image fills, the image's files never reaching the share and no `lost+found` in it; each guest its own NFS client (the identity file holds its machine uid, and a second guest's mount leaves the first's lock held); the mount's options (NFSv4.2, 1 MiB blocks, `hard`, `timeo=50`, `nosuid`, `nodev`); NFSv3, 4.0 and 4.1 mounts refused; a read-only share mounted `ro,nosuid,nodev` and a read-only Disk volume `ro` beside it; `ro` holding after the client remounts read-write; a user's supplementary groups counting; squash None, Root and All changed live by `SetExport`; a bad export (an anonymous uid of (uid_t)-1, a client 0.0.0.0) refused with the last kept, and one on a boot disk booting no server, the reason on its console (`TestAServerRefusesABadExport`); a client dropped from the export refused within seconds and let back in; a share not exported to a guest, or a subpath not in it, failing its boot with the volume named; links another client plants in a share never leading where the platform mounts its own: a subpath through a link to / mounts the share's root, and a target nested in an outer share through a link to /, or a share over /etc whose `hosts` links into the guest's procfs, fails the boot, saying so; and the server stopped, then killed, pausing a writer only seconds, its lock and another client's reclaimed, and a client killed for good holding the others up for the 15 s grace period alone, its lock then free, and client records it cannot read (set on the share's disk from the host, loop-mounted while the server is off) moved to `trusted.hangar.nfsd-clients.bad` with the server serving on (`TestAServerRestartPausesClientsSeconds`). Locks are tested with `flock -s`: busybox's `flock` opens read-only, and the NFS client refuses an exclusive lock on a file open read-only. No node or driver code |
| `test/testbed/disks/` | The image and storage drivers (`internal/node/driver/registry`, `zfsstore`) on the `hangar` pool: the conformance suite (`drivertest`), volumes and FileShares' volumes (`hangar/shares`) included, with decoy datasets about, Docker Hub pulls by tag and digest compared with the reference flattening, a guest booting from a root disk larger than its image, its filesystem grown by hangar-init, custom images' bases (the conformance suite's custom images part: a volume's snapshot sent into a base keyed by the image, which holds what the volume held when snapshotted and not what was written after, and outlives the volume; decoys at custom images' names left alone), and every zvol's device made once, as `dev` (`TestEveryZvolIsMadeAsDev`: the parent datasets given `volmode=dev`, 20 volumes cloned and grown with no device removed and no kernel BUG; only an OpenZFS 2.4.x host shows the remove-and-remake it guards against) |
| `test/testbed/vms/` | The real hypervisor driver (`internal/node/driver/chvm`) and its launcher, `hangar-node vmm`, under real systemd and Cloud Hypervisor, with every other real driver: the conformance suite (L3), its converter part with a converter VM booting the converter initramfs (`make guest-converter`) to inspect raw files staged between zeros (one ending in a VHD footer, one starting with VHDX's signature among them) and write them into bases the storage driver filled, read back from volumes of them, with guests booting from root disks the storage driver made and a FileShare's server (booted on its share's zvol behind a blank `vda`, on a server's tap alone, told an export and refusing a bad one, its zvol held while it runs, stopped as asked), run again on a host with an IPv6 prefix (`TestConformanceWithIPv6`, on a dummy uplink of its own, so the host's is left alone). That run also tells a container's guest its IPv6 while it runs and checks that a reboot brings back its boot disk's, and a firmware VM on an empty volume stopped by its power button, an Ubuntu cloud image booting through firmware to a login prompt on its serial console, one with a TPM (`TestAVMsTPMKeepsItsState`: `/dev/tpm0` in the guest, an NV index and a persistent key the guest makes with `tpmprobe.py`, raw TPM 2.0 commands, the same after the guest's reboot and after the VM's stop and start, the state on the host encrypted, root's while the VM is stopped and gone with its logs, and swtpm confined as Cloud Hypervisor is: the VM's user, no capability, no_new_privs, the VM's unit, AppArmor unconfined, its fds, nr_open of them, its control socket root's and the VM's group's), its log, its power button shutting it down at a host shutdown (`systemctl stop`, the boot ended as the platform's, the VM then not listed, for the agent to start again) and booting it again to a login prompt, and the driver's stop powering it off, a CirrOS image made a volume a guest sees partitioned, their logs read and followed across a restart, execs in them (output, stdin, a terminal and its resize, signals, an exec's group killed with its context), graceful and forced stops (`Stop`, a process ignoring SIGTERM, a frozen guest), restart counts, a VM outliving its agent killed outright, root disk links refused by `BLKZNAME`, volumes a guest sees as its next disks (`vdc` on, at their sizes, what one machine wrote read by the next, a volume's link to another zvol refused), volumes hangar-init mounts (`TestAGuestMountsItsVolumes`: a blank one formatted and filled from the image with its owners, what one machine wrote read by the next and not filled again, read-only and subpath mounts, a partitioned one failing the boot untouched), the console log under a 40 MiB flood and forged records (one VM's harming no other), a serial log failing under a `chattr +i` directory without freezing the guest, VMs interrupted as by a host shutdown under each policy (their units stopped with `systemctl stop`: the SIGTERM handler's output in the log, a process ignoring SIGTERM killed within the grace and slack, nothing recorded), the driver reconnecting to systemd, nothing left after the sweep with decoy units and files about, `hangar-node --drivers=host` starting, the VMs' CPU shares (`TestTheVMsShareTheirSliceByMemory`: with one CPU kept for the host, `hangar.slice` at weight 1000 on the other CPUs, each VM's unit in it at a weight of its memory, and the slice given every CPU again by a driver keeping none), the confinement (`TestCHIsConfined`: a probe, `chprobe`, run in Cloud Hypervisor's place in a real VM's unit, tries the host's files, another VM's directory, socket, zvol and process, devices, privileges and the network; links it plants at its own socket's and its API socket's paths, to the other VM's socket, refused by the driver, the second as it presses a firmware VM's power button; the other VM's real Cloud Hypervisor checked from the host; both launchers left with `CAP_DAC_OVERRIDE` and `CAP_KILL` alone once Cloud Hypervisor runs; the unit's `/dev` read-only to root in it; the device policy from a root process in the unit's cgroup; the VM's uid outside any unit; `systemd-analyze security`), and a VM started without VM users running on across an upgrade of the launcher and confined once started again |
| `test/testbed/network/` | The real network driver (`internal/node/driver/hostnet`): the drivers' conformance suite (L3), with and without an IPv6 prefix, then stand-in guests on taps through both NATs, isolation, forged sources, egress only by the uplink, drift, failing watches, collisions and cleanup; each tap receiving only its own unicast (a guest sniffing its eth0 sees none of two others' unicast or the namespace's, even with the bridge's forwarding database flushed, and does see it once its tap takes flooded unicast, the test's control), no interface taking another's MAC or the gateway's, while DHCP, ARP, neighbour discovery and DHCPv6 still work (`TestATapReceivesOnlyItsOwnUnicast`); and public IPv6 against an upstream that behaves as OVH's (the host's /64 on-link at it, each address asked for by neighbour solicitation and delivered only to the MAC that answers): guests configured by router advertisement and DHCPv6 with a real client (`dhcpcd`, which a stock Ubuntu has), out as their own /128, reached from the upstream or another network only as their groups allow and through the host's proxy NDP, forged sources, router advertisements, DHCPv6 replies and neighbour advertisements dropped with each filtering layer alone, and routes and proxy entries removed by hand repaired. It also checks that each /128 has a permanent neighbour entry on its network's bridge at its tap's MAC, that the driver refuses an address another tap holds, and that an address moving to an interface on another network takes its route, proxy entry and neighbour entry with it. File share servers' taps (the conformance suite's part, and `fileshare_test.go`): a server takes TCP to 2049 from its clients alone, whatever its groups and edge ports say, a client taken out reaches it no more, a client locked by its groups still reaches it; the server reaches neither the resolver nor the metadata service on its gateway, nor a guest, nor the upstream, and the platform's SSH does not reach it; a server's tap is refused behind a firewall an older agent wrote, until the firewall is written again. Then the abuse protection: TCP to 25 and 465 dropped over both families and 587 open (`TestNoMail`); a network's new connections held to a burst of 300 and 100 a second from one bucket for both families, a connection opened before still answered, another network not held back (`TestNewConnectionsAreLimited`, UDP probes the upstream counts); and every new IPv4 connection reported with the public port the upstream sees, joined to its network and machine address and port, between two networks with the same range and address, over TCP, UDP and ICMP, named by the agent's names and found by that port in a NAT log, and no connection of the host's own reported (`TestNATLog`); the conformance suite's NAT part also has a network made on the transit link of one deleted told apart from it. With a shared address (`shared_test.go`, `forward_test.go`, and the conformance suite's port forwards part, which `TestConformance` runs with one held): guests leave as it, TCP and UDP from its upper ports; the upstream reaches a guest's TCP and UDP through a forwarded port of it and the guest sees the upstream's own address, a port with no forward or a forward's port at the host's own address reaches nothing, the groups judge what a forward brings in, two networks of one range each get their own forward, a removed forward closes, one TCP connection and one UDP socket kept open across a forward moved within a network, to another and removed never reach the machine it left (`TestOpenFlowsEndWithTheirForward`), and the same forwards ensured again rewrite no table. Network endpoints (`endpoint_test.go`, and the conformance suite's part): a guest reaches a guest of another network of the same range at its endpoint's address of its own range, over TCP and UDP, seen from its network's transit address; no other port of the address answers, nor a ping; a network without an endpoint of the service reaches nothing of it, even at the service's transit address and port; the service's machine's groups judge what comes in from the network's transit address; one TCP connection and one UDP socket kept open across an endpoint moved and removed never reach the machine it left; the same endpoints, and their network, ensured again rewrite no table and keep the endpoint's address on the bridge; with two networks reaching one service, removing one's endpoint ends its flows in the service's namespace and keeps the other's (`TestARemovedNetworksFlowsEndAtTheService`); a network's endpoint taken away is gone while another network's namespace, its bridge deleted by hand, cannot be written (`TestARevokedEndpointGoesWhateverAnotherNetwork`); and a datagram from the upstream forging a network's transit address does not reach a machine through a port forward, where the upstream's own does (`TestNoTransitSourceComesInByTheUplink`). Needs root, not KVM |
| `test/testbed/flow/` | The whole system, as deploy/host puts it on a host: the real `hangar-apiserver` (as `nobody`) and `hangar-node --drivers=host` (as root, over the apiserver's node socket) as processes, driven by the real `hangar` CLI. One test, `TestFlow`, in ordered scenarios, each checked from the API and from the host: apply and Ready; online through both NATs, the connection in the agent's NAT log (`--nat-log-dir`, in `/var/tmp/hangar-testbed-flow/nat`) and named web by `hangar-node natlog` given its public port; stop and start with `runState`, the disk kept; restart under `on-failure`; growing the disk; changing the image; the agent killed mid-pull and started again; a host shutdown (the agent, then each VM's unit stopped with `systemctl stop`), after which `always` and `on-failure` boot again, their Cloud Hypervisor as the VM user it had, and `never` is Exited with 253; `hangar logs` across a restart, with each option, and `logs -f` killed ending the agent's read of the log; `hangar exec` in a busybox container (the container's environment and overrides, stdout and stderr apart, exit codes, `-i` with piped stdin, `-it` on a pty for its size, a resize and Ctrl-C as a key, the terminal put back, SIGINT passed on without `-t`, a CLI killed leaving nothing running in the guest, the apiserver's audit line); delete. Beside it, tests of their own cluster each, `TestAFileShare` among them: two containers mounting a share read each other's writes, the server on a zvol of the share's on `hnts` taps, a container declaring nothing reaching neither its 2049 nor its ping, the share grown and given a network with the clients' writes going on and the clients never restarted, and the share's zvol gone with it; and `TestAVMMountsAShareByHand`: an Ubuntu VM declaring a share mounts it with the console's commands (cloud-init installing `nfs-common`), reading a container's write and writing one it reads (minutes, as the cloud image tests below); and `TestAVMBootsWithItsCloudInit`: an Ubuntu VM from the image catalogue (the cached image served over HTTPS, its certificate given to the agent as `SSL_CERT_FILE`, the cached firmware as `--vm-firmware`; skipped in an installed run) with a key and a user-data, whose cloud-init reports on the serial console, read with `hangar logs vm`, the file the user-data wrote, its hostname, its instance id and the key from vendor-data (minutes, as the cloud image tests below). `TestAVMsTPMThroughTheAPI`: an Ubuntu VM applied with `tpm: {enabled: true}` to an apiserver with a key-encryption key, its agent given the swtpm bundle: `/dev/tpm0` in the guest, an NV index and a persistent key `tpmprobe.py` makes there the same after `hangar stop` and `hangar start`, the key the agent fetched a root-only file while the VM may run and gone once it is stopped, in no answer of `hangar get`, no log and no file of the VM's directory, and the TPM's state gone with the VM (minutes, as the cloud image tests below; skipped in an installed run). `TestACustomImageOfAnUbuntuVM`: an Ubuntu VM from the catalogue writes a marker, is cleaned with `cloud-init clean --machine-id` and stopped, and its root disk is captured as a CustomImage, checked on the pool as a read-only copy of no origin; a VM of the image reads the marker, has its own hostname, instance id and machine id, takes `hangar ssh` and `hangar exec`, and restarts with the source VM and its disk gone; the image deleted, its base stays over two and a half minutes of the agent's sweeps while that VM's disk is cloned from it, and goes with the VM (minutes, as the cloud image tests below). `TestACustomImageUploadedFromAQcow2`: `hangar image upload` sends the cached Ubuntu qcow2, is killed once a chunk counted, and run again goes on from the node's count; the real converter VM converts it, its base checked on the pool as finished as a captured one, its staged file and converter units gone; a VM of the image is Ready, finishes cloud-init and takes `hangar ssh`; that VM's disk, stopped, is captured as a new CustomImage, which becomes Ready; a raw file ending in a VHD footer is attached and converted as raw, and one starting with VHDX's signature attached as raw and refused by qemu-img in the converter (`InvalidImage`), never by Cloud Hypervisor, the staging's zeros at both ends; crafted qcow2s naming a data file or a backing file, and a vmdk, are refused as `InvalidImage`, and a qcow2 holding 30 GiB, over the agent's `--max-image-disk-size 20Gi`, as `ImageTooLarge`, none leaving a base; a qcow2 holding 4 GiB uploaded to a project whose disk limit is 2Gi fails as `QuotaExceeded`, read as 4 GiB, leaving no base nor staged file (minutes, as the cloud image tests below; skipped in an installed run). `TestAnUploadsBoundsOnTheHost` runs two clusters of its own: with `--inspect-timeout 1s` an upload fails as `ConvertFailed`, reading the file past its bound, its converter VM's unit gone and no base made; without the converter initramfs the node says it takes no uploads and an upload is refused as it is made (skipped in an installed run). `TestTheImageCatalogue` boots each image of `deploy/catalog/images.yaml` that `HANGAR_TESTBED_IMAGES` names (names, comma-separated, or `all`; unset, it skips), each file downloaded from its URL into the cache and checked against its sha256: Ready, its serial console, cloud-init done with its hostname and key, the root filesystem grown to a 20 GiB disk, `hangar ssh` by the CA and OpenSSH by a static key as the image's `defaultUser`, `hangar exec`'s exit code, and `hangar stop` powering it off by its power button (`make e2e-host-images IMAGES=...`, `docs/runbook/vm-images.md`). `TestContainersHavePublicIPv6` runs its own cluster whose agent has an IPv6 prefix and, as its uplink, a link to an upstream that behaves as OVH's (`tbfup0`, to the namespace `tbf-up6`): a container takes its /128 and default route from the boot disk, reaches the upstream, resolves its name to its /128, and is reached from the upstream only once a group lets it in. `TestAContainerIsResolvedByItsAlias`: a container's alias on its network resolves, by its full name and through the search domain, to its address in another container of the network, and is NXDOMAIN in one of another network. Two more tests, each with a cluster of its own, move an Address. In `TestAContainerTakesAMovedAddressLive` the container takes it live, answering within seconds, and the old one's tap receives no ping to it. In `TestAVMTakesAMovedAddress`, with Ubuntu VMs, the old one's tap receives nothing from the moment of the move, and the new one answers at its DHCPv6 renewal. FileShares' isolation, each test of its own cluster, is under "FileShare isolation" below |
| `test/testbed/internal/vmtest/` | Not tests: the harness that boots those guests, for any L4 package. A package calls `vmtest.Main` from its `TestMain`, then `vmtest.New`, `Start`, `Ready`, `Exec` |
| `test/testbed/host-bootstrap.sh` | `deploy/host` on a VM made to look like a fresh OVH install (`docs/runbook/host-bootstrap.md`), the front door included. It takes a release (`HANGAR_TESTBED_RELEASE`, the `host/` of what `make release` writes) and the front door's kit (`HANGAR_TESTBED_FRONT_DOOR`, `test/testbed/front-door-kit.sh DIR`: a `hangar-edge` built with the `testbed` tag, whose `--acme-dns testbed:<host:port>` answers DNS-01 with a DNS server of its own, and pebble with its certificates), both built where Go is and copied to the VM. Never install the kit's `hangar-edge` on a host |
| `test/testbed/fetch-release.sh` | `deploy/host/fetch-release.sh` (`hangar-fetch-release`, `docs/runbook/release.md`) against a stand-in for GitHub's release downloads on loopback, serving a small release made on the spot in the published layout: the host tar, with `deploy/` packed as `deploy.tar` in it, beside a CLI archive and `install.sh`. With the pinned `gh` (installed as the packages step does): a wrong digest, a file changed after `SHA256SUMS`, a `SHA256SUMS` naming a path, no `attestation.jsonl` and a bundle that is not one are each refused, with nothing left under the release's names. No release can carry an attestation from this repository's workflow until one is tagged, so for the rest `gh` is swapped for a stand-in that records what it is asked, checks it is asked for exactly this repository, its release workflow, the tag, the commit and GitHub-hosted runners, accepts a file whose digest the bundle lists as a stand-in line, and hands any other file to the real `gh`. A genuine GitHub attestation of another repository's file (cli/cli's of the pinned `gh` tarball) goes through the script to the real `gh` and is refused; the flags the script passed for it, replayed with the real `gh` with only the repository, ref and commit translated to cli/cli's, accept that attestation as it is and refuse it for another ref and for another commit, so a script that dropped `--source-ref` or `--source-digest` fails. A bundle leaving out one file is refused; past the attestations, a host tar holding a link, a hard link, a directory or a path, a file twice, a file its own `SHA256SUMS` does not list or that does not match it, or another commit's `VERSION` is refused; a good release lands in `/var/tmp/hangar/release/TAG` and `deploy-TAG`, a refused refetch leaves it as it was, a refetch replaces it whole, the CLI's files are never downloaded, and no extra argument skips a check. Needs root and to reach GitHub, not KVM |
| `test/testbed/setup.sh` | Not tests: makes the pool `hangar` on a sparse file behind a direct-IO loop device and `hangar/images`, as a host has them, for `test/testbed/disks` |

## Prepare a testbed VM

A bare Ubuntu 24.04 VM with nested KVM (`/dev/kvm`), 8 or more vCPUs, 8 GB
of memory and 20 GB of disk is enough. As a user with sudo:

1. Packages for the kernel build, and nftables:

   ```sh
   sudo apt-get install build-essential flex bison bc libelf-dev libssl-dev curl xz-utils git nftables zfsutils-linux e2fsprogs qemu-utils dosfstools
   ```

2. Go at the version in `go.mod`, from <https://go.dev/dl/>, on `PATH`.
3. Cloud Hypervisor at the version pinned in `deploy/host/versions.env`,
   checked against its sha256 there, installed as `cloud-hypervisor` on `PATH`.
4. A checkout of the branch under test, then `make guest-kernel` (a few
   minutes; `docs/runbook/guest-kernel.md`).
5. `sudo test/testbed/setup.sh`: `qemu-img` and `mkfs.vfat` if they are missing, the ZFS
   pool `hangar`, 24 GB on a sparse file in `/var/tmp` attached as a loop
   device with direct IO (`zpool status` shows `/dev/loopN`), `hangar/images`
   with an 8 GB quota, and the udev rules for zvols the `zfs` step of
   `deploy/host` installs. It is
   idempotent; a VM that ran `test/testbed/host-bootstrap.sh` has the pool
   already. After a reboot, run it again: it attaches the file and imports
   the pool. The pool is never on the file itself: a file vdev writes
   through the root filesystem's page cache, and writeback of a zvol then
   waits on it under the same global dirty limit, which wedged a full
   `make e2e-host` with tasks stuck in D state.

## Run

```sh
make e2e-host
```

It rebuilds the initramfs, and the testbed's own
(`bin/guest-testbed/initramfs.cpio`, `make guest-initramfs-testbed`,
`HANGAR_TESTBED_SERVER_PROBE_INITRAMFS` names another), makes the root disk image on first use, builds
`bin/hangar-node` (the VMs' launcher, `HANGAR_TESTBED_NODE` names another),
`bin/hangar-apiserver` and `bin/hangar` (`HANGAR_TESTBED_APISERVER`,
`HANGAR_TESTBED_CLI`), `bin/ch-probe` (`HANGAR_TESTBED_CHPROBE`), the swtpm bundle (`make host-swtpm`, unpacked as `bin/host/swtpm`, `HANGAR_TESTBED_SWTPM`) and
`bin/vsock-probe` (`HANGAR_TESTBED_VSOCKPROBE`, which the guest tests write
into a guest's root disk with `debugfs`), all
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

The network and flow tests turn IPv6 forwarding on and leave it on, as
they do IPv4's; with it on, the testbed's own uplink takes no more router
advertisements, which matters only to a testbed that relies on them.

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
`TestFreedMemoryReturns`, which runs every time, has a 2 GiB guest fill
and free 1 GiB and fails unless its unit gives most of it back to the host
within 30 s, through free page reporting; it takes about two minutes.
`hangar-testbed-reset` does not know them yet: after a run killed outright,
the next run's sweep, or `systemctl stop 'hangar-vm-*'`, removes them.

The cloud image tests need more than the others.
`TestAnUbuntuVMBootsToALoginPrompt` boots an Ubuntu 24.04 cloud image,
pinned by its sha256, through the EDK2 firmware pinned in
`deploy/host/versions.env`, to a login prompt on its serial console, which
takes minutes with no DHCP on its network yet, shuts it down by its power
button as a host shutdown does, within the shipped 60 s grace, boots it to a
login prompt again, and stops it; about five minutes in all.
`TestAVolumeOfACloudImage` makes a volume of a CirrOS image.
Both download what they need once a testbed (the firmware, and about 600 MB
for Ubuntu) into
`/var/cache/hangar/testbed/disk-images`, which a run keeps; delete it to
fetch again. `HANGAR_TESTBED_FIRMWARE` names another firmware file, which
must be readable by all, as a VM's own user reads it.

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

`TestTheDirectPath` (`RUN=TestTheDirectPath`) is the platform as a host runs
it with nothing in front of it: its cluster's apiserver is in oidc auth
against a fake provider in the test (`internal/auth/oidctest`), set up
through the admin socket, and `hangar-edge`, as `nobody`, is the front door
on `127.0.0.1:443` and `:80` (the test lets unprivileged processes bind
them, `net.ipv4.ip_unprivileged_port_start`, and puts it back), with
certificates from pebble, run in the test, over DNS-01 the edge answers
itself. So `make e2e-host` and `make e2e-host-flow` build `bin/hangar-edge`
with the `testbed` tag; never install it on a host. A browser, whose names
under `.test` reach 127.0.0.1, signs in to the console through the front
door and signs up; a service token reaches the API at `api.`; an nginx
container's private endpoint hands the signed-in browser off from the
console, and a stranger gets nothing; a public endpoint serves anyone. Its
logs include `front-door.log`. Ports 80 and 443 on loopback must be free.

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
env file naming the VM's uplink and address. The `monitoring` step goes
the same way after them, with Alertmanager pointed at a stand-in for
Telegram (`docs/runbook/host-bootstrap.md`, "Testing a change").

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

A FileShare server's restart holds a writer up for whichever of its
client's reconnects first finds the server back: the client notices a
killed server when an RPC times out (5 s, `timeo=50`), and reconnects 3 s,
then 6 s and 12 s more, after losing its connection
(`net/sunrpc/xprtsock.c`), each attempt failing at once while the server's
tap is gone. Over a dozen restarts on the testbed, stopped or killed, a
writer paused 1.4 to 17.8 s (`docs/decisions/file-shares.md`), so `TestAServerRestartPausesClientsSeconds`
fails only past 30 s; nfsd's own defaults stall it more than a minute.

## FileShare isolation

Five tests of `test/testbed/flow` check a share's isolation and behaviour
through the whole system (`docs/decisions/file-shares.md`). Beside each
refusal, a control must succeed; and where a test ends with a mutation, the
agent is killed so nothing puts back what it breaks, the host execs in
guests through their agents' sockets (`hangar exec` needs the agent), and
the check it made must then fail. `TestAShareOnTwoNetworks` and
`TestAShareServerInitiatesNothing` boot their servers from the testbed
initramfs (`make guest-initramfs-testbed`), whose agent reports a version
ending `+testbed` and runs what the host asks: `/init netprobe` and `/init
netlisten` (`cmd/hangar-init/server_testbed.go`).

- `TestWhoMayMountAShare`: a container declaring nothing, one on another
  network of the project, one of another project on the same range at a
  client's very address (made the lowest free by a container made and
  deleted before it), and a client on the network it does not mount from
  (its interface there shown to work: it reaches a neighbour by `eth1`)
  reach neither the server's 2049 nor a mount by hand, which a client
  makes. A guest forging a client's address, the server's MAC set by
  hand, sends frames tcpdump sees leave its tap and none reach the
  server's; each capture runs in immediate mode and ends on a sentinel
  frame sent after the forged ones. A `readOnly` client cannot write, even
  remounted read-write as root. A running container's declaration removed
  starts it again without the share, which it reaches no more. Exec into
  a server of the platform's initramfs is refused. Mutations: the
  forger's tap let through ahead of its binding in `bridge hangar`, its
  frames reach the server's tap; the export made read-write over vsock,
  the reader writes.
- `TestAShareOnTwoNetworks`: clients on each network of a share mount
  their own network's address and read each other's files; neither
  reaches the other directly, through the server's address on its own
  network routed to by hand, at the server's ports but 2049, or at its
  address on the other network, while a neighbour of each reaches its
  responders. The server listens on those ports (TCP 22, 111, 2048, 2050
  and UDP 2049, each answering from the server itself) with IPv4
  forwarding on, so what refuses them is the host's firewall, not a
  closed port or stack. An egress-locked sandbox declaring the share
  mounts it and reaches nothing a container beside it reaches. Shares on
  overlapping networks are refused, made or grown so; the plan's
  `fileShares` and `disk` refuse a share. Mutations: the plan raised takes
  the same shares; the server's and the clients' taps let through in
  `bridge hangar` and `hangar_sg`, with routes through the server, a
  reaches the server's 2050, UDP 2049 and ping, and b through it.
- `TestAShareServerInitiatesNothing`, in a cluster with an IPv6 prefix and
  the upstream of `TestContainersHavePublicIPv6` as its uplink (with
  198.51.100.1 and TCP and UDP echo on 7 there): `/init netprobe` runs in
  the server, with a default route and an IPv6 address of the prefix
  taken by hand, against every machine of its networks (their IPv4, /128
  and link-local addresses), a network it is not on, its gateways' DNS,
  metadata and ICMP, and the upstream, over TCP, UDP and ICMP. Each probe
  must find no answer (a timeout, or no neighbour); a refusal fails the
  test, and so does an error of the probe's own (no route, no address, a
  bad target). A container reaches each target, the control. Mutation:
  the server's taps let through in `bridge hangar` and `hangar_sg`, at
  least one probe gets out for each protocol, each family, on-link and
  routed, and each protocol over IPv4 on-link, IPv6 on-link and IPv4
  routed. Two kinds are never shown reached, so their refusals are not
  shown able to fail: routed IPv6 (the host routes nothing back to the
  address the server takes) and IPv4 to a network the server is not on
  (the host's own rule between networks, which the mutation leaves).
- `TestAVMLosesARevokedShare`: an Ubuntu VM (minutes, as the cloud image
  tests) mounts the share by hand and creates a file on it every half
  second, each given 3 s, its exit status and error on its console; its
  declaration removed, a create fails within 10 s, and must be refused
  by the server (EACCES or EPERM, not killed at its timeout); none
  succeeds after, while a container still writes and the VM is never
  started again. Its creates before are the control. Mutation: the
  server's tap let through, what the VM then gets is logged and a create
  that succeeds fails the test; the VM's address put back in the export
  over vsock, a create succeeds.
- `TestAShareServerKilledUnderLoad`: the server's Cloud Hypervisor killed
  while two containers each append a numbered line every 100 ms, synced
  before it is acknowledged on the console: the writers pause, under the
  30 s of `TestAServerRestartPausesClientsSeconds`, no acknowledged line
  is missing, none is there twice, and no write fails: a client is never
  refused for being unknown. It logs the server's console, whose export
  at each boot says how many clients it started with. Its mutation, the
  export made `async`, is by hand.

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
