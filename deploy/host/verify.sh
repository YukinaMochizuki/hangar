#!/usr/bin/env bash
# Reports, without changing anything, whether the host matches what
# bootstrap.sh converges it to. One PASS/FAIL line per check; exits 1 on any FAIL.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=deploy/host/lib.sh
. "$here/lib.sh"

host=$(hostname)
env_file=
while [ $# -gt 0 ]; do
	case $1 in
	--host) host=$2; shift 2 ;;
	--env) env_file=$2; shift 2 ;;
	*) echo "usage: verify.sh [--host NAME | --env FILE]" >&2; exit 2 ;;
	esac
done
[ "$(id -u)" = 0 ] || die "must run as root"
load_env "${env_file:-$here/hosts/$host.env}"

for f in "$here"/steps/[0-9][0-9]-*.sh; do
	step=$(basename "$f" .sh)
	step=${step#[0-9][0-9]-}
	# shellcheck disable=SC1090
	. "$f"
	"${step//-/_}_verify"
done
check "no initramfs rebuild pending" test ! -e "$INITRAMFS_MARKER"

[ "$FAILURES" = 0 ]
