# Shared helpers for bootstrap.sh, verify.sh and the steps. Sourced, not run.
#
# Every change a step makes goes through `run` or `put_file`, so --dry-run is
# honoured everywhere and the change count reflects what a real run would do.

# A generator that fails inside $(...) must abort the run, not hand put_file a
# truncated file.
shopt -s inherit_errexit

DRY_RUN=${DRY_RUN:-0}
CHANGES=0
FAILURES=0
STATE_DIR=/var/lib/hangar-bootstrap
INITRAMFS_MARKER=$STATE_DIR/initramfs-pending
INITRAMFS_PENDING=0
ROLLBACK_UNIT=hangar-netplan-rollback

log() { printf '%s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }

# run DESCRIPTION CMD... executes CMD, or only reports it under --dry-run.
run() {
	local desc=$1
	shift
	CHANGES=$((CHANGES + 1))
	if [ "$DRY_RUN" = 1 ]; then
		log "would: $desc"
		return 0
	fi
	log "apply: $desc"
	"$@"
}

# put_file PATH MODE CONTENT writes CONTENT plus a final newline to PATH when the
# content or mode differs, showing the diff either way. Callers capture CONTENT
# with $(...) first, so a failing generator stops the run before anything is
# written. The write is atomic so a crash never leaves a half-written file.
put_file() {
	local path=$1 mode=$2 content=$3 tmp
	tmp=$(mktemp)
	printf '%s\n' "$content" >"$tmp"
	if [ -f "$path" ] && cmp -s "$tmp" "$path" &&
		[ "$(stat -c %a "$path")" = "$mode" ]; then
		rm -f "$tmp"
		return 0
	fi
	if [ -f "$path" ]; then
		diff -u "$path" "$tmp" >&2 || true
	else
		log "--- new file $path"
		cat "$tmp" >&2
	fi
	CHANGES=$((CHANGES + 1))
	if [ "$DRY_RUN" = 1 ]; then
		log "would: write $path"
		rm -f "$tmp"
		return 0
	fi
	log "apply: write $path"
	install -D -m "$mode" "$tmp" "$path.hangar-new"
	mv -f "$path.hangar-new" "$path"
	rm -f "$tmp"
}

pkg_installed() {
	dpkg-query -W -f='${Status}' "$1" 2>/dev/null | grep -q 'install ok installed'
}

# need_initramfs records that the initramfs is stale. The marker lives on disk
# so a run that dies before flush_initramfs still rebuilds it next time.
need_initramfs() {
	INITRAMFS_PENDING=1
	run "mark the initramfs for rebuild" sh -c "mkdir -p $STATE_DIR && touch $INITRAMFS_MARKER"
}

flush_initramfs() {
	[ -e "$INITRAMFS_MARKER" ] || [ "$INITRAMFS_PENDING" = 1 ] || return 0
	run "rebuild the initramfs" sh -c "update-initramfs -u && rm -f $INITRAMFS_MARKER"
}

# rollback_pending succeeds while a netplan change is still on probation.
rollback_pending() { systemctl is-active --quiet "$ROLLBACK_UNIT.timer"; }

# check NAME CMD... prints one PASS/FAIL line for verify.sh and records failures.
check() {
	local name=$1
	shift
	if "$@" >/dev/null 2>&1; then
		printf 'PASS %s\n' "$name"
	else
		printf 'FAIL %s\n' "$name"
		FAILURES=$((FAILURES + 1))
	fi
}

require_vars() {
	local v
	for v in "$@"; do
		[ -n "${!v:-}" ] || die "$v is not set in the host env file"
	done
}

# load_env FILE sources the repo-wide versions and the per-host values.
load_env() {
	local env_file=$1 here
	here=$(dirname "${BASH_SOURCE[0]}")
	[ -f "$env_file" ] || die "host env file $env_file not found"
	# shellcheck source=deploy/host/versions.env
	. "$here/versions.env"
	# shellcheck disable=SC1090
	. "$env_file"
	require_vars HANGAR_UPLINK HANGAR_UPLINK_MAC HANGAR_IPV6_ADDR HANGAR_IPV6_GATEWAY \
		HANGAR_SWAP_PARTS HANGAR_SWAP_MD_NAME HANGAR_ZPOOL HANGAR_ARC_MAX_BYTES
	# ip prints addresses in canonical form; compare against the same form.
	HANGAR_IPV6_ADDR=$(ipv6_canon "$HANGAR_IPV6_ADDR")
	HANGAR_UPLINK_MAC=${HANGAR_UPLINK_MAC,,}
}

# ipv6_canon ADDR/LEN prints ADDR/LEN in canonical (RFC 5952) form.
ipv6_canon() {
	python3 -c 'import ipaddress, sys; print(ipaddress.IPv6Interface(sys.argv[1]).with_prefixlen)' "$1"
}

# ipv6_is_subnet_router ADDR/LEN succeeds when ADDR is the all-zero host
# address of its prefix (the Subnet-Router anycast address).
ipv6_is_subnet_router() {
	python3 - "$1" <<'EOF'
import ipaddress, sys
iface = ipaddress.IPv6Interface(sys.argv[1])
sys.exit(0 if iface.ip == iface.network.network_address else 1)
EOF
}

# uplink_global_ipv6 prints the uplink's global IPv6 addresses as ADDR/LEN.
uplink_global_ipv6() {
	ip -6 -o addr show dev "$HANGAR_UPLINK" scope global | awk '{print $4}'
}

uplink_holds_subnet_router() {
	local a
	for a in $(uplink_global_ipv6); do
		ipv6_is_subnet_router "$a" && return 0
	done
	return 1
}
