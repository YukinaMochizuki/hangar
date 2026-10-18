# Host packages, the pinned Cloud Hypervisor binary, the pinned firmware
# VirtualMachines boot through, and the pinned GitHub CLI hangar-fetch-release
# verifies releases with.

CH_BIN=/usr/local/bin/cloud-hypervisor
# The path deploy/host/fetch-release.sh runs, which it names itself, as it
# runs without this file.
GH_BIN=/usr/local/bin/gh
# hangar-node's --vm-firmware default. Every VM's unit binds it read-only,
# and Cloud Hypervisor, as the VM's own user, reads it.
FW_FILE=/usr/local/lib/hangar/CLOUDHV.fd

missing_packages() {
	local p
	for p in $HANGAR_PACKAGES; do
		pkg_installed "$p" || printf '%s ' "$p"
	done
}

ch_is_pinned() {
	[ -x "$CH_BIN" ] && echo "$HANGAR_CH_SHA256  $CH_BIN" | sha256sum -c --quiet -
}

install_ch() {
	local tmp
	tmp=$(mktemp)
	curl -fsSL -o "$tmp" "$HANGAR_CH_URL"
	if ! echo "$HANGAR_CH_SHA256  $tmp" | sha256sum -c --quiet -; then
		rm -f "$tmp"
		die "checksum mismatch for $HANGAR_CH_URL"
	fi
	install -m 0755 "$tmp" "$CH_BIN"
	rm -f "$tmp"
}

# qemu_img_recent says the installed qemu-img is HANGAR_QEMU_IMG_MIN or
# newer.
qemu_img_recent() {
	local v
	v=$(qemu-img --version 2>/dev/null | sed -n '1s/^qemu-img version \([0-9][0-9.]*\).*/\1/p')
	[ -n "$v" ] && [ "$(printf '%s\n%s\n' "$HANGAR_QEMU_IMG_MIN" "$v" | sort -V | head -n1)" = "$HANGAR_QEMU_IMG_MIN" ]
}

fw_is_pinned() {
	[ -f "$FW_FILE" ] && echo "$HANGAR_FW_SHA256  $FW_FILE" | sha256sum -c --quiet -
}

install_fw() {
	local tmp
	tmp=$(mktemp)
	curl -fsSL -o "$tmp" "$HANGAR_FW_URL"
	if ! echo "$HANGAR_FW_SHA256  $tmp" | sha256sum -c --quiet -; then
		rm -f "$tmp"
		die "checksum mismatch for $HANGAR_FW_URL"
	fi
	install -D -m 0644 "$tmp" "$FW_FILE"
	rm -f "$tmp"
}

gh_is_pinned() {
	[ -x "$GH_BIN" ] && echo "$HANGAR_GH_BIN_SHA256  $GH_BIN" | sha256sum -c --quiet -
}

install_gh() {
	local tmp
	tmp=$(mktemp -d)
	curl -fsSL -o "$tmp/gh.tar.gz" "$HANGAR_GH_URL"
	if ! echo "$HANGAR_GH_SHA256  $tmp/gh.tar.gz" | sha256sum -c --quiet -; then
		rm -rf "$tmp"
		die "checksum mismatch for $HANGAR_GH_URL"
	fi
	tar -xzf "$tmp/gh.tar.gz" -C "$tmp" --strip-components=2 "gh_${HANGAR_GH_VERSION}_linux_amd64/bin/gh"
	if ! echo "$HANGAR_GH_BIN_SHA256  $tmp/gh" | sha256sum -c --quiet -; then
		rm -rf "$tmp"
		die "the gh binary in $HANGAR_GH_URL does not match HANGAR_GH_BIN_SHA256"
	fi
	install -m 0755 "$tmp/gh" "$GH_BIN.hangar-new"
	mv -f "$GH_BIN.hangar-new" "$GH_BIN"
	rm -rf "$tmp"
}

packages_apply() {
	local missing
	missing=$(missing_packages)
	if [ -n "$missing" ]; then
		run "install packages: $missing" sh -c \
			"apt-get update -q && DEBIAN_FRONTEND=noninteractive apt-get install -qy --no-install-recommends $missing"
	fi
	ch_is_pinned || run "install cloud-hypervisor $HANGAR_CH_VERSION" install_ch
	fw_is_pinned || run "install the EDK2 firmware $HANGAR_FW_VERSION" install_fw
	gh_is_pinned || run "install gh $HANGAR_GH_VERSION" install_gh
}

packages_verify() {
	check "packages installed" test -z "$(missing_packages)"
	check "cloud-hypervisor $HANGAR_CH_VERSION" ch_is_pinned
	check "qemu-img $HANGAR_QEMU_IMG_MIN or newer" qemu_img_recent
	check "EDK2 firmware $HANGAR_FW_VERSION" fw_is_pinned
	check "gh $HANGAR_GH_VERSION" gh_is_pinned
}
