#!/usr/bin/env bash
# Builds the tailscale disk: a read-only ext4 image, bin/guest/tailscale.img,
# holding bin/tailscaled and bin/tailscale from the Tailscale release
# tailscale.env pins, checked against its sha256. A container that joins a
# tailnet gets it as a disk, which hangar-init mounts at
# /run/hangar/tailscale; nothing of it goes into the platform initramfs,
# which every guest loads into its memory
# (docs/decisions/container-tailscale.md).
#
# It needs the static mke2fs guest/e2fsprogs/build.sh builds, so a C
# toolchain the first time. The image is reproducible: SOURCE_DATE_EPOCH
# sets every time in it, and defaults to the time of the last commit.
set -euo pipefail
shopt -s inherit_errexit

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
# shellcheck source=guest/tailscale/tailscale.env
. "$HERE/tailscale.env"

WORK=${HANGAR_TAILSCALE_WORKDIR:-$REPO/.cache/tailscale}
OUT=${HANGAR_TAILSCALE_OUT:-$REPO/bin/guest}
MKE2FS=${HANGAR_E2FSPROGS_OUT:-$REPO/bin/guest}/mke2fs

log() { printf '%s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }

for cmd in curl sha256sum tar; do
	command -v "$cmd" >/dev/null || die "$cmd not found"
done

if [ -z "${SOURCE_DATE_EPOCH:-}" ]; then
	SOURCE_DATE_EPOCH=$(git -C "$REPO" log -1 --format=%ct)
fi
[[ $SOURCE_DATE_EPOCH =~ ^[0-9]+$ ]] || die "SOURCE_DATE_EPOCH must be seconds since the epoch"
# mke2fs reads it from its environment for the times of what -d copies.
export SOURCE_DATE_EPOCH

"$REPO/guest/e2fsprogs/build.sh"
[ -x "$MKE2FS" ] || die "$MKE2FS: no mke2fs"

TARBALL=$WORK/tailscale_${HANGAR_TAILSCALE_VERSION}_amd64.tgz
mkdir -p "$WORK"
if [ ! -f "$TARBALL" ]; then
	log "download: $HANGAR_TAILSCALE_URL"
	curl -fsSL -o "$TARBALL.part" "$HANGAR_TAILSCALE_URL"
	mv "$TARBALL.part" "$TARBALL"
fi
# Verified on every run, so a corrupt or swapped cache cannot slip through.
if ! echo "$HANGAR_TAILSCALE_SHA256  $TARBALL" | sha256sum --check --status; then
	rm -f "$TARBALL"
	die "sha256 mismatch for $TARBALL (deleted); tailscale.env and pkgs.tailscale.com disagree"
fi

STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT
ROOT=$STAGE/root
mkdir -p "$STAGE/x" "$ROOT/bin"
dir=tailscale_${HANGAR_TAILSCALE_VERSION}_amd64
tar -xzf "$TARBALL" -C "$STAGE/x" "$dir/tailscaled" "$dir/tailscale"
for b in tailscaled tailscale; do
	install -m 0755 "$STAGE/x/$dir/$b" "$ROOT/bin/$b"
done
chmod 0755 "$ROOT" "$ROOT/bin"
find "$ROOT" -exec touch -h -d "@$SOURCE_DATE_EPOCH" {} +

# Whole MiB, with room for ext4's own blocks.
bytes=$(du -sb "$ROOT" | cut -f1)
mib=$(((bytes * 11 / 10 + (8 << 20)) >> 20))
img=$STAGE/tailscale.img
truncate -s "${mib}M" "$img"
# No journal: it is only ever mounted read-only. A fixed UUID and hash seed,
# SOURCE_DATE_EPOCH for the inodes' times and the fake time for the
# filesystem's make the same files the same image.
E2FSPROGS_FAKE_TIME=$SOURCE_DATE_EPOCH "$MKE2FS" -q -F -t ext4 -O ^has_journal -m 0 -b 4096 -N 64 \
	-U 3f1c1a5e-6a8e-4d0c-9a6e-7461696c6e65 -E hash_seed=3f1c1a5e-6a8e-4d0c-9a6e-7461696c6e65,root_owner=0:0 \
	-L tailscale -d "$ROOT" "$img"

mkdir -p "$OUT"
install -m 0644 "$img" "$OUT/tailscale.img.part"
mv "$OUT/tailscale.img.part" "$OUT/tailscale.img"
(cd "$OUT" && sha256sum tailscale.img && ls -l tailscale.img) >&2
