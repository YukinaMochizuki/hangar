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
# What stays on the host and out of the repo: the bootstrap admin's email
# (the human's Cloudflare Access identity). The operator writes it by hand.
# shellcheck disable=SC2034
LOCAL_ENV=/etc/hangar/local.env
# The Cloudflare tunnel's token, written by hand like LOCAL_ENV; without it
# the host runs no tunnel.
# shellcheck disable=SC2034
CLOUDFLARED_ENV=/etc/hangar/cloudflared.env
CLOUDFLARED_UNIT=hangar-cloudflared

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

# skip NAME WHY prints a SKIP line for verify.sh: a check that does not apply
# to this host.
skip() { printf 'SKIP %s (%s)\n' "$1" "$2"; }

# refuse WHAT stops a real run; a dry run only warns and goes on.
refuse() {
	[ "$DRY_RUN" = 1 ] || die "$1"
	log "warning: $1; a real run refuses"
}

# local_env KEY prints KEY's value in the local env file, empty when unset.
local_env() {
	[ -f "$LOCAL_ENV" ] || return 0
	sed -n "s/^$1=//p" "$LOCAL_ENV" | tail -n 1 | tr -d "\"'"
}

# auth_mode prints how the public API authenticates, as the local env file
# asks: HANGAR_AUTH_MODE, dev when unset.
auth_mode() {
	local m
	m=$(local_env HANGAR_AUTH_MODE)
	printf '%s\n' "${m:-dev}"
}

# auth_problem prints what is wrong with the local env file's auth settings,
# nothing when they make a whole mode.
auth_problem() {
	local mode
	mode=$(auth_mode)
	case $mode in
	dev) ;;
	cloudflare)
		[[ $(local_env HANGAR_CF_TEAM_DOMAIN) =~ ^https://[A-Za-z0-9.-]+$ ]] ||
			printf '%s\n' "cloudflare auth needs HANGAR_CF_TEAM_DOMAIN=https://<team>.cloudflareaccess.com in $LOCAL_ENV"
		[[ $(local_env HANGAR_CF_AUDIENCE) =~ ^[A-Za-z0-9]+$ ]] ||
			printf '%s\n' "cloudflare auth needs HANGAR_CF_AUDIENCE=<the Access application's AUD tag> in $LOCAL_ENV"
		;;
	*) printf '%s\n' "HANGAR_AUTH_MODE in $LOCAL_ENV must be dev or cloudflare, not $mode" ;;
	esac
}

# apiserver_auth_mode prints the auth mode the running apiserver was started
# with, nothing when it is not running.
apiserver_auth_mode() {
	local pid
	pid=$(systemctl show -p MainPID --value hangar-apiserver.service)
	[ "$pid" != 0 ] || return 0
	tr '\0' '\n' <"/proc/$pid/cmdline" | sed -n '/^--auth-mode$/{n;p;q}'
}

# cloudflared_up holds while the tunnel runs, or would start at boot.
cloudflared_up() {
	systemctl is-active --quiet "$CLOUDFLARED_UNIT.service" ||
		systemctl is-enabled --quiet "$CLOUDFLARED_UNIT.service" 2>/dev/null
}

# cloudflared_down WHY stops and disables the tunnel, if it is up.
cloudflared_down() {
	cloudflared_up || return 0
	run "stop and disable $CLOUDFLARED_UNIT: $1" systemctl disable --now "$CLOUDFLARED_UNIT.service"
}

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
		HANGAR_SWAP_PARTS HANGAR_SWAP_MD_NAME HANGAR_ZPOOL HANGAR_ARC_MAX_BYTES \
		HANGAR_NODE_NAME HANGAR_PUBLIC_IPV4 HANGAR_IMAGES_QUOTA HANGAR_VMS_QUOTA \
		HANGAR_RESERVED_MEMORY
	local v
	for v in HANGAR_IMAGES_QUOTA HANGAR_VMS_QUOTA; do
		[[ ${!v} =~ ^[1-9][0-9]*G$ ]] || die "$v must be a whole number of GiB, such as 100G, not ${!v}"
	done
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

# gib_of SIZE prints the GiB in a size of the form NG.
gib_of() { printf '%s\n' "${1%G}"; }

# bytes_of SIZE prints the bytes in a size of the form NG.
bytes_of() { printf '%s\n' $(($(gib_of "$1") << 30)); }

uplink_holds_subnet_router() {
	local a
	for a in $(uplink_global_ipv6); do
		ipv6_is_subnet_router "$a" && return 0
	done
	return 1
}
