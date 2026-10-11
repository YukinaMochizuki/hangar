# Guest kernel and initramfs

`guest/kernel/` builds the one kernel every machine boots, and
`guest/initramfs/` the platform initramfs that carries hangar-init
(`docs/decisions/guest-kernel.md`):

| File | Holds |
|---|---|
| `kernel.env` | The pinned version, its tarball URL and sha256, and the base command line |
| `hangar.fragment` | What Hangar needs on top of x86_64 `allnoconfig` |
| `config` | The resolved `.config`, minus the lines that only name the toolchain |
| `build.sh` | Download, verify, configure, check, build |

## Build

Needs a C toolchain and the kernel's build dependencies. On Ubuntu:

```sh
sudo apt-get install build-essential flex bison bc libelf-dev libssl-dev curl xz-utils
make guest-kernel          # or guest/kernel/build.sh
```

It downloads the tarball from kernel.org into `.cache/guest-kernel/`, checks
its sha256 on every run, builds out of tree in `.cache/guest-kernel/build/`,
and writes `bin/guest/vmlinux` and `bin/guest/config`. A build takes three
to seven minutes on 8 cores. `HANGAR_KERNEL_WORKDIR`, `HANGAR_KERNEL_OUT` and
`JOBS` override the defaults.

The build stops before compiling when:

- the tarball's sha256 does not match `kernel.env` (the download is deleted);
- Kconfig dropped or changed anything `hangar.fragment` asks for, usually an
  unmet dependency or a renamed symbol. The message names the fragment line;
- the resolved config differs from the committed `config`. The diff is
  printed. If it is what you meant, run `guest/kernel/build.sh --update-config`
  and commit the new `config`.

`--config-only` stops after these checks, which takes seconds; use it while
editing the fragment.

The committed `config` was resolved with GCC 13 on Ubuntu 24.04. A different
compiler can change toolchain capability symbols (`CONFIG_CC_HAS_*` and the
options that depend on them); that shows up as a diff, and it is a real
difference in what gets built.

## Build the initramfs

```sh
make guest-initramfs                      # or guest/initramfs/build.sh
make guest-initramfs INIT_MAIN=path/to/dir  # pack another main package as /init
```

Needs only Go. It builds `./cmd/hangar-init` (or the main package in
`INIT_MAIN` / `--main DIR`, which may belong to another module) as a static
binary (`CGO_ENABLED=0`, `-trimpath`, linux/amd64) and packs it with
`tools/newc` into `bin/guest/initramfs.cpio`, an uncompressed newc archive
holding `/init` and the empty directories `dev`, `proc`, `sys` and
`newroot`. There are no device nodes: hangar-init mounts devtmpfs itself,
then mounts the root disk on `/newroot` and switches to it.

The archive is reproducible: entries are sorted, owned by 0:0 and stamped
with `SOURCE_DATE_EPOCH` (default: the time of the last commit), so two
builds of the same tree give the same bytes. It is left uncompressed on
purpose; the kernel has no `CONFIG_RD_*` decompressor built in, so a
compressed archive will not boot. `HANGAR_INITRAMFS_OUT` overrides the output
directory.

## Boot it

Cloud Hypervisor boots `vmlinux` directly through PVH, with the initramfs.
Start from `HANGAR_KERNEL_CMDLINE` in `kernel.env` and append the guest's own
settings; keep `quiet`. The kernel runs `/init` from the initramfs, so no
`init=` is needed.

```sh
. guest/kernel/kernel.env
cloud-hypervisor --kernel bin/guest/vmlinux \
  --initramfs bin/guest/initramfs.cpio \
  --cmdline "$HANGAR_KERNEL_CMDLINE" \
  --disk path=rootfs.ext4 --cpus boot=2 --memory size=2048M \
  --serial tty --console off
```

## Change the fragment

1. Edit `hangar.fragment`. Disable with `CONFIG_FOO=n`; the check treats it as
   "must not be set". `allnoconfig` turns off every option it may, whatever
   its default, so an option the guest needs must be named even when Kconfig
   defaults it to y.
2. `guest/kernel/build.sh --update-config` and read the diff of `config`
   (`git diff guest/kernel/config`). Options pulled in or dropped as a side
   effect show up there.
3. Build and boot it, and check whatever the change was for. Anything listed
   in `docs/decisions/guest-kernel.md` must keep working; Docker inside a guest
   is the broadest single check.
4. Commit the fragment and `config` together.

## Bump the patch release

Take every patch release of the current LTS line.

1. Find the version on <https://www.kernel.org/releases.json> (the `longterm`
   entry for the current line).
2. Take its sha256 from kernel.org's signed checksum list, not from your own
   download:

   ```sh
   curl -fsS https://cdn.kernel.org/pub/linux/kernel/v6.x/sha256sums.asc | grep ' linux-6.18.NN.tar.xz$'
   ```

3. Set `HANGAR_KERNEL_VERSION` and `HANGAR_KERNEL_SHA256` in `kernel.env`.
4. `guest/kernel/build.sh --update-config`. Usually only the version line in
   `config` changes; anything else in the diff deserves a look.
5. Build and boot it, and run Docker in a guest, before committing.

## Move to the next LTS

Move once kernel.org marks the new line `longterm`, not before.

1. Bump `kernel.env` as for a patch release (the URL follows the major
   version on its own).
2. `guest/kernel/build.sh --config-only`. Expect fragment failures: symbols get
   renamed, split or gain dependencies. Fix the fragment until the check
   passes, choosing what preserves the features in
   `docs/decisions/guest-kernel.md`.
3. `--update-config` and read the whole `config` diff; new defaults arrive
   with every release.
4. Build, boot and run Docker in a guest, and compare boot time against the
   previous kernel.
5. Update the current line named in `docs/decisions/guest-kernel.md` in the
   same change.
