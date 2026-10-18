#!/usr/bin/env bash
# Builds what a host runs, from the checked-out commit, into one directory:
# hangar-apiserver, hangar-node, hangar-edge and hangar (static), the guest
# kernel and the platform initramfs, the console's files as console.tar,
# deploy/ itself as deploy.tar, VERSION, and SHA256SUMS over all of them. The
# `release` step of deploy/host installs from that directory, and
# .github/workflows/release.yml publishes it for a tag
# (docs/runbook/release.md, docs/runbook/host-bootstrap.md).
#
#   release.sh [--allow-dirty] [OUT]
#
# OUT defaults to bin/release/hangar-<first 12 of the revision>. The build is
# reproducible: the same commit gives the same SHA256SUMS on the same
# toolchain. A tree with uncommitted changes is refused unless --allow-dirty,
# and its VERSION then ends in -dirty, as the binaries' own version does.
#
# HANGAR_KERNEL_CACHE=DIR reuses a prebuilt kernel from DIR/<key>, keyed by
# guest/kernel/cache.sh, and files a kernel it builds there. The cached
# config is checked against guest/kernel/config before it is used.
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

if [ -n "${HANGAR_KERNEL_CACHE:-}" ]; then
	HANGAR_KERNEL_CACHE=$(realpath -m -- "$HANGAR_KERNEL_CACHE")
fi
cd "$REPO"
version=$(git rev-parse HEAD)
if [ -n "$(git status --porcelain)" ]; then
	[ "$allow_dirty" = 1 ] || die "the tree has uncommitted changes; commit them, or pass --allow-dirty for a test build"
	version=$version-dirty
fi
short=${version:0:12}
case $version in *-dirty) short=$short-dirty ;; esac
out=${out:-$REPO/bin/release/hangar-$short}
# OUT is replaced, so it must be a release this script wrote before.
if [ -e "$out" ] && ! { [ -f "$out/VERSION" ] && [ -f "$out/SHA256SUMS" ]; }; then
	die "$out exists and is not a release (no VERSION and SHA256SUMS); refusing to replace it"
fi

# Everything is staged first, so a failed build never leaves a directory
# that looks like a release.
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT

log "release $version"
for cmd in hangar-apiserver hangar-node hangar-edge hangar; do
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags=-buildid= \
		-o "$stage/$cmd" "./cmd/$cmd"
done
# The console, built from the lockfile, packed with no time, owner or umask
# of this machine in it, so the same commit packs the same bytes.
pnpm install --frozen-lockfile
pnpm --dir web build
tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner --mode=u+rwX,go+rX,go-w \
	-C web/dist -cf "$stage/console.tar" .
# deploy/ the same way, its files as git knows them (an untracked one too in a
# dirty test build), so nothing ignored ships, sorted here because tar keeps
# the order of a file list.
git -C deploy ls-files -z --cached --others --exclude-standard |
	while IFS= read -r -d '' f; do [ ! -e "deploy/$f" ] || printf '%s\0' "$f"; done |
	LC_ALL=C sort -z >"$stage/deploy.list"
tar --mtime=@0 --owner=0 --group=0 --numeric-owner --mode=u+rwX,go+rX,go-w \
	-C deploy --null --no-recursion -T "$stage/deploy.list" -cf "$stage/deploy.tar"
rm "$stage/deploy.list"
HANGAR_INITRAMFS_OUT=$stage/guest guest/initramfs/build.sh
if [ -z "${HANGAR_KERNEL_CACHE:-}" ]; then
	HANGAR_KERNEL_OUT=$stage/guest guest/kernel/build.sh
else
	key=$(guest/kernel/cache.sh key)
	entry=$HANGAR_KERNEL_CACHE/$key
	if [ -e "$entry" ]; then
		guest/kernel/cache.sh check "$entry"
		cp "$entry/vmlinux" "$entry/config" "$stage/guest/"
		log "kernel: cache hit $key"
	else
		HANGAR_KERNEL_OUT=$stage/guest guest/kernel/build.sh
		# Filed under a temporary name first, so a reader never sees half an entry.
		mkdir -p "$HANGAR_KERNEL_CACHE"
		part=$(mktemp -d "$HANGAR_KERNEL_CACHE/.$key.XXXXXX")
		cp "$stage/guest/vmlinux" "$stage/guest/config" "$part/"
		chmod 0755 "$part"
		mv -T "$part" "$entry" 2>/dev/null || rm -rf "$part"
		log "kernel: built, cached under $key"
	fi
fi
mv "$stage/guest/vmlinux" "$stage/guest/initramfs.cpio" "$stage/"
rm -rf "$stage/guest"
printf '%s\n' "$version" >"$stage/VERSION"
(cd "$stage" && sha256sum -- * >SHA256SUMS.part && mv SHA256SUMS.part SHA256SUMS)

mkdir -p "$(dirname "$out")"
rm -rf "$out.part"
cp -r "$stage" "$out.part"
chmod 0755 "$out.part"
rm -rf "$out"
mv "$out.part" "$out"
log "wrote $out"
cat "$out/SHA256SUMS" >&2
# Compared on the host before installing (docs/runbook/host-bootstrap.md).
log "sha256sum SHA256SUMS: $(cd "$out" && sha256sum SHA256SUMS)"
