#!/usr/bin/env bash
# Reports, without changing anything, whether the host matches what
# bootstrap.sh converges it to. One PASS/FAIL line per check; exits 1 on any FAIL.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=deploy/host/lib.sh
. "$here/lib.sh"

env_file=$HOST_ENV
while [ $# -gt 0 ]; do
	case $1 in
	--env) env_file=$2; shift 2 ;;
	*) echo "usage: verify.sh [--env FILE]  (default: $HOST_ENV)" >&2; exit 2 ;;
	esac
done
[ "$(id -u)" = 0 ] || die "must run as root"
load_env "$env_file"

# The file the settings came from, which load_env found: /etc/hangar/host.env
# unless --env named another.
check "host env file $env_file root's, mode 0600" host_env_private "$env_file"

for f in "$here"/steps/[0-9][0-9]-*.sh; do
	step=$(basename "$f" .sh)
	step=${step#[0-9][0-9]-}
	# shellcheck disable=SC1090
	. "$f"
	"${step//-/_}_verify"
done
check "no initramfs rebuild pending" test ! -e "$INITRAMFS_MARKER"

[ "$FAILURES" = 0 ]
