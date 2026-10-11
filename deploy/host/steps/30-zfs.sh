# Properties on the pool root (inherited by every dataset), the ARC cap, and the
# datasets the host itself owns. The installer creates the pool; this step never
# creates or destroys one.

ZFS_MODPROBE=/etc/modprobe.d/zfs-hangar.conf
ZFS_ARC_PARAM=/sys/module/zfs/parameters/zfs_arc_max

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
}

zfs_verify() {
	check "pool $HANGAR_ZPOOL: compression=lz4 atime=off autotrim=on" zfs_props_ok
	check "datasets $(zfs_datasets | tr '\n' ' ')" zfs_datasets_ok
	check "ARC max $HANGAR_ARC_MAX_BYTES at runtime" zfs_arc_runtime_ok
	check "ARC max $HANGAR_ARC_MAX_BYTES across reboots" zfs_arc_persistent_ok
}
