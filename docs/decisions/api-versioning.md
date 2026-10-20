# API groups and versions

Status: accepted, 2026-10.

## Context

Hangar is meant to grow into a full public cloud, so its API areas will mature
at different speeds.

## Decision

- Proto packages are split by group: `hangar.meta.v1` (the shared envelope, not
  a resource group), `hangar.tenancy`, `hangar.iam`, `hangar.compute`,
  `hangar.storage`, `hangar.network`, each with its own version.
  `hangar.monitoring` holds no resource, only the platform's
  `MonitoringService` (`monitoring.md`).
- The `apiVersion` users write is `<group>.hangarlab.cloud/<version>` (for
  example `compute.hangarlab.cloud/v1alpha1`), mapped 1:1 from the proto
  package. The registry derives one from the other mechanically.
- The suffix is `hangarlab.cloud`, the control-plane domain, never
  `hangarlab.dev`, which holds user content. The same suffix prefixes the
  system's finalizers and its reserved label and annotation keys.
- Each kind lives in its own `.proto` file with its service.

## Consequences

- One group can move to `v1` while another stays in alpha.
- A breaking redesign means a new version plus a conversion, not an edit in
  place (see `buf-breaking-wire-json.md`).
- The suffix is stored in every object and in every finalizer, so changing it
  needs a data migration. It replaced the placeholder `hangar.dev` before any
  user's data existed; the only store holding the old one was emptied.
