# Releases

A hangar release is what a host runs: the binaries, the guest kernel and
initramfs, the console as `console.tar`, `deploy/` as `deploy.tar`,
`VERSION` (the commit) and `SHA256SUMS` over all of them, as
`deploy/release.sh` builds them. Releases are cut by pushing a tag. The
workflow `.github/workflows/release.yml` builds the tag, attests every file
and publishes the files as the tag's GitHub Release. A host downloads and
checks a release itself with `hangar-fetch-release`
(`docs/runbook/host-bootstrap.md`). Nothing is built on a workstation and
copied to a host.

## Tags

Tags are semver with a `v`, `v0.x.y` for now. A pre-release adds `-rc.N`,
counting from 1: `v0.4.0-rc.1`,
then `v0.4.0-rc.2` if the first needed a fix, then `v0.4.0` on the commit
that passed. The workflow refuses any other tag name, and marks a release
whose tag has a suffix as a pre-release on GitHub.

A tag is released once. Never move or reuse a tag that has a release: hosts
may have fetched it, and its digest is in their history. Fix forward with
the next tag.

## Cutting a release

1. Merge everything the release carries to `main`, with `make check`
   passing and CI green.
2. Tag the commit with an annotated tag and push the tag alone:

   ```sh
   git fetch origin
   git tag -a v0.4.0-rc.1 -m v0.4.0-rc.1 origin/main
   git push origin v0.4.0-rc.1
   ```

3. Watch the run: `gh run watch` (or the repository's Actions tab). It has
   two jobs. `build` checks out the tag's commit and builds as
   `deploy/release.sh` does, on the `ubuntu-24.04` runner, with a
   read-only token and no cache from any other run: the guest kernel is
   built in the run every time (several minutes), and so is everything
   else. It hands the files over as a workflow artifact. `publish` runs
   none of the repository's code: it downloads the files, checks them
   against `SHA256SUMS` and the run's commit, attests every one with
   `actions/attest-build-provenance`, which records each attestation with
   GitHub and also writes the bundle the release carries as
   `attestation.jsonl`, creates the release as a draft, uploads every file
   and the bundle, and only then publishes it.
4. The release's notes state the commit and the digest of `SHA256SUMS`
   (`sha256sum SHA256SUMS`), and the command to fetch it on a host. Deploy a
   pre-release to a testbed first, then tag the same commit without the
   suffix.

The workflow needs no secrets. `build`'s token may only read the
repository, and the checkout keeps no credential. `publish`'s has exactly
`contents: write` (to publish the release), `id-token: write` (the OIDC
token Sigstore signs the attestations with) and `attestations: write` (to
record them with GitHub). Every action either job uses, those of
`.github/actions/setup` included, is pinned to a full commit, its version
in a comment beside it; bump one by its commit.

If a run fails before it publishes, first delete the draft release it may
have left (`gh release list` shows it as a draft; `gh release delete TAG`,
which leaves the tag), since a run refuses a tag that has any release, a
draft included. Then run it again for the same tag, from the tag:

```sh
gh workflow run release.yml --ref v0.4.0-rc.1 -f tag=v0.4.0-rc.1
```

A run from any other ref is refused: the attestations name the ref the
workflow ran from, and a host accepts only `refs/tags/TAG`. A run for a tag
that already has a release is refused too. Delete a release only if no host
fetched it; otherwise cut the next tag.

## What a host checks

`hangar-fetch-release TAG DIGEST` (`deploy/host/fetch-release.sh`) refuses
the release unless:

- `sha256sum SHA256SUMS` is `DIGEST`, the digest the notes state;
- every line of `SHA256SUMS` is a checksum and a plain file name, it lists
  `VERSION` and `deploy.tar`, and every file it lists matches it;
- every file, `SHA256SUMS` included, has an attestation in
  `attestation.jsonl` that `gh attestation verify` accepts for this
  repository, signed by `.github/workflows/release.yml` on a GitHub-hosted
  runner, for `refs/tags/TAG`, at the commit `VERSION` names.

`gh attestation verify --bundle` needs no GitHub login: the bundle carries
the signature, the certificate and the transparency log entry. It still
reaches out for the trust roots, which it keeps current from Sigstore's
and GitHub's TUF repositories (`tuf-repo-cdn.sigstore.dev`,
`tuf-repo.github.com`). The downloads themselves come from `github.com` and
`release-assets.githubusercontent.com`, over HTTPS only. The `gh` it runs is
`/usr/local/bin/gh`, pinned in `deploy/host/versions.env`; the script
refuses a version older than the first that matches `--signer-workflow`
exactly.

`HANGAR_FETCH_RELEASE_BASE_URL` points the downloads at a stand-in for
GitHub, for `test/testbed/fetch-release.sh`. The checks stay the same; no
flag or variable skips one.

## Verifying a release from a workstation

To check a release before deploying it, or to see what a host would accept,
with `gh` (2.102.0 or newer) and a GitHub login:

```sh
TAG=v0.4.0-rc.1
dir=$(mktemp -d) && cd "$dir"
gh release download "$TAG" -R YukinaMochizuki/hangar
sha256sum SHA256SUMS            # the digest in the release notes
sha256sum -c SHA256SUMS
for f in SHA256SUMS $(cut -d' ' -f3 SHA256SUMS); do
  gh attestation verify "$f" -R YukinaMochizuki/hangar \
    --signer-workflow YukinaMochizuki/hangar/.github/workflows/release.yml \
    --source-ref "refs/tags/$TAG" --source-digest "$(cat VERSION)" \
    --deny-self-hosted-runners || echo "REFUSED: $f"
done
```

Without a login, add `--bundle attestation.jsonl` to each `gh attestation
verify`, as the host does, and download the files with `curl` from
`https://github.com/YukinaMochizuki/hangar/releases/download/$TAG/`.
`VERSION` must name the commit the tag points at
(`git rev-parse "$TAG^{commit}"`).

## Building a release by hand

`make release` runs `deploy/release.sh` on the checkout: a testbed installs
what it writes (`docs/runbook/testbed.md`), and two builds of one commit
with one toolchain give the same `SHA256SUMS`. A release built by hand is
for tests only: it has no attestation, so `hangar-fetch-release` would
refuse it, and a host never installs one.
