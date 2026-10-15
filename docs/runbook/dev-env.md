# Development environment

## What you need

Go and Node, at the versions in `go.mod` (the `go` line) and `.nvmrc`, plus pnpm
through corepack (`corepack enable pnpm`; the version is pinned by
`packageManager` in `package.json`). Nothing else is installed globally.

The devcontainer (`.devcontainer/`) provides all three. A workspace box built
from this repo gets it automatically.

## Where each tool is pinned

| Tool | Pinned in | Run with |
|---|---|---|
| protoc-gen-go, protoc-gen-connect-go | `go.mod` (`tool` block) | `go tool <name>` |
| buf, sqlc, golangci-lint | `tools/tools.mod` | `go tool -modfile=tools/tools.mod <name>` |
| protoc-gen-es | `package.json` | `node_modules/.bin/protoc-gen-es` |

The protoc-gen-go and connect plugins live in the main `go.mod` so their
versions move together with the runtime libraries the generated code imports.
The other CLIs live in `tools/tools.mod` so their large dependency trees stay out
of the module that SDK users import.

`make tools` prints every pinned version.

To upgrade a tool: `go get -tool <module>@<version>` (add
`-modfile=tools/tools.mod` for the second group), or edit `package.json` and
run `pnpm install`. Then `make check`.

## Everyday loop

```sh
make gen     # after editing anything under proto/
make check   # before every commit: lint, breaking, test, gen-check
```

`make check` is what CI runs, split into jobs that only run when their paths
change (`.github/workflows/ci.yml`).

## Running the apiserver and CLI locally

`make dev` runs the apiserver on `127.0.0.1:8080` with dev auth (the caller's
email comes from a header, so it refuses to listen anywhere but loopback) and
a `hangar.db` in the working directory. Only platform members may do
anything, so start a fresh database with yourself as its first admin:

```sh
rm -f hangar.db
go run ./cmd/hangar-apiserver --auth-mode=dev --bootstrap-admin you@example.com
```

Point the CLI at it with a dev context, and keep its config out of your real
one:

```sh
export HANGAR_CONFIG=/tmp/hangar-dev.yaml
go run ./cmd/hangar config set-context dev --endpoint http://127.0.0.1:8080 \
  --auth dev --user you@example.com --project web --use
go run ./cmd/hangar signup --username you   # once: the grant only invites you
go run ./cmd/hangar apply -f my-stack.yaml
go run ./cmd/hangar get containers -o json
```

A container in `my-stack.yaml` might read:

```yaml
apiVersion: compute.hangarlab.cloud/v1alpha1
kind: Container
metadata:
  name: api
spec:
  image: nginx:1
  env:
  - name: PORT
    value: 8080
  resources:
    cpu: 2
    memory: 512Mi
    disk: 10Gi
```

`apply` sends a YAML number written where the API has a string as the text
written, so `cpu: 2` and `value: 8080` need no quotes; `1.10` stays `1.10`.
A number anchored outside a string field and reached through a YAML alias
(`*name`) or merge key (`<<`) is not converted, so quote it where it is
anchored.

To stop a container and start it again without editing its manifest:

```sh
go run ./cmd/hangar stop ct api --wait     # waits for condition Stopped
go run ./cmd/hangar start ct api --wait    # waits for condition Ready
```

They change only `spec.runState`, so a later `apply` of a manifest without
`runState` starts the container again. Without a node, `--wait` waits until
`--timeout`.

To act as someone else, invite them first (a platform RoleBinding with role
`platform.member` for their email, applied as the admin), make a second
context with `--user`, and sign up in it.

To have a node, pass `--socket /tmp/hangar-node.sock --node-uid $(id -u)
--node-name dev` to the apiserver, which then registers a `Node` named `dev`,
and run the node agent against it:

```sh
go run ./cmd/hangar-node --name dev --socket /tmp/hangar-node.sock
```

The node turns Ready and machines get placed on it. Anything else on that
socket acts as the node too: it may report the status of the node and of the
machines placed on it, which is how tests play the node by hand.

## When something goes wrong

- **`gen-check` fails**: generated code differs from what the sources produce.
  Run `make gen` and commit the result. Never edit files under `gen/` by hand.
- **`breaking` fails**: the schema change would misread data already stored or
  YAML already written. Reserve the old field number and name and add a new
  field instead; see `docs/decisions/buf-breaking-wire-json.md`.
- **`pnpm install --frozen-lockfile` fails**: `package.json` changed without
  updating `pnpm-lock.yaml`. Run `pnpm install` and commit the lockfile.
- **The first `go tool` call is slow**: it compiles the tool once, then it is
  cached.
