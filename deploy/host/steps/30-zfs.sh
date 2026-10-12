# Properties on the pool root (inherited by every dataset), the ARC cap, and the
# datasets the host itself owns. The installer creates the pool; this step never
# creates or destroys one.

ZFS_MODPROBE=/etc/modprobe.d/zfs-hangar.conf
ZFS_ARC_PARAM=/sys/module/zfs/parameters/zfs_arc_max

# Zvols hold tenants' filesystems, which the host never parses: udev must not
# probe them (blkid, before 60-persistent-storage) nor watch them for writes
# (after 60-block, which turns watching on). docs/decisions/storage-zfs.md.
ZFS_UDEV_NOPROBE=/etc/udev/rules.d/59-hangar-zvol.rules
ZFS_UDEV_NOWATCH=/etc/udev/rules.d/99-hangar-zvol.rules

zfs_udev_noprobe() {
	printf '%s\n' '# Written by deploy/host/steps/30-zfs.sh.' \
		'KERNEL=="zd*", ENV{UDEV_DISABLE_PERSISTENT_STORAGE_BLKID_FLAG}="1"'
}

zfs_udev_nowatch() {
	printf '%s\n' '# Written by deploy/host/steps/30-zfs.sh.' \
		'KERNEL=="zd*", OPTIONS+="nowatch"'
}

zfs_udev_persisted() {
	zfs_udev_noprobe | cmp -s - "$ZFS_UDEV_NOPROBE" && zfs_udev_nowatch | cmp -s - "$ZFS_UDEV_NOWATCH" &&
		[ "$(stat -c %a "$ZFS_UDEV_NOPROBE")" = 644 ] && [ "$(stat -c %a "$ZFS_UDEV_NOWATCH")" = 644 ]
}

# zfs_udev_unprobed holds when no zvol carries what blkid would have read.
zfs_udev_unprobed() {
	local dev
	for dev in /dev/zd*; do
		[ -b "$dev" ] || continue
		udevadm info --query=property --name="$dev" | grep -q '^ID_FS_' && return 1
	done
	return 0
}

# Each entry is DATASET:MOUNTPOINT. hangar/poc holds throwaway experiments and is
# never mounted; volumes under it are reached as /dev/zvol paths.
zfs_datasets() {
	printf '%s\n' \
		"$HANGAR_ZPOOL/state:/var/lib/hangar/state" \
		"$HANGAR_ZPOOL/poc:none"
}

zfs_prop() { zfs get -H -o value "$1" "$2"; }

zfs_props_ok() {
	[ "$(zfs_prop compression "$HANGAR_ZPOOL")" = lz4 ] &&
		[ "$(zfs_prop atime "$HANGAR_ZPOOL")" = off ] &&
		[ "$(zpool get -H -o value autotrim "$HANGAR_ZPOOL")" = on ]
}

zfs_datasets_ok() {
	local entry ds mp
	for entry in $(zfs_datasets); do
		ds=${entry%%:*} mp=${entry#*:}
		zfs list -H -o name "$ds" >/dev/null 2>&1 || return 1
		[ "$(zfs_prop mountpoint "$ds")" = "$mp" ] || return 1
	done
}

zfs_arc_runtime_ok() { [ "$(cat "$ZFS_ARC_PARAM")" = "$HANGAR_ARC_MAX_BYTES" ]; }

zfs_arc_persistent_ok() {
	grep -qsx "options zfs zfs_arc_max=$HANGAR_ARC_MAX_BYTES" "$ZFS_MODPROBE"
}

zfs_apply() {
	local entry ds mp
	zpool list -H -o name "$HANGAR_ZPOOL" >/dev/null 2>&1 ||
		die "pool $HANGAR_ZPOOL does not exist; the OS installer is expected to create it"

	[ "$(zfs_prop compression "$HANGAR_ZPOOL")" = lz4 ] ||
		run "set compression=lz4 on $HANGAR_ZPOOL" zfs set compression=lz4 "$HANGAR_ZPOOL"
	[ "$(zfs_prop atime "$HANGAR_ZPOOL")" = off ] ||
		run "set atime=off on $HANGAR_ZPOOL" zfs set atime=off "$HANGAR_ZPOOL"
	[ "$(zpool get -H -o value autotrim "$HANGAR_ZPOOL")" = on ] ||
		run "set autotrim=on on $HANGAR_ZPOOL" zpool set autotrim=on "$HANGAR_ZPOOL"

	for entry in $(zfs_datasets); do
		ds=${entry%%:*} mp=${entry#*:}
		if ! zfs list -H -o name "$ds" >/dev/null 2>&1; then
			run "create dataset $ds" zfs create -o mountpoint="$mp" "$ds"
		elif [ "$(zfs_prop mountpoint "$ds")" != "$mp" ]; then
			run "set mountpoint=$mp on $ds" zfs set mountpoint="$mp" "$ds"
		fi
	done

	if ! zfs_arc_persistent_ok; then
		if pkg_installed zfs-initramfs; then need_initramfs; fi
		put_file "$ZFS_MODPROBE" 644 "options zfs zfs_arc_max=$HANGAR_ARC_MAX_BYTES"
	fi
	zfs_arc_runtime_ok ||
		run "set zfs_arc_max=$HANGAR_ARC_MAX_BYTES now" sh -c "echo $HANGAR_ARC_MAX_BYTES >$ZFS_ARC_PARAM"

	if ! zfs_udev_persisted; then
		put_file "$ZFS_UDEV_NOPROBE" 644 "$(zfs_udev_noprobe)"
		put_file "$ZFS_UDEV_NOWATCH" 644 "$(zfs_udev_nowatch)"
		run "reload udev rules" udevadm control --reload
	fi
	# Zvols probed before the rules keep what blkid read until an event.
	zfs_udev_unprobed ||
		run "forget what udev probed on zvols" udevadm trigger --action=change --subsystem-match=block --sysname-match='zd*' --settle
}

zfs_verify() {
	check "pool $HANGAR_ZPOOL: compression=lz4 atime=off autotrim=on" zfs_props_ok
	check "datasets $(zfs_datasets | tr '\n' ' ')" zfs_datasets_ok
	check "ARC max $HANGAR_ARC_MAX_BYTES at runtime" zfs_arc_runtime_ok
	check "ARC max $HANGAR_ARC_MAX_BYTES across reboots" zfs_arc_persistent_ok
	check "udev neither probes nor watches zvols" zfs_udev_persisted
	check "no zvol carries a probed filesystem" zfs_udev_unprobed
}
