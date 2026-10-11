# Authentication

Status: accepted, 2026-10.

## Context

All public traffic goes through Cloudflare Access; Hangar has no login of its
own and no public pages. Agents need non-interactive credentials. The node is a
separate root process on the same host.

## Decision

- Each listener has a chain of authenticators, and the first that succeeds
  produces a `Principal` of type User, ServiceToken, Node or System.
- **Public listener** (loopback only, behind cloudflared): verifies the
  `Cf-Access-Jwt-Assertion` JWT against the team's JWKS, issuer and audience.
  It never trusts `Cf-Access-Authenticated-User-Email` or any other header a
  client could set. A verified email is a User; a service token's client ID is
  looked up and mapped to a ServiceToken principal bound to a tenant and,
  optionally, to projects.
- **Agents** use Cloudflare service tokens, which Hangar creates on request
  through the Cloudflare API. The Access policy admits listed emails or any
  valid service token; Hangar decides whom the token belongs to.
- **People on the CLI** log in through `cloudflared access login`; the CLI gets
  the JWT from `cloudflared access token` and caches it until expiry. Not a
  reimplementation of Cloudflare's undocumented CLI flow.
- **The node** connects over a unix socket and is identified by `SO_PEERCRED`.
  An mTLS authenticator joins the chain when there is more than one host.
- **Dev mode** takes the user from a header. It must be enabled explicitly and
  refuses to start on a non-loopback address.
- Controllers run in-process as the System principal.
- A third principal source, workload identity issued through the metadata
  service, is reserved for later.

## Consequences

Tests sign their own JWTs with a local JWKS issuer, so the real verification
path runs in tests.
