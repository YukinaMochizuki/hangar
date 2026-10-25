# Releases

A hangar release is what a host runs and the CLI people run against it, as
`deploy/release.sh` builds them. Releases are cut by pushing a tag. The
workflow `.github/workflows/release.yml` builds the tag, attests every file
and publishes the files as the tag's GitHub Release. A host downloads and
checks a release itself with `hangar-fetch-release`
(`docs/runbook/host-bootstrap.md`). Nothing is built on a workstation and
copied to a host.

## What a release holds

| File | For |
|---|---|
| `hangar-host_TAG_linux_amd64.tar.gz` | A host: the binaries, the guest kernel and initramfs, the converter initramfs (`converter.cpio`, `docs/runbook/converter.md`), the swtpm bundle (`swtpm.tar`, `docs/runbook/swtpm.md`), the console as `console.tar`, `deploy/` as `deploy.tar`, `VERSION` and their own `SHA256SUMS`, flat, the directory `bootstrap.sh --release` takes once unpacked |
| `hangar_TAG_OS_ARCH.tar.gz` | The CLI, `hangar` alone, for `linux` and `darwin` on `amd64` and `arm64` |
| `hangar_TAG_windows_ARCH.zip` | The CLI for Windows, `hangar.exe` alone, on `amd64` and `arm64` |
| `install.sh` | Installs the CLI on Linux and macOS (`deploy/cli/install.sh.in`, the tag baked in) |
| `install.ps1` | Installs the CLI on Windows (`deploy/cli/install.ps1.in`, the tag baked in) |
| `VERSION` | The commit, the same as the host tar's |
| `SHA256SUMS` | Over every file above; its digest is in the release notes |
| `attestation.jsonl` | The build-provenance attestation of every other file |

A host takes `SHA256SUMS`, `VERSION` and the host tar, and nothing else.
Releases up to `v0.1.0-rc.4` have the host's files loose instead, and no
CLI but linux/amd64's `hangar`; going back to one, or forward from one,
fetches by hand (`docs/runbook/host-bootstrap.md`).

## The CLI

The console's overview shows the line that installs the CLI and makes a
context for its API, for the release it came with
(`web/src/lib/cli-install.ts`), for macOS and Linux or for Windows, the
browser's own first:

```sh
curl -fsSL https://github.com/YukinaMochizuki/hangar/releases/download/TAG/install.sh | sh -s -- --endpoint https://api.<cp-domain>
hangar login
```

```powershell
& ([scriptblock]::Create((irm https://github.com/YukinaMochizuki/hangar/releases/download/TAG/install.ps1))) -Endpoint https://api.<cp-domain>
hangar login
```

`install.sh` installs the newest published release of its own tag's
`vMAJOR.MINOR` that is not older than that tag, asking GitHub's API, so a
CLI fix reaches people without the host being upgraded; when the API does
not answer, or that release has no archive, it installs its own tag. A
pre-release is a candidate only when its own tag is one. `--version TAG`
installs exactly TAG. It checks the archive against that release's
`SHA256SUMS` before installing it into `~/.local/bin` (`/usr/local/bin` for
root, `--dir` for elsewhere). It does not check the attestation, which
needs `gh`; it prints the command that does. `--endpoint` makes a context
named after the endpoint's host without `api.` (`--context` names it) and
makes it current; a context of that name and endpoint is kept as it is, and
one with another endpoint is left alone.

`install.ps1` does the same on Windows, in Windows PowerShell 5.1 or
PowerShell 7, with `-Version`, `-Endpoint`, `-Context` and `-Dir`. It
installs `hangar.exe` from the release's zip into
`%LOCALAPPDATA%\Programs\hangar` (`-Dir` or `HANGAR_INSTALL_DIR` for
elsewhere) and adds that directory to the user's `PATH` unless it is on it.
Without arguments it runs as `irm URL | iex`; with them, as the console's
line does, through `[scriptblock]::Create`.

So **releases of one minor keep the CLI and the API compatible**: a CLI of
`v0.4.y` works against an apiserver of any `v0.4.x`. A change that breaks
that takes the next minor.

`test/install` runs `install.sh` against a stand-in for GitHub (`make
test`), and on Windows `install.ps1` through the same cases; CI's
`install-windows` job runs those under both PowerShells.

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
  `VERSION` and the host tar, and those two match it;
- `SHA256SUMS`, `VERSION` and the host tar each have an attestation in
  `attestation.jsonl` that `gh attestation verify` accepts for this
  repository, signed by `.github/workflows/release.yml` on a GitHub-hosted
  runner, for `refs/tags/TAG`, at the commit `VERSION` names;
- and, only then unpacked, the host tar holds plain files with plain names
  alone, its own `SHA256SUMS` lists exactly those files and they match it,
  and its `VERSION` is the release's.

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

`make release` runs `deploy/release.sh` on the checkout. It writes `host/`,
the host's files unpacked, which a testbed installs
(`docs/runbook/testbed.md`), and `dist/`, the files a release publishes.
`HANGAR_RELEASE_TAG=TAG` names them for TAG; without it they are named
`dev-<revision>` and the console shows no install line. Two builds of one
commit and tag with one toolchain give the same `SHA256SUMS`; the Windows
zips are packed by `tools/reprozip` for that. A release built by hand is
for tests only: it has no attestation, so `hangar-fetch-release` would
refuse it, and a host never installs one.
