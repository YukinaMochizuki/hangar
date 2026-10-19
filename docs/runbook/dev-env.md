# Development environment

## What you need

Go and Node, at the versions in `go.mod` (the `go` line) and `.nvmrc`, plus pnpm
through corepack (`corepack enable pnpm`; the version is pinned by
`packageManager` in `package.json`). Nothing else is installed globally.

The devcontainer (`.devcontainer/`) provides all three. Any environment that
builds a dev container from this repo gets it automatically.

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
email comes from a header, so it refuses to listen anywhere but a loopback
IP; `--listen localhost:8080` is refused too, use `127.0.0.1` or `[::1]`) and
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

Dev auth takes service tokens too, so an agent's path can be tried locally:

```sh
export HANGAR_TOKEN=$(go run ./cmd/hangar token create agent --role project.editor)
go run ./cmd/hangar whoami          # serviceToken:web/agent
unset HANGAR_TOKEN
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

To act as someone else, invite them first (`go run ./cmd/hangar admin invite
EMAIL` as the admin), make a second context with `--user`, and sign up in
it. Signing up opens a tenant and a project for them and makes the project
the context's.

To have a node, pass `--socket /tmp/hangar-node.sock --node-uid $(id -u)
--node-name dev` to the apiserver, which then registers a `Node` named `dev`,
and run the node agent against it:

```sh
go run ./cmd/hangar-node --name dev --socket /tmp/hangar-node.sock
```

The node turns Ready and machines get placed on it. Anything else on that
socket acts as the node too: it may report the status of the node and of the
machines placed on it, which is how tests play the node by hand.

## Running the console locally

The console (`web/`, `docs/decisions/console-design.md`) talks to the
apiserver `make dev` runs, through a proxy that sends the dev user's header,
so it is the console as that person sees it:

```sh
pnpm install
HANGAR_DEV_USER=you@example.com pnpm --dir web dev       # hot reload on :5173
pnpm --dir web build &&
  HANGAR_DEV_USER=you@example.com pnpm --dir web preview  # the built files on :4173
```

Without `HANGAR_DEV_USER` every call is unauthenticated, which is the
signed-out page; an email that holds no invitation shows the waiting page,
and an invited one without a User the signup page. `HANGAR_DEV_API` points
the proxy at another apiserver. Dev auth has no sign-in pages, so "Sign in
with Google" leads nowhere locally.

Both servers listen on `127.0.0.1` alone. Their proxy signs every call in
as `HANGAR_DEV_USER`, so anyone who can reach the port acts as that person,
platform administrator included. `HANGAR_DEV_HOST=0.0.0.0` widens it, for a
container whose port is forwarded to you alone; never on a shared network.

`vite preview` serves the console with the Content-Security-Policy the edge
sends; the dev server goes without it, as hot reload needs inline scripts.
`make check` runs the console's typecheck, lint and unit tests
(`make test-web`; vitest, `web/src/**/*.test.{ts,tsx}`, components' tests
in jsdom against an in-memory API, `web/src/test/harness.tsx`); CI also
builds it.
`pnpm --dir web exec vitest` reruns the tests as files change.

To screenshot pages for review, with the Chromium of a box's `browser`
add-on, against `vite preview`:

```sh
cd web && node scripts/shot.mjs /tmp/shots \
  'overview=/projects/my-demo?theme=dark@1440x900' 'phone=/projects/my-demo@390x844'
```

The script's header says what else a spec can ask for (a clip, a wait, a
click first).

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
