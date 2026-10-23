# The converter initramfs

Converter VMs, which read the files tenants upload for CustomImages
(`docs/decisions/custom-images.md`), boot the platform kernel with an
initramfs of their own: hangar-init as `/init`, the same binary as the
platform initramfs's, made a converter by the boot disk; and qemu-img with
musl and every shared library it needs, from Alpine's packages. The
platform initramfs carries none of it.

| File | Holds |
|---|---|
| `guest/converter/converter.env` | The Alpine branch and mirror, and each package as `REPO/NAME-VERSION SHA256` |
| `guest/converter/build.sh` | Download, check, extract, build hangar-init, pack |
| `deploy/host/versions.env` | `HANGAR_CONVERTER_QEMU_IMG`, the qemu-img version the build must find and a host's `verify.sh` checks |

## Build

```sh
make guest-converter          # or guest/converter/build.sh
```

It needs Go, curl, tar and sha256sum, and no C toolchain. It downloads each
package into `.cache/converter/`, checks its sha256 on every run (a
mismatch deletes the download and stops), extracts `usr/bin/qemu-img`,
musl's loader and the packages' shared libraries, checks qemu-img runs
under that loader (`qemu-img --version`), and writes
`bin/guest/converter.cpio`, an uncompressed newc archive, reproducibly:
the same tree builds the same bytes. `HANGAR_CONVERTER_WORKDIR` and
`HANGAR_CONVERTER_OUT` override the directories. `make release` builds it
into the release, listed in `SHA256SUMS` (`docs/runbook/release.md`);
`deploy/host` installs it as `/usr/local/lib/hangar/converter.cpio`, which
hangar-node boots converter VMs with (`--guest-converter-initramfs`).
Without the file, a node takes no uploads.

## Bump qemu-img, or a library

Alpine keeps only the latest build of each package in a branch, so a pin
stops downloading once Alpine ships a fix; the build then says the URL is
gone. To bump:

1. Fetch the index of the branch's `main` and `community` repositories
   (`https://dl-cdn.alpinelinux.org/alpine/v3.22/{main,community}/x86_64/APKINDEX.tar.gz`)
   and resolve `qemu-img`'s dependencies, the `so:` names on its `D:` line
   and theirs in turn, to the packages that provide them (`p:` lines).
   Skip `/bin/sh`, which only package scripts need. For qemu-img 10.0.0
   on 3.22 that is the 21 packages `converter.env` lists.
2. Download each `.apk` from the mirror and put its `sha256sum` beside it
   in `converter.env`; a moved branch (a new `v3.x`) changes
   `HANGAR_CONVERTER_ALPINE` too.
3. If qemu-img's version changed, change `HANGAR_CONVERTER_QEMU_IMG` in
   `deploy/host/versions.env` to match: the build refuses a pin of another.
4. `make guest-converter`, and on a testbed the converter's L4 part
   (`docs/runbook/testbed.md`): it uploads a qcow2, converts it and boots
   a VM of it, and has the converter refuse crafted files.

The host takes the new archive with the next release.
