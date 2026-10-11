# WIRE_JSON breaking checks, even in alpha

Status: accepted, 2026-10.

## Context

Alpha APIs are conventionally free to change. But objects are stored as proto
binary in the database, and users keep YAML that uses JSON field names. A
reused field number or a renamed JSON field does not fail loudly: stored data
and existing YAML are silently read as something else.

## Decision

`buf breaking` runs on every PR against `main` with the `WIRE_JSON` rule set,
and failures block the merge.

It allows renaming messages and packages and changing generated code APIs. It
forbids reusing field numbers, changing field types, changing JSON names, and
deleting a field without reserving its number and name.

## Consequences

- Refactoring a field means reserving the old one and adding a new one.
- A genuinely breaking redesign is a new version (`v1alpha2`) with a conversion,
  which per-group versioning makes possible (see `api-versioning.md`).
