# Hangar itself: the binaries and the guest kernel and initramfs, installed
# from a release directory `make release` made and someone copied to the host
# (bootstrap.sh --release DIR). Every file is checked against the release's
# SHA256SUMS before anything is installed. What is installed, and its
# checksums by installed path, are recorded under /usr/local/lib/hangar.
# Without --release the step keeps what is installed. The guest kernel and
# initramfs a VM boots are the ones installed when it starts; a running VM
# keeps its own.

RELEASE_LIB=/usr/local/lib/hangar
RELEASE_VERSION=$RELEASE_LIB/VERSION
RELEASE_SUMS=$RELEASE_LIB/SHA256SUMS

# Each entry is NAME:PATH:MODE, NAME being the file in the release.
release_files() {
	printf '%s\n' \
		hangar-apiserver:/usr/local/bin/hangar-apiserver:755 \
		hangar-node:/usr/local/bin/hangar-node:755 \
		hangar:/usr/local/bin/hangar:755 \
		"vmlinux:$RELEASE_LIB/vmlinux:644" \
		"initramfs.cpio:$RELEASE_LIB/initramfs.cpio:644"
}

sha_of() { sha256sum "$1" | cut -d' ' -f1; }

# release_check_dir DIR refuses a release whose files do not match its
# SHA256SUMS, or that lacks a file.
release_check_dir() {
	local dir=$1 entry name
	[ -f "$dir/SHA256SUMS" ] || die "$dir has no SHA256SUMS; is it a directory make release wrote?"
	for entry in $(release_files) VERSION; do
		name=${entry%%:*}
		grep -qE "^[0-9a-f]{64}  $name\$" "$dir/SHA256SUMS" ||
			die "$dir/SHA256SUMS does not list $name; refusing the release"
	done
	(cd "$dir" && sha256sum -c --quiet SHA256SUMS) >&2 ||
		die "checksum mismatch in $dir; refusing the release, nothing was installed"
}

# release_installed_sums prints the recorded checksums, by installed path,
# of the release in DIR.
release_installed_sums() {
	local dir=$1 entry name path
	for entry in $(release_files); do
		IFS=: read -r name path _ <<<"$entry"
		printf '%s  %s\n' "$(grep -E "  $name\$" "$dir/SHA256SUMS" | cut -d' ' -f1)" "$path"
	done
}

# release_file_ok PATH MODE SHA holds when PATH is that file.
release_file_ok() {
	[ -f "$1" ] && [ "$(stat -c '%a %U' "$1")" = "$2 root" ] && [ "$(sha_of "$1")" = "$3" ]
}

install_release_file() {
	local src=$1 path=$2 mode=$3
	install -D -m "$mode" -o root -g root "$src" "$path.hangar-new"
	# A rename, so a running binary keeps its old file and nothing ever
	# runs a half-written one.
	mv -f "$path.hangar-new" "$path"
}

release_apply() {
	local dir=${HANGAR_RELEASE_DIR:-} entry name path mode sha
	if [ -z "$dir" ]; then
		if [ ! -f "$RELEASE_VERSION" ]; then
			[ "$DRY_RUN" = 1 ] || die "no hangar release is installed; pass --release DIR (docs/runbook/host-bootstrap.md)"
			log "warning: no hangar release is installed; a real run needs --release DIR"
			return 0
		fi
		# Files of two releases (a run that died half-way, or one replaced by
		# hand): the services step would restart onto them.
		release_installed_ok >/dev/null 2>&1 && return 0
		local why
		why="the installed files do not match the checksums recorded for release $(cat "$RELEASE_VERSION")"
		[ "$DRY_RUN" = 1 ] || die "$why; rerun with --release DIR to install a whole release (docs/runbook/host-bootstrap.md)"
		log "warning: $why; a real run refuses without --release DIR"
		return 0
	fi
	release_check_dir "$dir"
	log "release $(cat "$dir/VERSION") from $dir"
	for entry in $(release_files); do
		IFS=: read -r name path mode <<<"$entry"
		sha=$(grep -E "  $name\$" "$dir/SHA256SUMS" | cut -d' ' -f1)
		release_file_ok "$path" "$mode" "$sha" && continue
		run "install $name as $path" install_release_file "$dir/$name" "$path" "$mode"
		# For the services step, which restarts what runs a replaced binary.
		RELEASE_REPLACED="${RELEASE_REPLACED:-} $name"
	done
	# Last, so a run that dies half-way leaves a record that does not match
	# what is installed, which verify reports and the next run repairs.
	put_file "$RELEASE_SUMS" 644 "$(release_installed_sums "$dir")"
	put_file "$RELEASE_VERSION" 644 "$(cat "$dir/VERSION")"
}

release_installed_ok() {
	[ -f "$RELEASE_VERSION" ] && [ -f "$RELEASE_SUMS" ] &&
		[ "$(wc -l <"$RELEASE_SUMS")" = "$(release_files | wc -l)" ] &&
		sha256sum -c --quiet "$RELEASE_SUMS"
}

release_modes_ok() {
	local entry path mode
	for entry in $(release_files); do
		IFS=: read -r _ path mode <<<"$entry"
		[ "$(stat -c '%a %U' "$path")" = "$mode root" ] || return 1
	done
}

release_verify() {
	check "hangar release $(cat "$RELEASE_VERSION" 2>/dev/null || echo '(none)') installed, checksums match" release_installed_ok
	check "hangar release files root's, modes as installed" release_modes_ok
}
