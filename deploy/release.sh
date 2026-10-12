#!/usr/bin/env bash
# Builds what a host runs, from the checked-out commit, into one directory:
# hangar-apiserver, hangar-node and hangar (static), the guest kernel and the
# platform initramfs, VERSION, and SHA256SUMS over all of them. The `release`
# step of deploy/host installs from that directory (docs/runbook/host-bootstrap.md).
#
#   release.sh [--allow-dirty] [OUT]
#
# OUT defaults to bin/release/hangar-<first 12 of the revision>. The build is
# reproducible: the same commit gives the same SHA256SUMS on the same
# toolchain. A tree with uncommitted changes is refused unless --allow-dirty,
# and its VERSION then ends in -dirty, as the binaries' own version does.
set -euo pipefail
shopt -s inherit_errexit

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/.." && pwd)

log() { printf '%s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }
usage() { echo "usage: $0 [--allow-dirty] [OUT]" >&2; exit 2; }

allow_dirty=0
out=
while [ $# -gt 0 ]; do
	case $1 in
	--allow-dirty) allow_dirty=1; shift ;;
	-*) usage ;;
	*) [ -z "$out" ] || usage; out=$1; shift ;;
	esac
done

cd "$REPO"
version=$(git rev-parse HEAD)
if [ -n "$(git status --porcelain)" ]; then
	[ "$allow_dirty" = 1 ] || die "the tree has uncommitted changes; commit them, or pass --allow-dirty for a test build"
	version=$version-dirty
fi
short=${version:0:12}
case $version in *-dirty) short=$short-dirty ;; esac
out=${out:-$REPO/bin/release/hangar-$short}

# Everything is staged first, so a failed build never leaves a directory
# that looks like a release.
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT

log "release $version"
for cmd in hangar-apiserver hangar-node hangar; do
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags=-buildid= \
		-o "$stage/$cmd" "./cmd/$cmd"
done
HANGAR_INITRAMFS_OUT=$stage/guest guest/initramfs/build.sh
HANGAR_KERNEL_OUT=$stage/guest guest/kernel/build.sh
mv "$stage/guest/vmlinux" "$stage/guest/initramfs.cpio" "$stage/"
rm -rf "$stage/guest"
printf '%s\n' "$version" >"$stage/VERSION"
(cd "$stage" && sha256sum -- * >SHA256SUMS.part && mv SHA256SUMS.part SHA256SUMS)

rm -rf "$out"
mkdir -p "$(dirname "$out")"
cp -r "$stage" "$out.part"
chmod 0755 "$out.part"
mv "$out.part" "$out"
log "wrote $out"
cat "$out/SHA256SUMS" >&2
