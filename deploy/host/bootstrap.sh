#!/usr/bin/env bash
# Brings a freshly installed host to the state hangar expects. Safe to re-run:
# every step checks before it changes anything.
set -euo pipefail

usage() {
	cat <<'EOF'
usage: bootstrap.sh [--host NAME | --env FILE] [--only STEP] [--release DIR] [--dry-run]

  --host NAME    use hosts/NAME.env (default: the output of `hostname`)
  --env FILE     use FILE instead of a file under hosts/
  --only STEP    run one step, e.g. --only swap-raid
  --release DIR  install the release in DIR (made by `make release`); without
                 it, the release step keeps what is installed
  --dry-run      print what would change and change nothing

Prints "changes: N" at the end; N is 0 when the host was already converged.
EOF
}

here=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=deploy/host/lib.sh
. "$here/lib.sh"

host=$(hostname)
env_file=
only=
# HANGAR_RELEASE_DIR is read by the release step.
# shellcheck disable=SC2034
while [ $# -gt 0 ]; do
	case $1 in
	--host) host=$2; shift 2 ;;
	--env) env_file=$2; shift 2 ;;
	--only) only=$2; shift 2 ;;
	--release) HANGAR_RELEASE_DIR=$(cd "$2" && pwd) || die "no release directory $2"; shift 2 ;;
	--dry-run) DRY_RUN=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) usage >&2; exit 2 ;;
	esac
done
[ "$(id -u)" = 0 ] || die "must run as root"
load_env "${env_file:-$here/hosts/$host.env}"
# While a netplan change is on probation the timer may still restore the old
# config, so nothing may build on the new one yet.
! rollback_pending ||
	die "$ROLLBACK_UNIT.timer is pending; wait for it to fire or run verify.sh, then re-run"
# The services step would refuse without the local env file; refuse before
# any step changes the host instead.
if [ "$DRY_RUN" != 1 ] && { [ -z "$only" ] || [ "$only" = services ]; }; then
	# shellcheck source=deploy/host/steps/80-services.sh
	. "$here/steps/80-services.sh"
	services_local_env_ok || die "$LOCAL_ENV is missing or wrong: $(services_local_env_how)"
	problem=$(auth_problem)
	[ -z "$problem" ] || die "$(printf '%s' "$problem" | tr '\n' ';' | sed 's/;/; /g')"
fi

ran=0
for f in "$here"/steps/[0-9][0-9]-*.sh; do
	step=$(basename "$f" .sh)
	step=${step#[0-9][0-9]-}
	[ -z "$only" ] || [ "$only" = "$step" ] || continue
	# shellcheck disable=SC1090
	. "$f"
	log "== $step"
	"${step//-/_}_apply"
	ran=1
done
[ "$ran" = 1 ] || die "no step named $only"
flush_initramfs

printf 'changes: %d\n' "$CHANGES"
