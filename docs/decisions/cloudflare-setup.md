# Cloudflare's side is converged by a tool that owns what it created, recorded in a state file

Status: accepted, 2026-10.

## Context

The public API sits behind a Cloudflare Tunnel and Access
(`authentication.md`). Their configuration lives in Cloudflare: the tunnel's
ingress, the DNS record, the Access application and its policies. It must be
reproducible, checkable for drift, and safe to run in an account that holds
other things. The apiserver will call the same API later (service tokens,
public endpoints), so the client has to be reusable Go.

## Decision

- **`hangar-cloudflare plan | apply | verify`** (`cmd/hangar-cloudflare`),
  idempotent like `deploy/host`, run by hand from a privileged box with a
  narrowly scoped API token. `internal/cloudflare` is a small `net/http`
  client for exactly the endpoints used; the official SDK would pull in every
  Cloudflare product for a handful of endpoints.
- **Ownership is a state file of IDs.** The tool records each object it
  creates in `state.json` in its output directory and changes nothing whose
  ID is not there. An unrecorded object with a colliding name or hostname
  makes it refuse before any change. Not tags or comments: tunnels, service
  tokens and reusable policies have no field to carry one, and a marker
  anyone can copy proves nothing. The DNS record still carries a comment, for
  people reading the dashboard.
- **Reusable Access policies**, attached to the application by ID and
  precedence. Cloudflare's application-scoped policies are legacy: they
  cannot be added to new applications.
- **The hostname is never routed before Access covers it.** A first apply
  creates the policies and the application, then the tunnel and its
  ingress, and the DNS record last. The ingress rule also sets
  `originRequest.access` (required, the team, the application's AUD), so a
  request that did not pass this application's Access check (the
  application deleted, or its destinations changed) never reaches the
  origin. On the host, the apiserver leaves `dev` auth
  before the tunnel token is installed (`docs/runbook/cloudflare.md`).
- **The hostname is fixed for a state file.** Changing it is refused rather
  than converged, since the old name's record, application and ingress would
  have to be taken down in the right order.
- **It deletes nothing.** Removing an object is a human's decision, in the
  dashboard.
- **Secrets go to files, never to output.** The tunnel token, and the
  smoke token's client secret (returned once, at creation), are written 0600
  to the output directory; the client's `Secret` type formats as a
  placeholder.

## Consequences

Losing `state.json` makes every object look foreign, so the tool refuses
until the file is restored or rebuilt by hand (`docs/runbook/cloudflare.md`).
That failure is loud and changes nothing, which is the side to fail on.
