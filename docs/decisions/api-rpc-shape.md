# RPC shape

Status: accepted, 2026-10.

## Context

The API is Connect over protobuf, consumed by the CLI, the web console, the
node and agents. Both the Kubernetes and the Google AIP styles were candidates.

## Decision

Every kind has a typed `<Kind>Service` with `Get`, `List`, `Watch`, `Apply`,
`Delete` and `UpdateStatus`, following buf's STANDARD lint rules with no
exceptions:

- Requests and responses are always wrapper messages
  (`GetContainerResponse { Container container }`), so fields can be added to a
  response later without breaking anything.
- Objects are addressed by `project` and `name` fields; kinds outside a project
  use only `name`.
- `List` returns the store revision it read at. `Watch` takes
  `since_revision` and streams `ADDED`, `MODIFIED`, `DELETED` and `BOOKMARK`
  events, so list-then-watch never misses an event. `since_revision` 0 means
  "from now". Every watch opens with a `BOOKMARK` at the revision it starts
  after, which also makes the response begin before the first change.
- Asking for a revision that compaction removed fails with `OUT_OF_RANGE`; the
  client lists again and watches from the new revision.
- A watch with a label selector reports objects while they match. An object
  that changes so it no longer matches produces no event; clients that cache
  by selector must list again to notice.
- An error never says whether an object the caller may not see exists, nor
  anything about it beyond what the request named. The exception is creating
  an object with a global name (a tenant, project or role binding): a taken
  name has to be refused.
- `Apply` returns `warnings` and whether it `created` the object. `Delete`
  accepts an optional `expected_resource_version`.

Not AIP: returning the bare resource leaves no room for response metadata, and
AIP resource-name strings would be a second identity scheme next to
`metadata.name` and `metadata.project`.

## Consequences

Clients unwrap one level. Cross-kind references in specs need their own
convention, decided when the first one appears.
