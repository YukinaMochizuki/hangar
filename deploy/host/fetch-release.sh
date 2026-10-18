#!/usr/bin/env bash
# Downloads a hangar release from its GitHub Release, verifies it, and puts
# it where bootstrap.sh takes it from. The release step installs it as
# /usr/local/sbin/hangar-fetch-release (docs/runbook/host-bootstrap.md).
#
#   hangar-fetch-release TAG DIGEST
#
# DIGEST is `sha256sum SHA256SUMS` of the release, as its release notes state
# it. Of the release's files a host takes SHA256SUMS, VERSION and
# hangar-host_TAG_linux_amd64.tar.gz, the host's files packed
# (deploy/release.sh), and attestation.jsonl to check them; the CLI's
# archives and install.sh it leaves alone. The
# release is refused unless SHA256SUMS has that digest, the other two match
# it, and all three carry a build-provenance attestation made by
# .github/workflows/release.yml in this repository, for the tag TAG, from the
# commit VERSION names. The attestations come with the release as
# attestation.jsonl and are checked with `gh attestation verify --bundle`,
# which needs no GitHub login; it reaches only Sigstore's and GitHub's TUF
# repositories, for their current trust roots (docs/runbook/release.md).
#
# Only then is the tar unpacked, and only when it holds plain files with
# plain names, and its own SHA256SUMS lists every one of them, they match it,
# and its VERSION is the release's.
#
# On success the host's files are in /var/tmp/hangar/release/TAG/, with the
# release's SHA256SUMS as release.SHA256SUMS and attestation.jsonl beside
# them, and deploy.tar is unpacked in /var/tmp/hangar/deploy-TAG/. Both are
# staged under a temporary name and renamed into place, replacing what a
# fetch of the same tag left there, so neither name ever holds half a
# release.
#
# HANGAR_FETCH_RELEASE_BASE_URL replaces the download URL up to the tag, for
# tests against a stand-in for GitHub. Nothing skips the checks.
set -euo pipefail
shopt -s inherit_errexit
umask 022

REPO=YukinaMochizuki/hangar
WORKFLOW=$REPO/.github/workflows/release.yml
# deploy/host/steps/10-packages.sh installs the pinned gh here, and this
# script runs without that file, so it names the path itself. The minimum is
# the first version that matches --signer-workflow exactly.
GH=/usr/local/bin/gh
GH_MIN=2.102.0
BUNDLE=attestation.jsonl
TOP=/var/tmp/hangar
DEFAULT_BASE_URL=https://github.com/$REPO/releases/download
BASE_URL=${HANGAR_FETCH_RELEASE_BASE_URL:-$DEFAULT_BASE_URL}

log() { printf '%s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }
usage() { echo "usage: hangar-fetch-release TAG DIGEST" >&2; exit 2; }

[ $# = 2 ] || usage
tag=$1
digest=${2,,}
[[ $tag =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.(0|[1-9][0-9]*))?$ ]] ||
	die "$tag is not a release tag (vMAJOR.MINOR.PATCH, or that with -rc.N)"
[[ $digest =~ ^[0-9a-f]{64}$ ]] || die "DIGEST must be the 64 hex digits of sha256sum SHA256SUMS, not $2"
[ "$(id -u)" = 0 ] || die "must run as root"
[ -x "$GH" ] || die "$GH not found; the packages step installs it (docs/runbook/host-bootstrap.md)"
gh_version=$("$GH" --version | sed -n '1s/^gh version \([0-9][0-9.]*\).*/\1/p')
[ -n "$gh_version" ] && [ "$(printf '%s\n%s\n' "$GH_MIN" "$gh_version" | sort -V | head -n1)" = "$GH_MIN" ] ||
	die "$GH is version ${gh_version:-unknown}; verifying a release needs $GH_MIN or newer"

# Everything lands in TOP and is renamed within it, so it must be root's and
# writable by no one else, or another user could swap a verified file.
for d in "$TOP" "$TOP/release"; do
	if [ ! -d "$d" ]; then
		install -d -m 0755 -o root -g root "$d"
	elif [ -L "$d" ] || [ "$(stat -c %u "$d")" != 0 ] || [ $(($(stat -c '0%a' "$d") & 022)) != 0 ]; then
		die "$d must be a directory of root's that no one else can write"
	fi
done
release=$TOP/release/$tag
deploy=$TOP/deploy-$tag

stage=$(mktemp -d "$TOP/.fetch-$tag.XXXXXX")
gh_home=$(mktemp -d)
trap 'rm -rf "$stage" "$gh_home"' EXIT
mkdir "$stage/dl" "$stage/release" "$stage/deploy"

curl_proto=(--proto '=https' --proto-redir '=https')
[ "$BASE_URL" = "$DEFAULT_BASE_URL" ] || curl_proto=()
fetch() {
	log "fetch $1"
	curl -fsSL --retry 3 --connect-timeout 30 "${curl_proto[@]}" \
		-o "$stage/dl/$1" "$BASE_URL/$tag/$1" ||
		die "could not download $1 of $tag from $BASE_URL/$tag/"
}

host_tar=hangar-host_${tag}_linux_amd64.tar.gz

fetch SHA256SUMS
got=$(sha256sum "$stage/dl/SHA256SUMS" | cut -d' ' -f1)
[ "$got" = "$digest" ] ||
	die "SHA256SUMS of $tag has digest $got, not $digest; refusing the release"

# check_sums FILE NAME... refuses FILE unless each line is DIGEST, two
# spaces, a plain file name, no name comes twice, and it lists every NAME.
check_sums() {
	local file=$1 line name
	shift
	local -a seen=()
	while IFS= read -r line; do
		[[ $line =~ ^[0-9a-f]{64}\ \ ([A-Za-z0-9][A-Za-z0-9._-]*)$ ]] ||
			die "$file of $tag has a line that is not a checksum and a file name: $line"
		name=${BASH_REMATCH[1]}
		case " ${seen[*]} " in *" $name "*) die "$file of $tag lists $name twice" ;; esac
		case $name in SHA256SUMS | "$BUNDLE") die "$file of $tag lists $name" ;; esac
		seen+=("$name")
	done
	for name in "$@"; do
		case " ${seen[*]} " in *" $name "*) ;; *) die "$file of $tag does not list $name; refusing the release" ;; esac
	done
}

# The release's files a host takes, and nothing else: no other name in
# SHA256SUMS is downloaded.
check_sums SHA256SUMS VERSION "$host_tar" <"$stage/dl/SHA256SUMS"
names=(VERSION "$host_tar")
for name in "${names[@]}"; do
	fetch "$name"
	want=$(awk -v n="$name" '$2 == n { print $1 }' "$stage/dl/SHA256SUMS")
	[ "$(sha256sum "$stage/dl/$name" | cut -d' ' -f1)" = "$want" ] ||
		die "$name of $tag does not match its SHA256SUMS; refusing the release"
done

version=$(cat "$stage/dl/VERSION")
[[ $version =~ ^[0-9a-f]{40}$ ]] || die "VERSION of $tag is $version, not a commit; refusing the release"

fetch "$BUNDLE"
# gh gets a home of its own for its TUF cache, and none of the caller's
# tokens or hosts: the check is the same for anyone who runs it.
verify() {
	env -i PATH=/usr/bin:/bin HOME="$gh_home" GH_NO_UPDATE_NOTIFIER=1 GH_PROMPT_DISABLED=1 \
		${HTTPS_PROXY:+HTTPS_PROXY="$HTTPS_PROXY"} ${https_proxy:+https_proxy="$https_proxy"} \
		${NO_PROXY:+NO_PROXY="$NO_PROXY"} ${no_proxy:+no_proxy="$no_proxy"} \
		"$GH" attestation verify "$stage/dl/$1" --bundle "$stage/dl/$BUNDLE" \
		--hostname github.com --repo "$REPO" --signer-workflow "$WORKFLOW" \
		--source-ref "refs/tags/$tag" --source-digest "$version" --deny-self-hosted-runners \
		--format json >/dev/null
}
for name in SHA256SUMS "${names[@]}"; do
	log "verify the attestation of $name"
	verify "$name" ||
		die "$name of $tag has no attestation from $WORKFLOW for refs/tags/$tag at $version; refusing the release"
done

# The host's files, unpacked only now, and only when the tar holds plain
# files with plain names and nothing else: no directory, link or path.
listing=$(tar -tvzf "$stage/dl/$host_tar") || die "$host_tar of $tag is not a gzipped tar; refusing the release"
while IFS= read -r line; do
	[[ $line =~ ^- ]] || die "$host_tar of $tag holds something that is not a plain file: $line"
done <<<"$listing"
members=$(tar -tzf "$stage/dl/$host_tar")
while IFS= read -r name; do
	[[ $name =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || die "$host_tar of $tag holds $name, not a plain file name; refusing the release"
done <<<"$members"
[ "$(wc -l <<<"$listing")" = "$(wc -l <<<"$members")" ] ||
	die "$host_tar of $tag has a name that is not one line; refusing the release"
tar -xzf "$stage/dl/$host_tar" -C "$stage/release" --no-same-owner --no-same-permissions
[ -f "$stage/release/SHA256SUMS" ] || die "$host_tar of $tag has no SHA256SUMS; refusing the release"
check_sums "$host_tar's SHA256SUMS" VERSION deploy.tar <"$stage/release/SHA256SUMS"
listed=$( (cut -d' ' -f3 "$stage/release/SHA256SUMS"; echo SHA256SUMS) | LC_ALL=C sort)
[ "$listed" = "$(LC_ALL=C sort <<<"$members")" ] ||
	die "$host_tar of $tag holds other files than its SHA256SUMS lists; refusing the release"
(cd "$stage/release" && sha256sum -c --quiet --strict SHA256SUMS) >&2 ||
	die "a file of $host_tar of $tag does not match its SHA256SUMS; refusing the release"
[ "$(cat "$stage/release/VERSION")" = "$version" ] ||
	die "$host_tar of $tag is of $(cat "$stage/release/VERSION"), not $version; refusing the release"
# The evidence, kept with the host's files.
cp "$stage/dl/SHA256SUMS" "$stage/release/release.SHA256SUMS"
cp "$stage/dl/$BUNDLE" "$stage/release/$BUNDLE"

tar -xf "$stage/release/deploy.tar" -C "$stage/deploy" --no-same-owner --no-same-permissions
[ -x "$stage/deploy/host/bootstrap.sh" ] || die "deploy.tar of $tag has no host/bootstrap.sh; refusing the release"
# The host's files 0644 whatever mode the tar gave them: the release step
# installs each with its own mode, and nothing runs them from here
# (test/testbed/host-bootstrap.sh checks the fetched release is 0644).
chmod 0644 "$stage/release"/*
chmod -R u=rwX,go=rX "$stage/release" "$stage/deploy"

# place FROM TO renames FROM to TO, moving what TO was aside first.
place() {
	local old
	if [ -e "$2" ] || [ -L "$2" ]; then
		old=$(mktemp -d "$TOP/.old.XXXXXX")
		mv -T "$2" "$old/x"
	fi
	mv -T "$1" "$2"
	[ -z "${old:-}" ] || rm -rf "$old"
}
place "$stage/release" "$release"
place "$stage/deploy" "$deploy"

log "release $tag ($version) in $release, deploy/ in $deploy"
log "next: sudo $deploy/host/bootstrap.sh --release $release --dry-run"
