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
