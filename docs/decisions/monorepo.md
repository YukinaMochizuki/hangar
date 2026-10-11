# One repository, one Go module

Status: accepted, 2026-10.

## Context

One person maintains Hangar. Most changes cross layers: a schema change touches
the proto, the server, the CLI and the web console at once.

## Decision

Everything lives in this repository: Go binaries, proto, the web console, the
guest kernel config and build scripts (not the kernel source), the OS image
pipeline, and host provisioning. Heavy artifacts (kernel, images) are built by
CI as release artifacts, behind path filters.

There is one Go module. Third-party CLIs are pinned in a separate modfile
(`tools/tools.mod`) so they do not leak into the module SDK users import.

## Consequences

- A cross-layer change is one PR and cannot ship half-applied.
- CI must filter by path so a web change does not rebuild a kernel.
- When the Go SDK needs independent versioning, `gen/go` and `sdk/go` become
  their own module without moving.
- A component that grows its own contributors can be split out with
  `git subtree split`.
