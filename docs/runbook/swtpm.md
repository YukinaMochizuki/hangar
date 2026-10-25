# The swtpm bundle

A VM with a TPM gets a TPM 2.0 (CRB) that Cloud Hypervisor serves from
swtpm, one per VM, which the VM's launcher runs in the VM's unit as the VM's
user, with no capability, as it runs Cloud Hypervisor
(`internal/node/driver/chvm/tpm.go`). That swtpm is Hangar's own bundle,
never the distribution's `/usr/bin/swtpm`: a VM's unit has an empty root
and takes only static executables, and Ubuntu's AppArmor profile for
`/usr/bin/swtpm` would refuse the VM's directory. The bundle is swtpm with
musl and every shared library it needs, from Alpine's packages, run by its
own musl loader, which is static:

```sh
DIR/lib/ld-musl-x86_64.so.1 --library-path DIR/lib DIR/bin/swtpm socket ...
```

| File | Holds |
|---|---|
| `deploy/swtpm/swtpm.env` | The Alpine branch and mirror, the release that mirrors the packages (`HANGAR_SWTPM_RELEASE`, the converter's `converter-deps`), and each package as `REPO/NAME-VERSION SHA256` |
| `deploy/swtpm/build.sh` | Download (`tools/converterdeps fetch -prefix HANGAR_SWTPM`), check, extract, pack |
| `tools/converterdeps` | `fetch`, `mirror` and `updates`, as for the converter (`docs/runbook/converter.md`), with `-prefix HANGAR_SWTPM -env deploy/swtpm/swtpm.env` |
| `.github/workflows/converter-mirror.yml`, `converter-updates.yml` | Mirror the bundle's pins too, and open an issue of their own titled `swtpm: newer Alpine ...` |
| `deploy/host/versions.env` | `HANGAR_SWTPM_VERSION`, the swtpm version the build must find and a host's `verify.sh` checks |

## Build

```sh
make host-swtpm          # or deploy/swtpm/build.sh
```

It needs Go, tar and sha256sum, and no C toolchain. It downloads each
package into `.cache/swtpm/`, first from the release, then from Alpine,
each checked against its pin, keeps `usr/bin/swtpm`, musl's loader and the
packages' shared libraries, flat in `lib/` (swtpm's own
`libswtpm_libtpms.so.0` among them: the loader is given `--library-path`
in place of its RUNPATH), writes `VERSION` (`hangar-swtpm
swtpm-0.10.2-r0`), checks that swtpm runs under that loader
(`swtpm --version`), and writes `bin/host/swtpm.tar`, one directory
`swtpm/`, reproducibly. `make host-swtpm` also unpacks it as
`bin/host/swtpm`, which the L4 tests run. `HANGAR_SWTPM_WORKDIR` and
`HANGAR_SWTPM_OUT` override the directories. About 13 MB unpacked, 5 MB
gzipped; libcrypto is most of it.

`make release` builds it into the release, listed in `SHA256SUMS`.
`deploy/host` installs it as `/usr/local/lib/hangar/swtpm.tar`, unpacks it
into `/usr/local/lib/hangar/swtpm.d/<first 16 of its sha256>/`, and points
the symbolic link `/usr/local/lib/hangar/swtpm` at that, in one rename;
the previous release's bundle is kept, older ones removed. A running VM's
swtpm stays the one it started with, even after two upgrades have removed
its directory: it is already loaded, and nothing runs it again before the
VM's next start, which takes the new one. `verify.sh` checks the link is
the installed tar's and its `VERSION` says `HANGAR_SWTPM_VERSION`.
hangar-node runs it from `--vm-swtpm` (that link by default); without it,
machines with a TPM are refused.

## How it runs

The launcher, before Cloud Hypervisor: reads the VM's key from the unit's
credentials (`LoadCredential=vtpm:/run/hangar/vtpm/<machine uid>`, the
agent's file, `--vm-tpm-keys`), makes `tpm/` in the VM's directory the
VM's user's (root's 0700 first: the launcher has no `CAP_FOWNER`), listens
on `tpm.sock`, root's and the VM's group's, 0660, outside `ch/`, and runs
swtpm through vmm-exec with that socket on fd 3, the key on a pipe on fd 4
and a pipe for its log on fd 5:

```sh
swtpm socket --tpm2 --tpmstate dir=<vm>/tpm --ctrl type=unixio,fd=3 \
  --key fd=4,format=binary,mode=aes-256-cbc --flags not-need-init --log fd=5,level=1
```

- Never `--terminate`: swtpm would end with Cloud Hypervisor's first
  connection, which a guest's reboot ends, and the VM with it.
- Never `--log fd=2`: given a log fd, swtpm closes its stderr first, and
  then fails writing to it, exiting 1 with nothing said.
- `not-need-init`: a wrong key fails swtpm as it starts, in the unit's
  journal, not Cloud Hypervisor's boot.
- swtpm's RLIMIT_NOFILE is the host's `nr_open`, which the node reads (the
  unit's `ProcSubset=pid` hides `/proc/sys`): the unit's hard limit is
  `nr_open` and its soft one 1024, and vmm-exec, as root, raises swtpm's
  alone; it has no `CAP_SYS_RESOURCE` to go past the unit's.

Once Cloud Hypervisor exits, the launcher sends swtpm SIGTERM, then
SIGKILL after its grace. A stop keeps `tpm/`, made root's before the VM's
id is freed; it goes with the VM's logs, when the machine is gone.

## Known issues

Both are Cloud Hypervisor's; a patched Cloud Hypervisor is to fix them
(hangar#212, `docs/decisions/vm-tpm.md`).

- Cloud Hypervisor (v53.0, and main as of 2026-10) sends its stdin's fd
  with every control command after the first (`tpm/src/socket.rs` resets
  the fd to send to 0, not -1), and swtpm keeps them: 7 more per guest
  boot. swtpm's RLIMIT_NOFILE of `nr_open` keeps that from mattering.
- Cloud Hypervisor gives up on a TPM reply after 100 ms (`SO_RCVTIMEO` on
  the data socket, cloud-hypervisor#9053), and the guest's TPM then fails
  until the guest reboots (`tpm_recv: error -5`). An RSA-2048
  `CreatePrimary` takes swtpm 30 to 120 ms on a testbed, so a guest
  making RSA keys can hit it without any load.

## Bump swtpm, or a library

As for the converter's packages (`docs/runbook/converter.md`), with
`swtpm.env`: resolve the dependencies from the NEEDED entries of
`usr/bin/swtpm` and of `usr/lib/swtpm/libswtpm_libtpms.so.0`, not from the
package's `D:` line, which counts swtpm_setup's and swtpm_cert's libraries
too (gnutls, gmp, libtasn1); for swtpm 0.10.2 on 3.22 that is the 14
packages `swtpm.env` lists. A new swtpm changes `HANGAR_SWTPM_VERSION`.
Then `make host-swtpm`, and on a testbed the TPM's L4 test
(`make e2e-host-vms RUN=TestAVMsTPMKeepsItsState`, `docs/runbook/testbed.md`).
