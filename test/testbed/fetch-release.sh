#!/usr/bin/env bash
# Exercises deploy/host/fetch-release.sh (hangar-fetch-release) against a
# local stand-in for GitHub's release downloads, an HTTP server on loopback
# (HANGAR_FETCH_RELEASE_BASE_URL), serving a small release made here with
# deploy/ packed as deploy.tar the way deploy/release.sh packs it.
#
# With the pinned gh it checks that the script refuses a wrong digest, a file
# changed after SHA256SUMS was made, a SHA256SUMS naming a path, a missing
# attestation bundle and a bundle that is not one, each with nothing left
# under the release's names.
#
# No hangar release carries an attestation from this repository's workflow
# until one is tagged, and a test cannot make one, so for the rest gh is
# swapped for a stand-in. It records what it is asked, checks it is asked
# exactly what a release needs (this repository, its release workflow, the
# tag, the commit, GitHub-hosted runners), and accepts a file when the
# bundle has a stand-in line with its digest; any other file it hands to the
# real gh, with the bundle's real lines. So: a genuine GitHub attestation of
# another repository's file (cli/cli's of the pinned gh tarball) reaches the
# real gh through the script and is refused; the very flags the script
# passed, replayed with the real gh against that attestation with only the
# repository, tag and commit translated to cli/cli's, accept it as it is and
# refuse it for another ref or another commit, so dropping --source-ref or
# --source-digest fails the test; a bundle that leaves out one file is
# refused; a good release is put in place, and refetching a tag replaces it
# whole, a refused refetch leaving the earlier one as it was.
#
# Run as root on a throwaway VM only: it writes /var/tmp/hangar and replaces
# /usr/local/bin/gh for a while. It needs to reach github.com and the TUF
# repositories gh reads its trust roots from.
set -euo pipefail

repo=$(cd "$(dirname "$0")/../.." && pwd)
host_dir=$repo/deploy/host
fetch=$host_dir/fetch-release.sh
# shellcheck source=deploy/host/versions.env
. "$host_dir/versions.env"
work=/var/tmp/hangar-testbed-fetch
top=/var/tmp/hangar
gh=/usr/local/bin/gh
tag=v0.0.1-rc.1
port=18089
base=http://127.0.0.1:$port
server_pid=

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo; echo "### $*" >&2; }

[ "$(id -u)" = 0 ] || fail "must run as root"

cleanup() {
	[ -z "$server_pid" ] || kill "$server_pid" 2>/dev/null || true
	[ ! -e "$work/gh.real" ] || mv -f "$work/gh.real" "$gh"
}
trap cleanup EXIT

# ensure_gh puts the pinned gh where the packages step does, as it does.
ensure_gh() {
	# shellcheck source=deploy/host/lib.sh
	. "$host_dir/lib.sh"
	# shellcheck source=deploy/host/steps/10-packages.sh
	. "$host_dir/steps/10-packages.sh"
	gh_is_pinned || install_gh
	gh_is_pinned || fail "the pinned gh did not install"
}

# make_release DIR VERSION writes a release into DIR: deploy/ as deploy.tar,
# VERSION, a stand-in binary, and SHA256SUMS over them.
make_release() {
	local dir=$1 version=$2
	rm -rf "$dir"
	mkdir -p "$dir"
	git -C "$repo/deploy" ls-files -z --cached --others --exclude-standard | LC_ALL=C sort -z >"$work/deploy.list"
	tar --mtime=@0 --owner=0 --group=0 --numeric-owner --mode=u+rwX,go+rX,go-w \
		-C "$repo/deploy" --null --no-recursion -T "$work/deploy.list" -cf "$dir/deploy.tar"
	echo "$version" >"$dir/VERSION"
	printf 'hangar %s\n' "$version" >"$dir/hangar"
	(cd "$dir" && sha256sum -- deploy.tar VERSION hangar >SHA256SUMS)
}

# serve DIR makes DIR what the stand-in serves for the tag.
serve() {
	rm -rf "$work/www/$tag"
	cp -r "$1" "$work/www/$tag"
}

digest_of() { sha256sum "$1/SHA256SUMS" | cut -d' ' -f1; }

# fetch_refused WHY DIGEST SAYS runs a fetch that must be refused, saying
# SAYS, with nothing left under the release's names, nor anything staged.
fetch_refused() {
	local why=$1 digest=$2 says=$3
	if HANGAR_FETCH_RELEASE_BASE_URL=$base "$fetch" "$tag" "$digest" >"$work/out.log" 2>&1; then
		fail "fetched $why"
	fi
	grep -qF -- "$says" "$work/out.log" || fail "$why: refused, but not for that: $(cat "$work/out.log")"
	assert_nothing_left "$why"
}

assert_nothing_left() {
	[ ! -e "$top/release/$tag" ] && [ ! -e "$top/deploy-$tag" ] || fail "$1: left a release behind"
	assert_nothing_staged "$1"
}

assert_nothing_staged() {
	[ -z "$(find "$top" -maxdepth 1 -name '.*' ! -name . 2>/dev/null)" ] || fail "$1: left staging behind: $(ls -a "$top")"
}

prepare() {
	step "prepare: the pinned gh, a release, a stand-in for GitHub"
	rm -rf "$work" "$top/release/$tag" "$top/deploy-$tag"
	mkdir -p "$work/www"
	ensure_gh
	make_release "$work/good" "$(printf '%040d' 1 | tr 0 a)"
	(cd "$work/www" && exec python3 -m http.server --bind 127.0.0.1 "$port") >"$work/server.log" 2>&1 &
	server_pid=$!
	for _ in $(seq 1 50); do
		curl -fsS -o /dev/null "$base/" 2>/dev/null && return 0
		sleep 0.1
	done
	fail "the stand-in for GitHub did not start"
}

test_refuses_without_real_attestations() {
	local d bad

	step "a wrong digest is refused"
	serve "$work/good"
	fetch_refused "a release with the wrong digest" "$(printf '%064d' 0)" "not $(printf '%064d' 0); refusing the release"

	step "a file changed after SHA256SUMS is refused"
	bad=$work/changed
	rm -rf "$bad"; cp -r "$work/good" "$bad"
	echo tampered >>"$bad/hangar"
	serve "$bad"
	fetch_refused "a file changed after SHA256SUMS" "$(digest_of "$bad")" "does not match its SHA256SUMS; refusing the release"

	step "a SHA256SUMS naming a path is refused"
	bad=$work/path
	rm -rf "$bad"; cp -r "$work/good" "$bad"
	echo "$(sha256sum "$bad/hangar" | cut -d' ' -f1)  ../hangar" >>"$bad/SHA256SUMS"
	serve "$bad"
	fetch_refused "a SHA256SUMS naming a path" "$(digest_of "$bad")" "a line that is not a checksum and a file name"

	step "a missing attestation bundle is refused"
	serve "$work/good"
	d=$(digest_of "$work/good")
	fetch_refused "a release without attestation.jsonl" "$d" "could not download attestation.jsonl"

	step "a bundle that is not one is refused"
	echo '{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}' >"$work/www/$tag/attestation.jsonl"
	fetch_refused "a release with a broken bundle" "$d" "SHA256SUMS of $tag has no attestation from"
}

# foreign_bundle FILE prints the SLSA provenance bundle GitHub's attestation
# API holds for FILE in cli/cli, as one JSON line. The API inlines a bundle
# or links to it snappy-compressed; the block format is decoded here.
foreign_bundle() {
	local sha
	sha=$(sha256sum "$1" | cut -d' ' -f1)
	curl -fsSL "https://api.github.com/repos/cli/cli/attestations/sha256:$sha" >"$work/foreign.json"
	python3 - "$work/foreign.json" <<'EOF' | grep . || fail "no provenance of the gh tarball from GitHub's API"
import base64, json, sys, urllib.request

def unsnappy(src):
    n, shift, i = 0, 0, 0
    while True:
        b = src[i]; i += 1
        n |= (b & 0x7f) << shift; shift += 7
        if b < 0x80:
            break
    out = bytearray()
    while i < len(src):
        tag = src[i]; i += 1
        kind = tag & 3
        if kind == 0:
            ln = tag >> 2
            if ln >= 60:
                k = ln - 59
                ln = int.from_bytes(src[i:i + k], "little"); i += k
            ln += 1
            out += src[i:i + ln]; i += ln
            continue
        if kind == 1:
            ln = ((tag >> 2) & 7) + 4
            off = ((tag >> 5) << 8) | src[i]; i += 1
        else:
            ln = (tag >> 2) + 1
            k = 2 if kind == 2 else 4
            off = int.from_bytes(src[i:i + k], "little"); i += k
        for _ in range(ln):
            out.append(out[-off])
    assert len(out) == n
    return bytes(out)

for a in json.load(open(sys.argv[1]))["attestations"]:
    b = a.get("bundle")
    if b is None:
        b = json.loads(unsnappy(urllib.request.urlopen(a["bundle_url"]).read()))
    statement = json.loads(base64.b64decode(b["dsseEnvelope"]["payload"]))
    if statement["predicateType"].startswith("https://slsa.dev/provenance/"):
        print(json.dumps(b))
        break
EOF
}

# The stand-in gh: `--version` as the real one. `attestation verify FILE
# ARGS` records its whole argument list as WORK/args/<name of FILE>, fails
# unless ARGS are exactly what a release needs, succeeds for a FILE whose
# digest the bundle has as "stand-in <sha256>", and otherwise asks the real
# gh, with the same ARGS on the bundle's real lines, its errors appended to
# WORK/gh-real.log. fetch-release.sh runs gh with no environment, so the
# paths are written in.
stand_in_gh() {
	mv "$gh" "$work/gh.real"
	mkdir -p "$work/args"
	sed -e "s|@WORK@|$work|g" -e "s|@TAG@|$tag|g" >"$gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
real=@WORK@/gh.real
if [ "$1" = --version ]; then exec "$real" --version; fi
[ "$1 $2" = "attestation verify" ] || exit 64
file=$3
printf '%s\0' "$@" >"@WORK@/args/$(basename "$file")"
shift 3
dir=$(dirname "$file")
want="--bundle $dir/attestation.jsonl --hostname github.com --repo YukinaMochizuki/hangar --signer-workflow YukinaMochizuki/hangar/.github/workflows/release.yml --source-ref refs/tags/@TAG@ --source-digest $(cat "$dir/VERSION") --deny-self-hosted-runners --format json"
[ "$*" = "$want" ] || { echo "stand-in gh: asked $*" >>@WORK@/gh-real.log; exit 65; }
grep -qx "stand-in $(sha256sum "$file" | cut -d' ' -f1)" "$dir/attestation.jsonl" && exit 0
grep -v '^stand-in ' "$dir/attestation.jsonl" >@WORK@/real-bundle.jsonl || true
args=()
for a in "$@"; do
	[ "$a" != "$dir/attestation.jsonl" ] || a=@WORK@/real-bundle.jsonl
	args+=("$a")
done
exec "$real" attestation verify "$file" "${args[@]}" 2>>@WORK@/gh-real.log
EOF
	chmod 755 "$gh"
}

# replay ARGS REF COMMIT runs the real gh on the gh tarball and cli/cli's
# bundle with the argument list fetch-release.sh gave gh for it (ARGS, as
# the stand-in recorded it), translated to cli/cli: its repository and
# release workflow for this repository's, REF for refs/tags/TAG, COMMIT for
# the release's VERSION. Anything else the script passed is passed as it is.
replay() {
	local argsfile=$1 ref=$2 commit=$3 version a i=0
	local -a args=()
	version=$(cat "$work/foreign/VERSION")
	while IFS= read -r -d '' a; do
		i=$((i + 1))
		# attestation verify FILE: the file is the tarball itself.
		if [ "$i" -le 2 ]; then args+=("$a"); continue; fi
		if [ "$i" = 3 ]; then args+=("$work/foreign/gh.tar.gz"); continue; fi
		case $a in
		*/attestation.jsonl) a=$work/foreign.jsonl ;;
		YukinaMochizuki/hangar/.github/workflows/release.yml) a=cli/cli/.github/workflows/deployment.yml ;;
		YukinaMochizuki/hangar) a=cli/cli ;;
		"refs/tags/$tag") a=$ref ;;
		"$version") a=$commit ;;
		esac
		args+=("$a")
	done <"$argsfile"
	"$work/gh.real" "${args[@]}"
}

# attest DIR writes the stand-in's bundle for every file of DIR but SKIP.
attest() {
	local dir=$1 skip=${2:-} f
	for f in "$dir"/*; do
		case $(basename "$f") in attestation.jsonl | "$skip") continue ;; esac
		echo "stand-in $(sha256sum "$f" | cut -d' ' -f1)"
	done >"$dir/attestation.jsonl"
}

test_with_stand_in_gh() {
	local d out v2

	stand_in_gh
	step "a genuine attestation of another repository's file reaches gh and is refused"
	# The pinned gh tarball, as a file of the release, with the bundle
	# GitHub's attestation API holds for it: valid, but cli/cli's. Every other
	# file has a stand-in line, so the tarball's is the check that decides.
	local bad=$work/foreign ref commit
	rm -rf "$bad"; cp -r "$work/good" "$bad"
	curl -fsSL -o "$bad/gh.tar.gz" "$HANGAR_GH_URL"
	(cd "$bad" && sha256sum -- deploy.tar VERSION hangar gh.tar.gz >SHA256SUMS)
	attest "$bad" gh.tar.gz
	foreign_bundle "$bad/gh.tar.gz" >"$work/foreign.jsonl"
	cat "$work/foreign.jsonl" >>"$bad/attestation.jsonl"
	# What the bundle is, as cli/cli's: the ref and commit it was made at.
	read -r ref commit < <("$work/gh.real" attestation verify "$bad/gh.tar.gz" --bundle "$work/foreign.jsonl" \
		--repo cli/cli --format json | python3 -c 'import json, sys
c = json.load(sys.stdin)[0]["verificationResult"]["signature"]["certificate"]
print(c["sourceRepositoryRef"], c["sourceRepositoryDigest"])') ||
		fail "the pinned gh does not accept cli/cli's own attestation of its tarball; the test proves nothing"
	serve "$bad"
	rm -f "$work/gh-real.log" "$work/args/gh.tar.gz"
	fetch_refused "a release attested by another repository" "$(digest_of "$bad")" "gh.tar.gz of $tag has no attestation from"
	if ! grep -q '^Error: ' "$work/gh-real.log" || grep -q '^stand-in gh: asked' "$work/gh-real.log"; then
		fail "the real gh did not refuse the tarball: $(cat "$work/gh-real.log" 2>/dev/null)"
	fi

	step "the flags it passes, with the real gh, refuse another ref and another commit"
	[ -f "$work/args/gh.tar.gz" ] || fail "the script never asked gh about the tarball"
	replay "$work/args/gh.tar.gz" "$ref" "$commit" >/dev/null 2>"$work/replay.log" ||
		fail "translated to cli/cli, the script's flags refuse cli/cli's own attestation: $(cat "$work/replay.log")"
	if replay "$work/args/gh.tar.gz" refs/tags/v0.0.0 "$commit" >/dev/null 2>"$work/replay.log"; then
		fail "the script's flags accept an attestation made at another ref"
	fi
	grep -q 'expected SourceRepositoryRef' "$work/replay.log" || fail "another ref refused, but not for its ref: $(cat "$work/replay.log")"
	if replay "$work/args/gh.tar.gz" "$ref" "$(printf '%040d' 0)" >/dev/null 2>"$work/replay.log"; then
		fail "the script's flags accept an attestation made at another commit"
	fi
	grep -q 'expected SourceRepositoryDigest' "$work/replay.log" || fail "another commit refused, but not for its commit: $(cat "$work/replay.log")"

	step "a bundle that leaves out one file is refused"
	attest "$work/good" hangar
	serve "$work/good"
	d=$(digest_of "$work/good")
	fetch_refused "a release whose bundle leaves out a file" "$d" "hangar of $tag has no attestation from"

	step "a good release is put in place"
	attest "$work/good"
	serve "$work/good"
	HANGAR_FETCH_RELEASE_BASE_URL=$base "$fetch" "$tag" "$d" >"$work/out.log" 2>&1 ||
		fail "a good release was refused: $(cat "$work/out.log")"
	assert_nothing_staged "a good fetch"
	(cd "$top/release/$tag" && sha256sum -c --quiet SHA256SUMS) || fail "the fetched release does not match its SHA256SUMS"
	[ "$(digest_of "$top/release/$tag")" = "$d" ] || fail "the fetched SHA256SUMS is not the release's"
	[ -f "$top/release/$tag/attestation.jsonl" ] || fail "the bundle was not kept with the release"
	[ -x "$top/deploy-$tag/host/bootstrap.sh" ] || fail "deploy.tar was not unpacked in $top/deploy-$tag"
	diff -r "$repo/deploy/host" "$top/deploy-$tag/host" >/dev/null || fail "the unpacked deploy/ differs from the one packed"
	out=$(stat -c '%U %a' "$top/release/$tag" "$top/deploy-$tag" "$top/release/$tag/hangar" "$top/deploy-$tag/host/bootstrap.sh" | tr '\n' ' ')
	[ "$out" = "root 755 root 755 root 644 root 755 " ] || fail "owners and modes: $out"
	grep -q "next: sudo $top/deploy-$tag/host/bootstrap.sh --release $top/release/$tag --dry-run" "$work/out.log" ||
		fail "the fetch did not say what to run next"

	step "a refused refetch leaves the earlier release as it was"
	echo tampered >>"$work/www/$tag/hangar"
	if HANGAR_FETCH_RELEASE_BASE_URL=$base "$fetch" "$tag" "$d" >"$work/out.log" 2>&1; then fail "fetched a tampered release over a good one"; fi
	(cd "$top/release/$tag" && sha256sum -c --quiet SHA256SUMS) || fail "a refused refetch changed the earlier release"
	assert_nothing_staged "a refused refetch"

	step "a refetch of the tag replaces the release whole"
	make_release "$work/next" "$(printf '%040d' 2 | tr 0 b)"
	attest "$work/next"
	serve "$work/next"
	v2=$(digest_of "$work/next")
	echo stale >"$top/deploy-$tag/stale"
	HANGAR_FETCH_RELEASE_BASE_URL=$base "$fetch" "$tag" "$v2" >"$work/out.log" 2>&1 ||
		fail "the refetch was refused: $(cat "$work/out.log")"
	[ "$(cat "$top/release/$tag/VERSION")" = "$(cat "$work/next/VERSION")" ] || fail "the refetch did not replace the release"
	[ ! -e "$top/deploy-$tag/stale" ] || fail "the refetch kept a file of the earlier deploy/"
	assert_nothing_staged "a refetch"

	step "there is no way to skip the check"
	for f in --no-verify --skip-attestation --insecure; do
		if HANGAR_FETCH_RELEASE_BASE_URL=$base "$fetch" "$f" "$tag" "$v2" >/dev/null 2>&1 ||
			HANGAR_FETCH_RELEASE_BASE_URL=$base "$fetch" "$tag" "$v2" "$f" >/dev/null 2>&1; then
			fail "$f was accepted"
		fi
	done

	mv -f "$work/gh.real" "$gh"
	rm -rf "$top/release/$tag" "$top/deploy-$tag"
}

prepare
test_refuses_without_real_attestations
test_with_stand_in_gh
echo
echo "PASS: fetch-release"
