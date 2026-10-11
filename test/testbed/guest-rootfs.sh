#!/usr/bin/env bash
# Makes an ext4 root disk image from an OCI image, for the guest tests in
# test/testbed/guest (make e2e-host). Run as root, with Go on PATH: it
# loop-mounts the image to unpack the layers with their owners. Under sudo,
# the image is handed to the invoking user, so make can replace it later.
#
#   guest-rootfs.sh IMAGE OUT [SIZE]
set -euo pipefail
shopt -s inherit_errexit

[ $# -ge 2 ] || { echo "usage: $0 IMAGE OUT [SIZE]" >&2; exit 2; }
image=$1 out=$2 size=${3:-4G}
repo=$(cd "$(dirname "$0")/../.." && pwd)

[ "$(id -u)" = 0 ] || { echo "error: must run as root" >&2; exit 1; }

mkdir -p "$(dirname "$out")"
tmp=$out.part
mnt=$(mktemp -d)
trap 'umount "$mnt" 2>/dev/null || true; rmdir "$mnt"; rm -f "$tmp"' EXIT

rm -f "$tmp"
truncate -s "$size" "$tmp"
mkfs.ext4 -q -F -L root "$tmp"
mount -o loop "$tmp" "$mnt"
(cd "$repo" && go tool -modfile=tools/tools.mod crane export --platform linux/amd64 "$image" -) |
	tar -xpf - --numeric-owner --xattrs --xattrs-include='*' -C "$mnt"
umount "$mnt"
chown "${SUDO_UID:-0}:${SUDO_GID:-0}" "$tmp"
mv "$tmp" "$out"
echo "wrote $out from $image" >&2
