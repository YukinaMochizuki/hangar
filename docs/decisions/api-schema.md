# The API serves its own schema

Status: accepted, 2026-10.

## Context

An agent given only the `hangar` CLI and an endpoint has to learn which kinds
exist and which fields each takes. That knowledge lives only in `proto/`: the
CLI lists kinds from its compiled-in registry, has no `explain`, and nothing
outside the repo documents a manifest. The proto comments are good user
documentation, but the descriptors protoc-gen-go embeds carry no comments, so
no running binary has them. Many comments also point at `docs/decisions/`,
which a user of the API cannot see beside the schema.

A CLI built before a kind was added should still be able to show that kind,
so the schema has to come from the server rather than from the CLI.

## Decision

- **`hangar.discovery.v1alpha1.DiscoveryService`**, written by hand like
  `NameService`, with two RPCs, both wrapped per `api-rpc-shape.md`:
  - `ListAPIResources` returns every kind: `api_version`, `kind`, `plural`,
    `short_names`, `scope`, and a one-line `description` (the first sentence
    of the message's comment).
  - `GetSchema(api_version, kind)` returns `json_schema`, the kind's JSON
    Schema (draft 2020-12) as a string. Unknown kinds fail with `NotFound`,
    and the error lists the kinds that do exist.
- **Any authenticated caller** may call both, including someone invited who
  has not signed up yet. Every caller sees every kind: the schema is public in
  the repo already.
- **The schema is JSON Schema** because every consumer reads it: the CLI's
  `explain`, the console, and MCP tool definitions later, with no protobuf
  library needed. Properties use the JSON names protojson writes; a field's
  comment becomes its `description`; enums list their values except the zero
  one; maps are `additionalProperties`; `status` is `readOnly`; 64-bit
  integers accept a number or a string of digits, as protojson does;
  `Empty` is an object with no properties, and any other of protobuf's own
  types fails generation until it is given its JSON form. The schema states
  what protojson writes; the API also reads proto field names, enum numbers
  and nulls, and a oneof's members exclude each other only in their
  descriptions.
- **protoc-gen-hangar generates the schemas** (`mode=schema`) from the
  descriptors, comments included, into `gen/go/hangar/schema`, one JSON file
  per kind, embedded and committed. A proto change then shows up in review as
  a diff to the schema users will see. The apiserver serves those files as
  they are.
- **References to docs in comments become links** when the apiserver serves
  them: `docs/<path>.md` becomes
  `https://github.com/YukinaMochizuki/hangar/blob/<revision>/docs/<path>.md`,
  where `<revision>` is the revision the apiserver was built from
  (`internal/buildinfo`), or `main` for a dirty or unknown build. A test fails
  if a resource's served schema still names a path under `internal/` or
  `cmd/`, or links a doc the repo does not have.
- **The CLI** adds:
  - `hangar api-resources [-o json|name]`, listing the server's kinds the way
    kubectl does.
  - `hangar explain KIND[.FIELD...] [--recursive] [-o json]`, which renders
    the schema, or prints the JSON Schema of that field.

  `get` and `apply` keep using the compiled-in registry. A manifest's
  mistake at a field names the `hangar explain` that describes it.

## Consequences

- A CLI older than a kind can list and explain that kind, but cannot apply it.
  Doing that would need descriptors, not JSON Schema. If it is ever wanted,
  `GetSchemaResponse` can gain a descriptor field without breaking anything.
- Changing a resource's comments changes `make gen` output, so the comments
  are reviewed as user-facing text.
- `docs/checklists/new-resource.md` gains a step: read the generated schema
  diff.
- The served schema is only as good as the proto comments. A field without a
  comment has no description.
