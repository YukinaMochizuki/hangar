# Resources are declared in proto; codegen does the rest

Status: accepted, 2026-10.

## Context

Every kind needs the same envelope, the same six RPCs, a registry entry, a set
of permissions, and a CLI entry. Connect requires a concrete method per RPC,
which Go generics cannot produce. One maintainer cannot keep fifteen hand-written
copies consistent.

## Decision

- A resource is declared on its message with
  `option (hangar.meta.v1.resource) = { kind, plural, short_names, scope }`. This
  is the single source of truth for the registry, the CLI and authorization
  scope.
- `tools/protoc-gen-hangar` reads those declarations and generates, per kind,
  the Connect handler shim (delegating to the generic resource machinery), the
  registry entry, the permission table, and the CLI kind table.
- The envelope is enforced by a Go test, `internal/apimachinery/schemacheck`,
  which walks every `hangar.*` descriptor: fields 1 to 5 must be `api_version`,
  `kind`, `metadata`, `spec` (`<Kind>Spec`) and `status` (`<Kind>Status`), and
  `<Kind>Service` must have the six RPCs with the expected shapes.
- Generated code is committed, and CI fails if regenerating changes it. All
  plugins run locally at pinned versions; no remote plugins.

Not a buf lint plugin, for now: the same check function can be wrapped as one
later, and a test costs no extra binary.

## Consequences

Adding a kind means writing its proto and its controller. Everything else is
generated, and a malformed resource fails `go test`.
