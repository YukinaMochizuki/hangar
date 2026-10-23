# The converter initramfs

Converter VMs, which read the files tenants upload for CustomImages
(`docs/decisions/custom-images.md`), boot the platform kernel with an
initramfs of their own: hangar-init as `/init`, the same binary as the
platform initramfs's, made a converter by the boot disk; and qemu-img with
musl and every shared library it needs, from Alpine's packages. The
platform initramfs carries none of it.

| File | Holds |
|---|---|
| `guest/converter/converter.env` | The Alpine branch and mirror, the release that mirrors the packages (`HANGAR_CONVERTER_RELEASE`), and each package as `REPO/NAME-VERSION SHA256` |
| `guest/converter/build.sh` | Download (`tools/converterdeps fetch`), check, extract, build hangar-init, pack |
| `tools/converterdeps` | `fetch`, `mirror` and `updates`, below |
| `.github/workflows/converter-mirror.yml` | Uploads to the release each pinned package it lacks, on a push to main that changes `converter.env`, and on dispatch |
| `.github/workflows/converter-updates.yml` | Weekly, and on dispatch: opens an issue naming the pins Alpine has newer versions of, and the CVEs they fix |
| `deploy/host/versions.env` | `HANGAR_CONVERTER_QEMU_IMG`, the qemu-img version the build must find and a host's `verify.sh` checks |

## Build

```sh
make guest-converter          # or guest/converter/build.sh
```

It needs Go, tar and sha256sum, and no C toolchain. It downloads each
package into `.cache/converter/`, first from this repository's release
`converter-deps` (the `HANGAR_CONVERTER_RELEASE` URL, a slash and the
file's name), then, for one the release lacks or holds wrong, from Alpine;
each is checked against its pin as it comes, and again on every run (a
mismatch deletes the download and stops). It then extracts
`usr/bin/qemu-img`, musl's loader and the packages' shared libraries,
checks qemu-img runs under that loader (`qemu-img --version`), and writes
`bin/guest/converter.cpio`, an uncompressed newc archive, reproducibly:
the same tree builds the same bytes. `HANGAR_CONVERTER_WORKDIR` and
`HANGAR_CONVERTER_OUT` override the directories. `make release` builds it
into the release, listed in `SHA256SUMS` (`docs/runbook/release.md`);
`deploy/host` installs it as `/usr/local/lib/hangar/converter.cpio`, which
hangar-node boots converter VMs with (`--guest-converter-initramfs`).
Without the file, a node takes no uploads.

**The release, and a token.** A public repository's release assets
download by their URL, with no credential. A private repository's do not:
with `GITHUB_TOKEN` set, `fetch` reads the release through GitHub's API
instead, with that token, which needs only to read the repository's
contents; without one, or where the release cannot be read, it falls back
to Alpine, so a build never needs the token, only a reachable Alpine
without it. The release workflow's build passes its own read-only token
(`release.yml`). On a testbed or a workstation, export a fine-grained
token with read access to the repository's contents, as `GITHUB_TOKEN`,
before `make guest-converter`, or let it download from Alpine.

## Bump qemu-img, or a library

Alpine keeps only the latest build of each package in a branch, so a pin
stops downloading from Alpine once Alpine ships a fix; the release keeps
it, which is why the build goes there first. The converter-updates
workflow says, each Monday, which pins Alpine has newer versions of: it
opens one issue titled by the packages and their new versions, with the
CVEs Alpine's secdb lists as fixed since each pin, labelled `security` if
there are any, unless an open issue of that title is there already. Run it
by hand with `go run ./tools/converterdeps updates` (no token: it only
prints). To bump:

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
4. Merge to main. The converter-mirror workflow then downloads each new
   package from Alpine, checks it against its pin and uploads it to the
   `converter-deps` release, with the workflow's own `GITHUB_TOKEN`
   (`contents: write`) and no other credential; the old ones stay, for
   builds of older commits. Run it by hand from the Actions tab, or with
   `GITHUB_TOKEN=... go run ./tools/converterdeps mirror`.
5. `make guest-converter`, and on a testbed the converter's L4 part
   (`docs/runbook/testbed.md`): it uploads a qcow2, converts it and boots
   a VM of it, and has the converter refuse crafted files.
6. Release (`docs/runbook/release.md`): the host takes the new archive
   with it.
