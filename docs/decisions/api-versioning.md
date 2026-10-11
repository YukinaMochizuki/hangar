# API groups and versions

Status: accepted, 2026-10.

## Context

Hangar is meant to grow into a full public cloud, so its API areas will mature
at different speeds.

## Decision

- Proto packages are split by group: `hangar.meta.v1` (the shared envelope, not
  a resource group), `hangar.tenancy`, `hangar.iam`, `hangar.compute`,
  `hangar.storage`, `hangar.network`, each with its own version.
- The `apiVersion` users write is `<group>.hangar.dev/<version>` (for example
  `compute.hangar.dev/v1alpha1`), mapped 1:1 from the proto package. The
  registry derives one from the other mechanically.
- Each kind lives in its own `.proto` file with its service.

## Consequences

- One group can move to `v1` while another stays in alpha.
- A breaking redesign means a new version plus a conversion, not an edit in
  place (see `buf-breaking-wire-json.md`).
- The `hangar.dev` suffix is a placeholder until the project's domain is chosen.
  It is stored in every object, so it must be settled before real users arrive;
  changing it afterwards needs a data migration.
