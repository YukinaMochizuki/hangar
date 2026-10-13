#!/usr/bin/env bash
# Gives a disposable testbed VM what the disk tests (test/testbed/disks) need,
# as a host has it: qemu-img, a ZFS pool named hangar, on a sparse file here,
# the dataset for images, mounted where the node keeps them and with a quota,
# and the udev rules deploy/host installs for zvols. Idempotent.
#
# Run as root on a throwaway VM only.
set -euo pipefail

repo=$(cd "$(dirname "$0")/../.." && pwd)
pool=hangar
pool_file=${HANGAR_TESTBED_POOL_FILE:-/var/tmp/hangar-testbed-pool.img}
pool_size=${HANGAR_TESTBED_POOL_SIZE:-24G}
images_quota=${HANGAR_TESTBED_IMAGES_QUOTA:-8G}
images_mount=/var/lib/hangar/images

fail() { echo "FAIL: $*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || fail "must run as root"
command -v zpool >/dev/null || fail "zfsutils-linux is not installed"
command -v mkfs.ext4 >/dev/null || fail "e2fsprogs is not installed"
# The disk image driver's, as deploy/host's packages step installs it.
if ! command -v qemu-img >/dev/null; then
	apt-get update -q && DEBIAN_FRONTEND=noninteractive apt-get install -qy --no-install-recommends qemu-utils
fi
modprobe zfs

if ! zpool list -H -o name "$pool" >/dev/null 2>&1; then
	[ -e "$pool_file" ] || truncate -s "$pool_size" "$pool_file"
	# As deploy/host leaves the pool: lz4, no atime, never mounted itself.
	zpool create -o ashift=12 -o autotrim=on -O compression=lz4 -O atime=off \
		-O mountpoint=none "$pool" "$pool_file"
	echo "created pool $pool on $pool_file ($pool_size)"
fi

# Unpacked images hold tenants' setuid binaries and device-free trees; the
# host never runs or opens them as such, and they never take the pool's room.
if ! zfs list -H -o name "$pool/images" >/dev/null 2>&1; then
	zfs create -o mountpoint="$images_mount" -o setuid=off -o exec=off -o devices=off "$pool/images"
	echo "created $pool/images at $images_mount"
fi
zfs set quota="$images_quota" "$pool/images"
chmod 700 "$images_mount"

# The rules as deploy/host's zfs step writes them.
# shellcheck source=deploy/host/steps/30-zfs.sh
. "$repo/deploy/host/steps/30-zfs.sh"
zfs_udev_noprobe >"$ZFS_UDEV_NOPROBE"
zfs_udev_nowatch >"$ZFS_UDEV_NOWATCH"
udevadm control --reload
udevadm trigger --action=change --subsystem-match=block --sysname-match='zd*' --settle

zfs list -o name,used,avail,quota,mountpoint,setuid,exec,devices -r "$pool"
