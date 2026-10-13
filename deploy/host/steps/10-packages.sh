# Host packages and the pinned Cloud Hypervisor binary.

CH_BIN=/usr/local/bin/cloud-hypervisor

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

packages_apply() {
	local missing
	missing=$(missing_packages)
	if [ -n "$missing" ]; then
		run "install packages: $missing" sh -c \
			"apt-get update -q && DEBIAN_FRONTEND=noninteractive apt-get install -qy --no-install-recommends $missing"
	fi
	ch_is_pinned || run "install cloud-hypervisor $HANGAR_CH_VERSION" install_ch
}

packages_verify() {
	check "packages installed" test -z "$(missing_packages)"
	check "cloud-hypervisor $HANGAR_CH_VERSION" ch_is_pinned
	check "qemu-img $HANGAR_QEMU_IMG_MIN or newer" qemu_img_recent
}
