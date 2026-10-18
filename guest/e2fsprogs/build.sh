#!/usr/bin/env bash
# Builds the static mke2fs the platform initramfs carries, in
# bin/guest/mke2fs, from the e2fsprogs release e2fsprogs.env pins. A build
# whose inputs, the toolchain among them, have not changed since the last
# is kept.
#
# It needs a C toolchain, as the kernel does: docs/runbook/guest-kernel.md.
set -euo pipefail
shopt -s inherit_errexit

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
# shellcheck source=guest/e2fsprogs/e2fsprogs.env
. "$HERE/e2fsprogs.env"

WORK=${HANGAR_E2FSPROGS_WORKDIR:-$REPO/.cache/e2fsprogs}
OUT=${HANGAR_E2FSPROGS_OUT:-$REPO/bin/guest}
JOBS=${JOBS:-$(nproc)}

log() { printf '%s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }

for cmd in curl sha256sum tar xz make gcc strip readelf ldd; do
	command -v "$cmd" >/dev/null || die "$cmd not found (prerequisites: docs/runbook/guest-kernel.md)"
done

# The toolchain is an input: mke2fs is linked statically against its libc.
key=$( (cat "$HERE/e2fsprogs.env" "$HERE/build.sh"; gcc --version | sed -n 1p; ldd --version | sed -n 1p) | sha256sum | cut -d' ' -f1)
if [ -x "$OUT/mke2fs" ] && [ "$(cat "$OUT/mke2fs.key" 2>/dev/null)" = "$key" ]; then
	log "mke2fs: up to date"
	exit 0
fi

TARBALL=$WORK/e2fsprogs-$HANGAR_E2FSPROGS_VERSION.tar.xz
mkdir -p "$WORK"
if [ ! -f "$TARBALL" ]; then
	log "download: $HANGAR_E2FSPROGS_URL"
	curl -fsSL -o "$TARBALL.part" "$HANGAR_E2FSPROGS_URL"
	mv "$TARBALL.part" "$TARBALL"
fi
# Verified on every run, so a corrupt or swapped cache cannot slip through.
if ! echo "$HANGAR_E2FSPROGS_SHA256  $TARBALL" | sha256sum --check --status; then
	rm -f "$TARBALL"
	die "sha256 mismatch for $TARBALL (deleted); e2fsprogs.env and kernel.org disagree"
fi

BUILD=$WORK/build
rm -rf "$BUILD"
mkdir -p "$BUILD"
tar -xJf "$TARBALL" -C "$BUILD" --strip-components=1
# Static, as /init is: the initramfs holds no libraries. Only mke2fs and the
# libraries it links are built.
(
	cd "$BUILD"
	./configure --quiet --disable-nls --disable-fuse2fs --disable-defrag \
		--disable-debugfs --disable-imager --disable-resizer --disable-uuidd \
		--disable-e2initrd-helper --without-libarchive \
		LDFLAGS=-static >/dev/null
	make -s -j"$JOBS" libs >/dev/null
	make -s -j"$JOBS" -C misc mke2fs >/dev/null
)
# The initramfs has no dynamic loader.
if readelf -lW "$BUILD/misc/mke2fs" | grep -q INTERP; then
	die "mke2fs is dynamically linked"
fi
strip "$BUILD/misc/mke2fs"

mkdir -p "$OUT"
install -m 0755 "$BUILD/misc/mke2fs" "$OUT/mke2fs.part"
mv "$OUT/mke2fs.part" "$OUT/mke2fs"
printf '%s\n' "$key" >"$OUT/mke2fs.key"
"$OUT/mke2fs" -V 2>&1 | head -n 1 >&2
