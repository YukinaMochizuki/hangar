#!/usr/bin/env bash
# Builds the platform initramfs every machine boots with: hangar-init as /init,
# the static mke2fs that formats a container's blank volumes as /sbin/mke2fs
# (guest/e2fsprogs/build.sh, built first), and the empty mount points it
# needs, as an uncompressed, reproducible newc cpio in
# bin/guest/initramfs.cpio.
#
#   build.sh                  build ./cmd/hangar-init
#   build.sh --main DIR       build the main package in DIR instead (any module)
#
# SOURCE_DATE_EPOCH sets every entry's mtime; it defaults to the time of the
# last commit. docs/runbook/guest-kernel.md covers how to boot the result.
set -euo pipefail
shopt -s inherit_errexit

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
OUT=${HANGAR_INITRAMFS_OUT:-$REPO/bin/guest}
MAIN=$REPO/cmd/hangar-init

usage() { echo "usage: $0 [--main DIR]" >&2; exit 2; }
while [ $# -gt 0 ]; do
	case $1 in
	--main) [ $# -ge 2 ] || usage; MAIN=$2; shift 2 ;;
	*) usage ;;
	esac
done

die() { echo "error: $*" >&2; exit 1; }
[ -d "$MAIN" ] || die "$MAIN: no such directory"
MAIN=$(cd "$MAIN" && pwd)

if [ -z "${SOURCE_DATE_EPOCH:-}" ]; then
	SOURCE_DATE_EPOCH=$(git -C "$REPO" log -1 --format=%ct)
fi
[[ $SOURCE_DATE_EPOCH =~ ^[0-9]+$ ]] || die "SOURCE_DATE_EPOCH must be seconds since the epoch"

STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT
ROOT=$STAGE/root
mkdir -m 0755 "$ROOT"

# Static, so /init needs nothing from the image it is about to mount. The VCS
# stamp stays: it is the version the guest agent reports
# (internal/buildinfo), and it is the same for every build of one tree.
(cd "$MAIN" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
	-ldflags=-buildid= -o "$STAGE/init" .)
install -m 0755 "$STAGE/init" "$ROOT/init"
"$REPO/guest/e2fsprogs/build.sh"
mkdir -m 0755 "$ROOT/sbin"
install -m 0755 "${HANGAR_E2FSPROGS_OUT:-$REPO/bin/guest}/mke2fs" "$ROOT/sbin/mke2fs"
# Mount points only: hangar-init mounts devtmpfs on /dev, so no device nodes.
mkdir -m 0755 "$ROOT/dev" "$ROOT/proc" "$ROOT/sys" "$ROOT/newroot"

mkdir -p "$OUT"
(cd "$REPO" && go run ./tools/newc -root "$ROOT" -mtime "$SOURCE_DATE_EPOCH" -o "$OUT/initramfs.cpio.part")
mv "$OUT/initramfs.cpio.part" "$OUT/initramfs.cpio"
(cd "$OUT" && sha256sum initramfs.cpio && ls -l initramfs.cpio) >&2
