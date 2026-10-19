#!/usr/bin/env bash
# Brings a freshly installed host to the state hangar expects. Safe to re-run:
# every step checks before it changes anything.
set -euo pipefail

usage() {
	cat <<'EOF'
usage: bootstrap.sh [--env FILE] [--only STEP] [--release DIR] [--dry-run]

  --env FILE     read the host's settings from FILE instead of
                 /etc/hangar/host.env
  --only STEP    run one step, e.g. --only swap-raid
  --release DIR  install the release in DIR (hangar-fetch-release puts one in
                 /var/tmp/hangar/release/TAG); without it, the release step
                 keeps what is installed
  --dry-run      print what would change and change nothing

Prints "changes: N" at the end; N is 0 when the host was already converged.
EOF
}

here=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=deploy/host/lib.sh
. "$here/lib.sh"

env_file=$HOST_ENV
only=
# HANGAR_RELEASE_DIR is read by the release step.
# shellcheck disable=SC2034
while [ $# -gt 0 ]; do
	case $1 in
	--env) env_file=$2; shift 2 ;;
	--only) only=$2; shift 2 ;;
	--release) HANGAR_RELEASE_DIR=$(cd "$2" && pwd) || die "no release directory $2"; shift 2 ;;
	--dry-run) DRY_RUN=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) usage >&2; exit 2 ;;
	esac
done
[ "$(id -u)" = 0 ] || die "must run as root"
load_env "$env_file"
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
# The edge step would refuse an edge that cannot run as local.env asks,
# and the edge refuses to start on flags its own check-flags refuses, such
# as two domains that share a registrable domain: refuse before any step,
# the release step's new hangar-edge included, so a refused run changes
# nothing. The hangar-edge that checks is the one the unit runs after this
# run: the release's when the release step installs one, else (no
# --release, or --only edge) the installed one. --only release installs a
# hangar-edge the unit runs at its next restart: its check-flags checks the
# flags the next edge step will write.
if [ -z "$only" ] || [ "$only" = edge ] || [ "$only" = release ]; then
	# shellcheck source=deploy/host/steps/85-edge.sh
	. "$here/steps/85-edge.sh"
	if [ -z "$only" ]; then
		problem=$(services_endpoints_problem; edge_problem)
	elif [ "$only" = release ]; then
		problem=$(edge_check_problem)
	else
		problem=$(services_endpoints_problem; HANGAR_RELEASE_DIR='' edge_problem)
	fi
	[ -z "$problem" ] || refuse "$(printf '%s' "$problem" | tr '\n' ';' | sed 's/;/; /g')"
fi
# A hangar-node the release step installs refuses to start while anything
# else has one of the VMs' ids; refuse before it is installed.
if [ -z "$only" ] || [ "$only" = release ] || [ "$only" = services ]; then
	# shellcheck source=deploy/host/steps/80-services.sh
	. "$here/steps/80-services.sh"
	services_vm_ids_free || refuse "$(services_vm_ids_problem)"
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
