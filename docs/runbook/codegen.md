# Code generation

Everything generated comes from `proto/` or from SQL, and is committed.
`make gen` regenerates it; `make check` fails if the committed output is stale.

## What runs

`make gen` runs two buf templates in order:

| Template | Plugin | Output |
|---|---|---|
| `buf.gen.yaml` | protoc-gen-go | `gen/go/...` messages |
| | protoc-gen-connect-go | `gen/go/.../<pkg>connect` clients and handler interfaces |
| | protoc-gen-es | `gen/ts/src/...` TypeScript messages and service descriptors |
| `buf.gen.hangar.yaml` | protoc-gen-hangar, `mode=registry` | `gen/go/hangar/registry/registry.go` |
| | protoc-gen-hangar, `mode=server` | `internal/apiserver/servicegen/services.go` |

`make gen` then runs sqlc (`sqlc.yaml`), which turns
`internal/store/sqlite/queries.sql` into `internal/store/sqlite/db`, checking
every query against the schema in `internal/store/sqlite/migrations`.

The second buf template runs separately because protoc-gen-hangar
(`tools/protoc-gen-hangar`) imports the generated `hangar.meta.v1` package to
read resource declarations, so that package must exist first. The first
template cleans its output directories; the second does not, so a hand-written
test next to `services.go` survives.

## Changing protoc-gen-hangar

1. Edit `tools/protoc-gen-hangar/main.go`.
2. `make gen`, then read the diff of `registry.go` and `services.go`.
3. `go test ./internal/apiserver/servicegen/` exercises the shims through real
   Connect calls against a recording backend; extend it when the shim gains
   behaviour.

## Changing the store schema

Add a new numbered goose migration under `internal/store/sqlite/migrations`;
never edit one that has shipped. The apiserver applies pending migrations at
startup. Then `make gen`, and run `go test ./internal/store/...`, which runs
the contract in `internal/store/storetest`.

## The API group domain

The suffix in every `apiVersion` (`hangar.dev`) is the `domain` option of the
registry plugin in `buf.gen.hangar.yaml`, and nowhere else.

## Troubleshooting

- **`go run ./tools/protoc-gen-hangar` fails to compile during `make gen`**:
  the plugin depends on `gen/go/hangar/meta/v1`. If that package is broken
  (for example after a bad edit to `proto/hangar/meta/v1`), fix the proto and
  rerun; the first template regenerates the package before the plugin builds.
- **The envelope test fails**: the error names the descriptor and what it
  expected. The shape rules live in `internal/apimachinery/schemacheck`.
