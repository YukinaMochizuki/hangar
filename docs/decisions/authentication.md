# Authentication

Status: accepted, 2026-10.

## Context

All public traffic goes through Cloudflare Access; Hangar has no login of its
own and no public pages. Agents need non-interactive credentials. The node is a
separate root process on the same host.

## Decision

- Each listener has a chain of authenticators, and the first that succeeds
  produces a `Principal` of type User, ServiceToken, Node, System, Local or
  Edge.
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
- **The node** connects over a unix socket and is identified by `SO_PEERCRED`;
  the socket belongs to one host, so the agent acts as the `Node` named by the
  apiserver's `--node-name`. An mTLS authenticator, carrying the node name in
  the certificate, joins the chain when there is more than one host. The
  tunnel the agent opens on the socket, over which the apiserver calls it,
  is that node by the same authentication (`node-tunnel.md`).
- **Root on the host** connects over a third unix socket, the admin socket
  (`/run/hangar/admin.sock`, root's, 0600), and is identified by
  `SO_PEERCRED` alone: uid 0 is the Local principal `local:root@<node>`,
  any other uid is refused. It holds `platform.root` without a RoleBinding,
  and no RoleBinding can name it, so it is the way back in when every
  binding is wrong or the Access setup is broken: Kubernetes' `admin.conf`.
  Nothing else runs on that listener, and its authenticator reads only
  what that listener recorded of the connection, never a header, so no
  other listener can produce the principal. Every request on it is logged
  with the caller's pid. systemd makes the socket (a socket unit) in a
  root-owned `/run/hangar` and passes it to the apiserver, which runs as an
  unprivileged user and could neither make a root-owned socket nor keep one
  in its own runtime directory, which systemd chowns to it and removes on
  stop. The node socket is made the same way. The CLI reaches it as a
  `unix://` endpoint with `local` auth, which sends no credential.
- **The edge** (`hangar-edge`, `public-endpoints.md`) connects over a
  fourth unix socket, the edge socket (`/run/hangar/edge.sock`, root's and
  the edge group's, 0660), and is identified by `SO_PEERCRED` alone: the
  uid of the edge's user (`--edge-uid`, never root) is the Edge principal
  `edge:<host>`, any other is refused. Its authenticator reads only what
  that listener recorded, as the admin socket's does, so no other listener
  can produce it. It may get, list and watch public endpoints, role
  bindings, projects and users, everywhere, and do nothing else, whatever
  the bindings say. Users of public endpoints are not principals of the API:
  the edge authenticates them itself, from the Access token of the
  endpoint domain's application. An mTLS authenticator joins its chain
  with more hosts, as the node's.
- Not a dev listener instead: a remotely-managed tunnel's routes are set
  in Cloudflare, and one pointed at the wrong origin would expose a
  listener that takes whoever the caller says it is.
- **Dev mode** takes the user from a header. It must be enabled explicitly and
  refuses to start on a non-loopback address. It also refuses any request
  carrying a header Cloudflare adds (`Cf-Warp-Tag-Id`, which cloudflared
  sets on every request it proxies, and `Cf-Connecting-Ip`, `Cdn-Loop` and
  `Cf-Ray` from its edge), and the host runs cloudflared only while the
  apiserver is in cloudflare auth, which the tunnel's unit checks itself at
  every start (`docs/runbook/host-bootstrap.md`), so a tunnel never
  reaches it.
- Controllers run in-process as the System principal.
- A third principal source, workload identity issued through the metadata
  service, is reserved for later.

## Consequences

Tests sign their own JWTs with a local JWKS issuer, so the real verification
path runs in tests.
