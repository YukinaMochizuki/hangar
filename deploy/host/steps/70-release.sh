# Hangar itself: the binaries, the guest kernel and initramfs, the
# converter initramfs, and the console's files, installed from a release
# directory: one
# hangar-fetch-release downloaded and verified (bootstrap.sh --release DIR),
# or, on a testbed, one `make release` made. Every file is
# checked against the release's SHA256SUMS before anything is installed. What
# is installed, and its checksums by installed path, are recorded under
# /usr/local/lib/hangar. Without --release the step keeps what is installed.
# The guest kernel and initramfs a VM boots are the ones installed when it
# starts; a running VM keeps its own.
#
# The console comes as console.tar, installed as the other files are, then
# unpacked into a directory of its own under RELEASE_CONSOLE_VERSIONS, named
# by the tar's checksum, with the previous release's assets copied in beside
# its own. RELEASE_CONSOLE, which hangar-edge serves (--console-dir), is a
# symbolic link to it, replaced in one rename, so the edge needs no restart,
# and a page loaded just before the switch still gets its assets: they are
# named by their content, so the two releases' never collide. The previous
# release's directory is kept; older ones are removed.
#
# The step also installs deploy/host/fetch-release.sh, from the deploy/ it
# runs from, as RELEASE_FETCH, with or without --release.

RELEASE_LIB=/usr/local/lib/hangar
RELEASE_VERSION=$RELEASE_LIB/VERSION
RELEASE_SUMS=$RELEASE_LIB/SHA256SUMS
RELEASE_CONSOLE_TAR=$RELEASE_LIB/console.tar
RELEASE_CONVERTER=$RELEASE_LIB/converter.cpio
RELEASE_CONSOLE=/usr/local/share/hangar/console
RELEASE_CONSOLE_VERSIONS=/usr/local/share/hangar/console.d
RELEASE_FETCH=/usr/local/sbin/hangar-fetch-release
RELEASE_FETCH_SRC=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/fetch-release.sh

# Each entry is NAME:PATH:MODE, NAME being the file in the release.
release_files() {
	printf '%s\n' \
		hangar-apiserver:/usr/local/bin/hangar-apiserver:755 \
		hangar-node:/usr/local/bin/hangar-node:755 \
		hangar-edge:/usr/local/bin/hangar-edge:755 \
		hangar:/usr/local/bin/hangar:755 \
		"vmlinux:$RELEASE_LIB/vmlinux:644" \
		"initramfs.cpio:$RELEASE_LIB/initramfs.cpio:644" \
		"converter.cpio:$RELEASE_LIB/converter.cpio:644" \
		"console.tar:$RELEASE_CONSOLE_TAR:644"
}

# Files a release lists that an installed release may lack, having come
# before they were part of one: the record of such a release is still whole.
RELEASE_LATER_FILES="console.tar converter.cpio"

release_later_file() { case " $RELEASE_LATER_FILES " in *" $1 "*) return 0 ;; esac; return 1; }

sha_of() { sha256sum "$1" | cut -d' ' -f1; }

# release_check_dir DIR refuses a release whose files do not match its
# SHA256SUMS, or that lacks a file.
release_check_dir() {
	local dir=$1 entry name
	[ -f "$dir/SHA256SUMS" ] || die "$dir has no SHA256SUMS; is it a release directory (hangar-fetch-release, make release)?"
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
	put_file "$RELEASE_FETCH" 755 "$(cat "$RELEASE_FETCH_SRC")"
	if [ -z "$dir" ]; then
		if [ ! -f "$RELEASE_VERSION" ]; then
			[ "$DRY_RUN" = 1 ] || die "no hangar release is installed; pass --release DIR (docs/runbook/host-bootstrap.md)"
			log "warning: no hangar release is installed; a real run needs --release DIR"
			return 0
		fi
		# Files of two releases (a run that died half-way, or one replaced by
		# hand): the services step would restart onto them.
		if release_installed_ok >/dev/null 2>&1; then
			# The record matches what is installed, console.tar included, so a
			# console link that is not that tar's (removed by hand, or a run
			# that died before the switch) is made again from it.
			[ ! -f "$RELEASE_CONSOLE_TAR" ] || release_console_installed_ok ||
				run "unpack the console into $RELEASE_CONSOLE" release_console_unpack
			return 0
		fi
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
	release_console_ok "$(grep -E '  console.tar$' "$dir/SHA256SUMS" | cut -d' ' -f1)" ||
		run "unpack the console into $RELEASE_CONSOLE" release_console_unpack
	# Last, so a run that dies half-way leaves a record that does not match
	# what is installed, which verify reports and the next run repairs.
	put_file "$RELEASE_SUMS" 644 "$(release_installed_sums "$dir")"
	put_file "$RELEASE_VERSION" 644 "$(cat "$dir/VERSION")"
}

# release_console_dir SHA prints the directory the console of the tar with
# that checksum is unpacked in.
release_console_dir() { printf '%s/%s\n' "$RELEASE_CONSOLE_VERSIONS" "${1:0:16}"; }

# release_console_ok SHA holds when RELEASE_CONSOLE is the console of the
# tar with that checksum.
release_console_ok() {
	[ -n "$1" ] && [ -L "$RELEASE_CONSOLE" ] &&
		[ "$(readlink "$RELEASE_CONSOLE")" = "$(release_console_dir "$1")" ] &&
		[ -f "$(release_console_dir "$1")/index.html" ]
}

# release_console_carry FROM TO copies the console FROM's own assets into the
# console TO, but those TO already has: a name both have is the same
# content, assets being named by it.
release_console_carry() {
	local from=$1 to=$2 asset
	[ -f "$from.own" ] || return 0
	while IFS= read -r asset; do
		[ -e "$to/$asset" ] || [ ! -f "$from/$asset" ] ||
			install -D -m 644 -o root -g root "$from/$asset" "$to/$asset"
	done <"$from.own"
}

# release_console_unpack unpacks the installed console.tar and points
# RELEASE_CONSOLE at it, then removes the consoles of every earlier release
# but the one it served until now. The console it switches to, new or kept
# from before (a rollback), gets the assets of the one it switches from, so
# a page loaded just before the switch still loads.
release_console_unpack() {
	local sha target part old prev=
	sha=$(sha_of "$RELEASE_CONSOLE_TAR")
	target=$(release_console_dir "$sha")
	install -d -m 755 -o root -g root "$RELEASE_CONSOLE_VERSIONS"
	# The release served until now, kept beside the new one.
	if [ -L "$RELEASE_CONSOLE" ] && [ -d "$(readlink "$RELEASE_CONSOLE")" ] && [ "$(readlink "$RELEASE_CONSOLE")" != "$target" ]; then
		prev=$(readlink "$RELEASE_CONSOLE")
	fi
	if [ -f "$target/index.html" ]; then
		# Kept from before: it is not served now, so it takes the assets in
		# place.
		[ -z "$prev" ] || release_console_carry "$prev" "$target"
	else
		# Unpacked under a temporary name first, so a directory named for a
		# release always holds all of it.
		part=$(mktemp -d "$RELEASE_CONSOLE_VERSIONS/.part.XXXXXX")
		tar -xf "$RELEASE_CONSOLE_TAR" -C "$part" --no-same-owner --no-same-permissions
		# Which assets are the release's own, beside the directory so the edge
		# never serves the list: only those are carried to the next release.
		(cd "$part" && find assets -type f 2>/dev/null | sort) >"$part.own"
		[ -z "$prev" ] || release_console_carry "$prev" "$part"
		chown -R root:root "$part"
		chmod -R u=rwX,go=rX "$part"
		rm -rf "$target"
		mv -f "$part.own" "$target.own"
		mv -T "$part" "$target"
	fi
	# A real directory, from before releases carried the console, is moved
	# aside rather than written over.
	if [ -e "$RELEASE_CONSOLE" ] && [ ! -L "$RELEASE_CONSOLE" ]; then
		mv -T "$RELEASE_CONSOLE" "$RELEASE_CONSOLE.hangar-old"
	fi
	ln -sfn "$target" "$RELEASE_CONSOLE.hangar-new"
	mv -Tf "$RELEASE_CONSOLE.hangar-new" "$RELEASE_CONSOLE"
	for old in "$RELEASE_CONSOLE_VERSIONS"/* "$RELEASE_CONSOLE_VERSIONS"/.part.*; do
		case $old in "$target" | "$target.own" | "$prev" | "$prev.own") continue ;; esac
		[ ! -e "$old" ] || rm -rf "$old"
	done
	rm -rf "$RELEASE_CONSOLE.hangar-old"
}

# release_recorded PATH holds when the installed release's record lists PATH.
release_recorded() { grep -qE "^[0-9a-f]{64}  $1\$" "$RELEASE_SUMS"; }

# release_installed_ok holds when the record lists every file a release
# has (but those an earlier release may lack), and nothing else, and every
# file it lists matches.
release_installed_ok() {
	[ -f "$RELEASE_VERSION" ] && [ -f "$RELEASE_SUMS" ] || return 1
	local entry name path listed=0
	for entry in $(release_files); do
		IFS=: read -r name path _ <<<"$entry"
		if release_recorded "$path"; then
			listed=$((listed + 1))
		else
			release_later_file "$name" || return 1
		fi
	done
	[ "$(wc -l <"$RELEASE_SUMS")" = "$listed" ] && sha256sum -c --quiet "$RELEASE_SUMS"
}

release_modes_ok() {
	local entry path mode
	for entry in $(release_files); do
		IFS=: read -r _ path mode <<<"$entry"
		release_recorded "$path" || continue
		[ "$(stat -c '%a %U' "$path")" = "$mode root" ] || return 1
	done
}

release_fetch_ok() {
	[ "$(stat -c '%a %U' "$RELEASE_FETCH")" = "755 root" ] && generated_is "$RELEASE_FETCH" cat "$RELEASE_FETCH_SRC"
}

release_verify() {
	check "$RELEASE_FETCH is this deploy/'s, root's, mode 755" release_fetch_ok
	check "hangar release $(cat "$RELEASE_VERSION" 2>/dev/null || echo '(none)') installed, checksums match" release_installed_ok
	check "hangar release files root's, modes as installed" release_modes_ok
	if [ -f "$RELEASE_CONSOLE_TAR" ]; then
		check "the console served is the installed release's" release_console_installed_ok
	else
		skip "the console served is the installed release's" "the installed release carries no console"
	fi
	if [ -f "$RELEASE_CONVERTER" ]; then
		check "the converter initramfs carries qemu-img $HANGAR_CONVERTER_QEMU_IMG" release_converter_ok
	else
		skip "the converter initramfs carries qemu-img $HANGAR_CONVERTER_QEMU_IMG" "the installed release carries no converter initramfs"
	fi
}

# release_converter_ok holds when the installed converter initramfs says it
# carries the qemu-img versions.env pins (guest/converter/build.sh): its one
# marker, taken out of the archive, is that version's. The marker is a
# file's content in the cpio, right after that file's header and name with
# no newline before it, so it is matched alone (grep -o), never as a line.
release_converter_ok() {
	local marker
	marker=$(LC_ALL=C grep -ao 'hangar-converter qemu-img-[^[:space:]]*' "$RELEASE_CONVERTER") &&
		[ "$marker" = "hangar-converter qemu-img-$HANGAR_CONVERTER_QEMU_IMG" ]
}

release_console_installed_ok() {
	[ -f "$RELEASE_CONSOLE_TAR" ] && release_console_ok "$(sha_of "$RELEASE_CONSOLE_TAR")"
}
