#!/usr/bin/env bash
# Names and checks prebuilt guest kernels, for deploy/release.sh's kernel cache.
#
#   cache.sh key          print the key of the checkout's kernel inputs
#   cache.sh check DIR    check that DIR holds a vmlinux and a config that
#                         matches the committed guest/kernel/config
#
# The key is the sha256 of kernel.env, hangar.fragment, config and build.sh,
# concatenated in that order: everything build.sh reads from the repo, and
# build.sh itself. The hangar-testbed blueprint's `hangar-testbed-prepare
# --kernel-key` computes the same value, so its prebuilt kernels are hits; the
# two change together. The key leaves out the toolchain, which the comparison
# in `check` covers as far as the config shows it (docs/runbook/guest-kernel.md).
set -euo pipefail
shopt -s inherit_errexit

HERE=$(cd "$(dirname "$0")" && pwd)

log() { printf '%s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }
usage() { echo "usage: $0 key | check DIR" >&2; exit 2; }

# The same lines build.sh leaves out of its comparison; fragment_test.go keeps
# the two patterns equal.
normalize() {
	grep -vE '^CONFIG_(CC_VERSION_TEXT|(GCC|CLANG|AS|LD|LLD|RUSTC|RUSTC_LLVM|PAHOLE)_VERSION)=' "$1"
}

case ${1:-} in
key)
	[ $# -eq 1 ] || usage
	(cd "$HERE" && cat kernel.env hangar.fragment config build.sh) | sha256sum | cut -d' ' -f1
	;;
check)
	[ $# -eq 2 ] || usage
	dir=$2
	[ -f "$dir/vmlinux" ] || die "$dir has no vmlinux"
	[ -f "$dir/config" ] || die "$dir has no config"
	if ! normalize "$dir/config" | diff -u --label committed --label cached "$HERE/config" - >&2; then
		die "the config in $dir differs from guest/kernel/config; remove $dir to build the kernel again"
	fi
	;;
*) usage ;;
esac
