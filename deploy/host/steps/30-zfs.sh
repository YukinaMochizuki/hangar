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

# Each entry is DATASET:MOUNTPOINT:PROPERTIES(comma-separated):MODE, MODE that
# of the mount point, then root's. What each dataset is for:
# docs/runbook/host-bootstrap.md.
zfs_datasets() {
	printf '%s\n' \
		"$HANGAR_ZPOOL/state:/var/lib/hangar/state::" \
		"$HANGAR_ZPOOL/images:/var/lib/hangar/images:setuid=off,exec=off,devices=off,quota=$HANGAR_IMAGES_QUOTA:700" \
		"$HANGAR_ZPOOL/vms:/var/lib/hangar/vms:setuid=off,exec=off,devices=on,quota=$HANGAR_VMS_QUOTA:700" \
		"$HANGAR_ZPOOL/poc:none::"
}

zfs_prop() { zfs get -H -o value "$1" "$2"; }

zfs_props_ok() {
	[ "$(zfs_prop compression "$HANGAR_ZPOOL")" = lz4 ] &&
		[ "$(zfs_prop atime "$HANGAR_ZPOOL")" = off ] &&
		[ "$(zpool get -H -o value autotrim "$HANGAR_ZPOOL")" = on ]
}

# zfs_prop_ok DATASET NAME=VALUE holds when the dataset has that value; a
# quota (NG) is compared in bytes.
zfs_prop_ok() {
	local ds=$1 name=${2%%=*} want=${2#*=}
	if [ "$name" = quota ]; then
		[ "$(zfs get -H -p -o value quota "$ds")" = "$(bytes_of "$want")" ]
	else
		[ "$(zfs_prop "$name" "$ds")" = "$want" ]
	fi
}

zfs_dataset_ok() {
	local ds=$1 mp=$2 props=$3 mode=$4 p
	zfs list -H -o name "$ds" >/dev/null 2>&1 || return 1
	[ "$(zfs_prop mountpoint "$ds")" = "$mp" ] || return 1
	for p in ${props//,/ }; do zfs_prop_ok "$ds" "$p" || return 1; done
	[ "$mp" = none ] && return 0
	[ "$(zfs_prop canmount "$ds")" = on ] && [ "$(zfs_prop mounted "$ds")" = yes ] || return 1
	[ -z "$mode" ] || [ "$(stat -c '%a %U %G' "$mp")" = "$mode root root" ]
}

zfs_datasets_ok() {
	local entry ds mp props mode
	for entry in $(zfs_datasets); do
		IFS=: read -r ds mp props mode <<<"$entry"
		zfs_dataset_ok "$ds" "$mp" "$props" "$mode" || return 1
	done
}

zfs_arc_runtime_ok() { [ "$(cat "$ZFS_ARC_PARAM")" = "$HANGAR_ARC_MAX_BYTES" ]; }

zfs_arc_persistent_ok() {
	grep -qsx "options zfs zfs_arc_max=$HANGAR_ARC_MAX_BYTES" "$ZFS_MODPROBE"
}

# zfs_dataset_apply DATASET MOUNTPOINT PROPERTIES MODE makes the dataset, or
# brings its mount point and properties to what is wanted, then the mode of
# its mount point.
zfs_dataset_apply() {
	local ds=$1 mp=$2 props=$3 mode=$4 p opts=()
	if ! zfs list -H -o name "$ds" >/dev/null 2>&1; then
		for p in ${props//,/ }; do opts+=(-o "$p"); done
		run "create dataset $ds${props:+ with $props}" zfs create -o mountpoint="$mp" "${opts[@]}" "$ds"
	else
		[ "$(zfs_prop mountpoint "$ds")" = "$mp" ] ||
			run "set mountpoint=$mp on $ds" zfs set mountpoint="$mp" "$ds"
		for p in ${props//,/ }; do
			zfs_prop_ok "$ds" "$p" || run "set $p on $ds" zfs set "$p" "$ds"
		done
		if [ "$mp" != none ] && [ "$(zfs_prop mounted "$ds")" != yes ]; then
			run "mount $ds" zfs mount "$ds"
		fi
	fi
	[ -n "$mode" ] || return 0
	# Under --dry-run a dataset not made yet has no mount point; zfs makes it
	# 755 and root's.
	if [ ! -d "$mp" ] || [ "$(stat -c '%a %U %G' "$mp")" != "$mode root root" ]; then
		run "set $mp to $mode, root's" sh -c "chown root:root '$mp' && chmod $mode '$mp'"
	fi
}

zfs_apply() {
	local entry ds mp props mode
	zpool list -H -o name "$HANGAR_ZPOOL" >/dev/null 2>&1 ||
		die "pool $HANGAR_ZPOOL does not exist; the OS installer is expected to create it"

	[ "$(zfs_prop compression "$HANGAR_ZPOOL")" = lz4 ] ||
		run "set compression=lz4 on $HANGAR_ZPOOL" zfs set compression=lz4 "$HANGAR_ZPOOL"
	[ "$(zfs_prop atime "$HANGAR_ZPOOL")" = off ] ||
		run "set atime=off on $HANGAR_ZPOOL" zfs set atime=off "$HANGAR_ZPOOL"
	[ "$(zpool get -H -o value autotrim "$HANGAR_ZPOOL")" = on ] ||
		run "set autotrim=on on $HANGAR_ZPOOL" zpool set autotrim=on "$HANGAR_ZPOOL"

	for entry in $(zfs_datasets); do
		IFS=: read -r ds mp props mode <<<"$entry"
		zfs_dataset_apply "$ds" "$mp" "$props" "$mode"
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
	local entry ds mp props mode
	for entry in $(zfs_datasets); do
		IFS=: read -r ds mp props mode <<<"$entry"
		check "dataset $ds at $mp${props:+, $props}${mode:+, mode $mode owned by root}" \
			zfs_dataset_ok "$ds" "$mp" "$props" "$mode"
	done
	check "zfs-mount.service mounts datasets at boot" systemctl is-enabled --quiet zfs-mount.service
	check "ARC max $HANGAR_ARC_MAX_BYTES at runtime" zfs_arc_runtime_ok
	check "ARC max $HANGAR_ARC_MAX_BYTES across reboots" zfs_arc_persistent_ok
	check "udev neither probes nor watches zvols" zfs_udev_persisted
	check "no zvol carries a probed filesystem" zfs_udev_unprobed
}
