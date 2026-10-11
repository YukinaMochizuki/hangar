# Mirrors swap across both disks with md RAID1, so losing a disk cannot take
# swapped-out pages with it. Swap stays off ZFS: swapping to a zvol can deadlock
# under the memory pressure that makes swap necessary.

SWAP_WIPE_MARKER=$STATE_DIR/swap-wipe-started
RESUME_CONF=/etc/initramfs-tools/conf.d/resume

swap_md() { printf '/dev/md/%s' "$HANGAR_SWAP_MD_NAME"; }

swap_md_uuid() {
	local uuid
	uuid=$(blkid -p -o value -s UUID "$(swap_md)")
	[ -n "$uuid" ] || die "$(swap_md) has no swap UUID"
	printf '%s' "$uuid"
}

swap_active() { awk 'NR > 1 {print $1}' /proc/swaps | grep -qx "$(readlink -f "$1")"; }

# The installer leaves a plain swap on each partition. Anything else means the
# env file points at the wrong partition, and wiping it would destroy data. A
# blank partition is accepted only when an earlier run recorded wiping it.
swap_check_parts() {
	local p t
	for p in $HANGAR_SWAP_PARTS; do
		[ -b "$p" ] || die "$p is not a block device"
		t=$(blkid -p -o value -s TYPE "$p" || true)
		[ "$t" = swap ] && continue
		if [ -z "$t" ] && grep -qx "$p" "$SWAP_WIPE_MARKER" 2>/dev/null; then
			continue
		fi
		if [ "$t" = linux_raid_member ]; then
			die "$p is already an md member but $(swap_md) is missing; assemble it by hand (mdadm --assemble $(swap_md) $HANGAR_SWAP_PARTS)"
		fi
		die "$p holds '${t:-nothing}', not swap; refusing to wipe it"
	done
}

swap_create_array() {
	local p tool
	# shellcheck disable=SC2086
	set -- $HANGAR_SWAP_PARTS
	[ $# = 2 ] || die "HANGAR_SWAP_PARTS must name exactly two partitions"
	for tool in mdadm mkswap wipefs swapoff; do
		command -v "$tool" >/dev/null || die "$tool is not installed; run the packages step first"
	done
	mkdir -p "$STATE_DIR"
	printf '%s\n' "$@" >"$SWAP_WIPE_MARKER"
	for p in "$@"; do
		if swap_active "$p"; then swapoff "$p"; fi
		wipefs -q -a "$p"
	done
	mdadm --create "$(swap_md)" --run --level=1 --raid-devices=2 \
		--metadata=1.2 --name="$HANGAR_SWAP_MD_NAME" "$@"
	udevadm settle
	mkswap -L hangar-swap "$(swap_md)"
	rm -f "$SWAP_WIPE_MARKER"
}

# swap_fstab prints /etc/fstab with every swap entry replaced by one for the array.
swap_fstab() {
	local uuid
	uuid=$(swap_md_uuid)
	awk '$1 !~ /^#/ && $3 == "swap" {next} {print}' /etc/fstab
	printf 'UUID=%s none swap sw 0 0\n' "$uuid"
}

swap_md_brief() {
	local line
	line=$(mdadm --detail --brief "$(swap_md)")
	case $line in ARRAY*UUID=*) printf '%s' "$line" ;; *) die "unexpected mdadm output: $line" ;; esac
}

swap_mdadm_conf() {
	local line
	line=$(swap_md_brief)
	cat /etc/mdadm/mdadm.conf
	printf '%s\n' "$line"
}

swap_conf_has_array() {
	local uuid
	uuid=$(swap_md_brief | grep -o 'UUID=[^ ]*')
	grep -q "^ARRAY .*$uuid" /etc/mdadm/mdadm.conf
}

# The host never hibernates. Without this, the initramfs waits at boot for a
# resume device, which after the swap rebuild may no longer exist.
swap_resume_conf() { printf 'RESUME=none'; }

# swap_cmdline_resume prints resume= arguments on the running kernel's command
# line or in the GRUB defaults; any of them can stall boot once swap moves.
swap_cmdline_resume() {
	{
		cat /proc/cmdline
		cat /etc/default/grub /etc/default/grub.d/*.cfg 2>/dev/null || true
	} | grep -o 'resume=[^ "]*' || true
}

swap_raid_apply() {
	local before content
	pkg_installed initramfs-tools ||
		die "this step only knows initramfs-tools; inspect the initramfs generator by hand"
	[ -z "$(swap_cmdline_resume)" ] ||
		die "kernel command line or GRUB sets $(swap_cmdline_resume); remove it before moving swap"

	if [ ! -e "$(swap_md)" ]; then
		swap_check_parts
		run "build $(swap_md) from $HANGAR_SWAP_PARTS" swap_create_array
		if [ "$DRY_RUN" = 1 ]; then
			log "would: point /etc/fstab, mdadm.conf and the resume config at $(swap_md) and swapon"
			need_initramfs
			return 0
		fi
	fi
	[ "$(blkid -p -o value -s TYPE "$(swap_md)" || true)" = swap ] ||
		die "$(swap_md) exists but does not hold swap; inspect it by hand"

	before=$CHANGES
	content=$(swap_fstab)
	put_file /etc/fstab 644 "$content"
	[ "$CHANGES" = "$before" ] || run "reload systemd units from fstab" systemctl daemon-reload

	if ! swap_conf_has_array; then
		content=$(swap_mdadm_conf)
		need_initramfs
		put_file /etc/mdadm/mdadm.conf 644 "$content"
	fi

	if [ "$(cat "$RESUME_CONF" 2>/dev/null)" != "$(swap_resume_conf)" ]; then
		need_initramfs
		put_file "$RESUME_CONF" 644 "$(swap_resume_conf)"
	fi

	swap_active "$(swap_md)" || run "swapon $(swap_md)" swapon "$(swap_md)"
}

swap_only_md_active() {
	[ "$(awk 'NR > 1' /proc/swaps | wc -l)" = 1 ] && swap_active "$(swap_md)"
}

swap_md_mirrored() {
	local detail p
	detail=$(mdadm --detail "$(swap_md)")
	grep -q 'Raid Level : raid1' <<<"$detail" || return 1
	grep -q 'Active Devices : 2' <<<"$detail" || return 1
	for p in $HANGAR_SWAP_PARTS; do
		grep -q "active sync *$(readlink -f "$p")\$" <<<"$detail" || return 1
	done
}

swap_fstab_single_entry() {
	[ "$(awk '$1 !~ /^#/ && $3 == "swap"' /etc/fstab | wc -l)" = 1 ] &&
		grep -q "^UUID=$(swap_md_uuid) .* swap " /etc/fstab
}

swap_no_resume_device() {
	[ "$(cat "$RESUME_CONF")" = "$(swap_resume_conf)" ] && [ -z "$(swap_cmdline_resume)" ]
}

swap_raid_verify() {
	check "swap array $(swap_md) is a RAID1 of $HANGAR_SWAP_PARTS" swap_md_mirrored
	check "swap is active only on $(swap_md)" swap_only_md_active
	check "fstab has one swap entry, the array" swap_fstab_single_entry
	check "mdadm.conf lists the swap array" swap_conf_has_array
	check "boot waits for no resume device" swap_no_resume_device
}
