# Hangar

Hangar is a small IaaS cloud built directly on Cloud Hypervisor. VMs and
containers both run as microVMs ("Machines") on a single host, managed through a
declarative, Kubernetes-style API (Connect + protobuf). One person maintains it,
and agents are first-class users of both the code and the API.

Status: early. The apiserver and the node agent run Containers as Cloud
Hypervisor microVMs on one host, deployed there by `deploy/host`.
`docs/architecture.md` describes the target.

## Layout

| Path | Contents |
|---|---|
| `proto/hangar/<group>/<version>/` | API schema, the single source of truth for resources |
| `gen/go`, `gen/ts` | Generated code. Committed, never edited by hand |
| `cmd/` | Binaries: `hangar-apiserver`, `hangar-node`, `hangar-edge` (the front door on :443, and public endpoints' proxy), `hangar-init`, `hangar` (CLI), `hangar-cloudflare` (Cloudflare DNS) |
| `internal/` | Implementation, split by function rather than by binary |
| `sdk/go` | Public Go client |
| `web/` | React console |
| `guest/` | Guest kernel config and OS image pipeline |
| `deploy/` | Host provisioning and systemd units; `deploy/cloudflare/` holds the example `hangar-cloudflare` config |
| `tools/` | Our own codegen and lint tools; `tools/tools.mod` pins third-party CLIs |
| `test/` | End-to-end harnesses and the testbed setup |
| `docs/` | See the index below |

This is the intended layout; directories appear as the work reaches them. Only
`gen/go` and `sdk/go` are importable from outside; everything else is
`internal/` and carries no compatibility promise.

## Commands

| Command | Does |
|---|---|
| `make gen` | Regenerate everything from `proto/` (and SQL, once it exists) |
| `make check` | Lint, test, and fail if `make gen` would change anything. Run before every commit |
| `make e2e` | Hermetic end-to-end tests (no KVM, no root) |
| `make e2e-host` | Real-host tests (L4): boots guests as root, on a disposable testbed VM only |
| `make dev` | Run the apiserver locally in dev auth mode |
| `make lint-sh` | shellcheck every shell script (part of `make check`) |
| `make guest-kernel` | Build the guest kernel (minutes, needs a C toolchain; not part of `make check`) |
| `make guest-initramfs` | Build the platform initramfs with hangar-init as `/init` and a static `mke2fs` (needs a C toolchain the first time; `INIT_MAIN=DIR` packs another main package) |
| `make tools` | Print the pinned tool versions |

The only toolchains needed are Go and Node (versions in `go.mod` and `.nvmrc`).
Every other tool is pinned in the repo (`go.mod`, `tools/tools.mod`, or
`package.json`) and run through the Makefile; never install one globally.

## Rules that are easy to break

- Generated code changes only through `proto/` (or SQL) plus `make gen`.
- All text in the repo (code, comments, docs, commit messages) is English.
- Comments follow `docs/conventions/comments.md`. Reviewers check it.
- A change that alters how something is done updates the matching checklist or
  runbook in the same PR.
- Refer to other docs by file path, never by section number.

## Docs index

| Doc | Read it when |
|---|---|
| `docs/architecture.md` | You need the map: components, data flow, who calls whom |
| `docs/decisions/` | You are about to question why something is the way it is |
| `docs/conventions/comments.md` | You write or review a code comment |
| `docs/conventions/testing.md` | You write or change a test |
| `docs/checklists/review.md` | You review a change, or before you ask for one |
| `docs/runbook/dev-env.md` | You set up a workspace or a tool misbehaves |
| `docs/runbook/codegen.md` | You change `proto/` or the code generator |
| `docs/checklists/new-resource.md` | You add a resource kind |
| `docs/checklists/new-controller.md` | You add or change a controller |
| `docs/runbook/host-bootstrap.md` | You set up a host or change `deploy/host/` |
| `docs/runbook/release.md` | You cut a release, change `.github/workflows/release.yml`, `deploy/release.sh` or `hangar-fetch-release`, or check a release from a workstation |
| `docs/runbook/cloudflare.md` | You set up or change DNS (`hangar-cloudflare`), create a Cloudflare token, or cut a host over from the tunnel (or back) |
| `docs/runbook/testbed.md` | You run or add a real-host test (L4), or change the guest boot path |
| `docs/runbook/guest-kernel.md` | You build the guest kernel or initramfs, change the kernel config, or bump its version |

When you add a doc, add it to this index.
