#!/usr/bin/env bash
# Gives a disposable testbed VM what the disk tests (test/testbed/disks) need,
# as a host has it: qemu-img, a ZFS pool named hangar, on a sparse file here
# behind a direct-IO loop device, the dataset for images, mounted where the
# node keeps them and with a quota, and the udev rules deploy/host installs for
# zvols. Idempotent, and after a reboot it attaches the file and imports the
# pool again.
#
# Not a file vdev: its writes go through the page cache and can deadlock zvol
# writeback (docs/runbook/testbed.md).
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
# The VM tests' own cloud-init seeds, for a VM not pointed at the platform's.
if ! command -v mkfs.vfat >/dev/null; then
	apt-get update -q && DEBIAN_FRONTEND=noninteractive apt-get install -qy --no-install-recommends dosfstools
fi
modprobe zfs

# A pool made before the loop device, on the file itself, is moved onto one.
if zpool list -H -o name "$pool" >/dev/null 2>&1 &&
	zpool list -H -P -v "$pool" | awk -v f="$pool_file" '$1 == f { found = 1 } END { exit !found }'; then
	zpool export "$pool"
	echo "exported pool $pool, which was on $pool_file itself"
fi

# A pool destroyed with its file removed (hangar-testbed-reset --pool) leaves
# the loop device holding the deleted file, and its room.
losetup -n -l -O NAME,BACK-FILE | awk -v f="$pool_file (deleted)" '{ n = $1; sub(/^[^ ]+ +/, "") } $0 == f { print n }' |
	while read -r stale; do
		losetup -d "$stale"
		echo "detached $stale from the deleted $pool_file"
	done

new_file=0
if [ ! -e "$pool_file" ]; then
	truncate -s "$pool_size" "$pool_file"
	new_file=1
fi
loop=$(losetup -n -O NAME -j "$pool_file" | head -n 1)
if [ -z "$loop" ]; then
	loop=$(losetup --find --show --direct-io=on "$pool_file")
	echo "attached $pool_file to $loop"
else
	losetup --direct-io=on "$loop"
fi
# losetup falls back to buffered IO when the file cannot take direct IO.
[ "$(losetup -n -O DIO "$loop" | tr -d ' ')" = 1 ] || fail "$loop is not in direct-IO mode"

if ! zpool list -H -o name "$pool" >/dev/null 2>&1; then
	if [ "$new_file" = 1 ]; then
		# As deploy/host leaves the pool: lz4, no atime, never mounted itself.
		zpool create -o ashift=12 -o autotrim=on -O compression=lz4 -O atime=off \
			-O mountpoint=none "$pool" "$loop"
		echo "created pool $pool on $loop ($pool_file, $pool_size)"
	else
		zpool import -d "$loop" "$pool"
		echo "imported pool $pool from $loop ($pool_file)"
	fi
fi
zpool list -H -P -v "$pool" | awk -v l="$loop" '$1 == l { found = 1 } END { exit !found }' ||
	fail "pool $pool is not on $loop"

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
