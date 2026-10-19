# Public endpoints: one hostname each under a user-content domain, served by an edge of its own

Status: accepted, 2026-10. Revised 2026-10 when hangar-edge took over
signing private endpoints' callers in from Cloudflare Access.

## Context

Machines have private IPv4 addresses and public IPv6 ones, so an
IPv4-only client cannot reach one, and ordinary HTTPS goes through
hangar-edge, the front door (`front-door.md`). User content must not share a
registrable domain with the console and the API, so it lives under its own
domain, `hangarlab.dev`. The wildcard certificate covers one level only.
Cloudflare has left the traffic path, and Access with it (from the region
the platform is used from, traffic through Cloudflare's proxy took a long
detour that added latency to every request), so Hangar signs the
callers of private endpoints in itself (`authentication.md`).

## Decision

- **A `PublicEndpoint`** (group `network`) exposes one TCP port of a
  Container or VirtualMachine, on its first interface, at
  `https://<endpoint>-<project>.<domain>`. It is `PRIVATE` unless set
  `PUBLIC`. Its status says its URL, the machine and node it reaches, and
  whether it is `Ready`: its machine runs. The domain is the apiserver's
  `--endpoint-domain`, `hangarlab.dev` by default. The backend speaks
  plain HTTP; the front door holds the TLS.
- **The hostname is claimed for good**. `-`
  joins two names that may hold it themselves (`web-my` of `demo-1`, `web`
  of `my-demo-1`), so the hostname label is claimed when the endpoint is
  made, like a project name, and never given to another endpoint: the claim
  records the project and the endpoint's name, so the same endpoint made
  again takes it back, and since project names are never reused, nobody
  else ever does. Admission refuses a label longer than 63 characters, one
  with `--` in its third and fourth places (kept for internationalized
  names), and one starting with `hangar-`, kept for the platform's own.
  Every endpoint label has a `-`, so it never equals a platform name without
  one. Endpoints count toward the tenant's `publicEndpoints` quota, one
  each; a container counts one for each public port, in place of the
  endpoint it owns for it (`internal/apiserver/quota.go`).
- **A container port with `public: true`** gets a `PRIVATE` endpoint the
  container owns, `<container>-<port name>`, which follows the port and goes
  with it. One of that name the container does not own is left alone, and
  the container's `PublicEndpoints` condition says so. Only TCP ports may be
  public. A VirtualMachine names no ports; its endpoints are made by hand.
- **Who may use a `PRIVATE` endpoint** is a permission of its own,
  `network.publicendpoints.access` on the endpoint's project, apart from
  reading or editing the endpoint.
  It is one of the reach permissions (`tenancy-and-iam.md`): the owner and
  editor roles and `platform.root` hold it, viewers do not, and it can be
  bound to service tokens. People are held to platform membership as
  everywhere.
- **The edge is a separate, unprivileged `hangar-edge`**: the front door sends it every request for a hostname
  under the domain, and it routes each by `Host`. User traffic never passes
  the apiserver, and the edge can scale apart from it.
  - It reads the API as the `Edge` principal (`authentication.md`): get,
    list and watch public endpoints, role bindings, projects and users,
    everywhere, and nothing else; it writes nothing. From them it keeps its
    own view of the endpoints and an authorizer (`authz.NewFromSource`), the
    same rules the apiserver runs, and serves nothing until both are read.
  - For a `PRIVATE` endpoint it finds who calls, then checks the permission
    on every request; no valid credential is 401 (a browser is sent to sign
    in instead), no permission 403. A `PUBLIC` endpoint takes anyone.
  - It streams responses as they come and passes upgrades (WebSockets)
    through. Nothing it let through outlives what let it through: an
    endpoint's connections are closed, and its requests ended, when it goes
    or anything it stands on changes (its port, target, machine or
    access), and the transport keeps its connections under a key of all of
    those, so a request after a change never takes one opened before.
- **Who calls a private endpoint** (as Access and oauth2-proxy do it). The edge takes one of:
  - its own session cookie for that host, `__Host-hangar-endpoint`, which a
    sign-in handoff from the console sets;
  - `Authorization: Bearer` with a CLI access token (`hga_`) or a service
    token (`hgr_`), for agents.
  Any other `Authorization` is the endpoint's own: the edge ignores it and
  passes it on. The edge asks the apiserver who holds a credential, on
  every request, over the edge socket (`EndpointAuthService`, served there
  alone and to the `Edge` principal alone; the socket's peer uid is the
  edge's, `authentication.md`). There is no cache: a revoked credential
  counts on the next request.
- **The handoff.** A browser (a GET or HEAD that is a navigation) without a
  valid session is sent to the console:
  1. The edge redirects to the console's `/auth/endpoint-login` (its
     `--console-url`, `https://console.<cp-domain>` by default) with the
     host and a state. The state, the path to return to and the
     host stay in this browser, sealed (AES-GCM, a key the edge makes at
     start and holds in memory) in a cookie of the endpoint's host,
     `__Host-hangar-endpoint-state-<id>`, for 10 minutes; the id lets
     several tabs hand off at once. The return path never leaves the edge.
  2. Endpoint-login, the apiserver's, signs the person in to the console
     first if they are not (`authentication.md`), and with a console session
     goes on without asking. It is a GET from another
     site by design: what it makes goes nowhere but to the endpoint's own
     callback. It checks that the host is exactly one PRIVATE endpoint's,
     `<label>.<domain>`, and that the person may use it; otherwise it shows
     a page and redirects nowhere.
  3. It issues a one-time code: 32 random bytes, stored as its SHA-256 only,
     bound to the host, the person, the state and the console session's
     login, valid 60 seconds, and sends the browser to
     `https://<host>/.hangar/callback` with the code and the state. Codes
     not yet exchanged are capped as the CLI's are, and at 20 per person,
     so no one fills the shared cap for others.
  4. The edge's callback takes the code only from the browser holding that
     state's cookie, for that host, in time, and trades it with the
     apiserver, which spends it whatever the outcome and checks the host,
     the state, the invitation and the permission again.
  5. The edge sets `__Host-hangar-endpoint` (Secure, HttpOnly,
     SameSite=Lax, for the whole host and no other) and sends the browser
     back to the path it asked for, checked as sign-in's return paths are.
  An edge restart makes a new key, so a handoff in progress then fails at
  the callback, and the next page load starts another.
- **An endpoint session** (`hge_`) counts on its host alone (the
  credential's audience). It lasts a day and never past the console
  session it came from, whatever its use, and is in that session's login:
  signing out of the console, or revoking the person's logins, ends it.
- **What ends a request in flight** to a PRIVATE endpoint, a stream or an
  upgrade included: its credential's expiry; any change to the bindings,
  projects or users the edge follows after which its caller may not use
  the endpoint; and, within 30 seconds (the edge checks every credential in
  flight again that often), the credential's revoke: a sign-out, a revoke of
  the person's logins, a service token rotated or deleted. A PUBLIC
  endpoint's requests have no credential to end with. An upgraded
  connection to any endpoint, PUBLIC or PRIVATE, also ends once it has
  carried no byte either way for 10 minutes (`--upgrade-idle-timeout`),
  counted on the edge's connection to the endpoint: an anonymous, silent
  WebSocket would otherwise hold a place in the front door for ever
  (`front-door.md`).
- **What never reaches an endpoint**. The edge keeps
  `/.hangar/` for itself on every endpoint, public ones too, however the path
  is spelled (case, dot segments, encoded dots, a backslash, a
  `;parameter`): the callback, and 404. Before forwarding, it removes every
  cookie whose name starts with `__Host-hangar` (in any case), Hangar's
  bearer tokens, every `Hangar-` header a client sent, and what Access used
  (`Cf-Access-*`, `CF_Authorization`). An endpoint answering with a
  `Set-Cookie` for one of Hangar's names, in any header block it sends (a
  1xx such as a 103 or a 101 included), has it dropped, and a `Set-Cookie`
  trailer is dropped whatever its name, so its answers cannot
  plant or clear one; its own scripts can still write one where none is
  set, on its own host alone, which the edge takes only if it issued that
  value for that host. No header tells an endpoint who calls; a signed one is
  possible later work.
- **Cookie tossing.** Every endpoint is a host under one domain, and a
  tenant's can set a cookie with `Domain=` the endpoint domain. The edge
  reads only `__Host-` names, which a browser takes only from the host
  itself, with no `Domain`, so no other endpoint can plant or overwrite
  them; a lookalike without the prefix is ignored. The endpoint domain is
  not on the Public Suffix List (at this scale the request would likely be
  refused). Instead the edge makes every
  cookie an endpoint sets host-only: it removes the `Domain` attribute,
  in any case and wherever it stands, from each `Set-Cookie` in every
  header block of an endpoint's answer, 1xx ones included, and keeps the
  rest as written. Browsers ignore `Set-Cookie` in trailers, so the edge
  drops it there rather than filtering it. Endpoints are one level
  under the domain, so a `Domain` equal to the endpoint's own host loses
  nothing. The known limits: a cookie a page's script sets
  (`document.cookie`) never passes the edge and can still carry the parent
  `Domain`, reaching every other endpoint; and browsers treat all endpoints
  as one site, so `SameSite` does not separate one tenant's endpoint from
  another's.
- **The edge reaches a machine through the node agent's dial socket**
  (`internal/edgedial`), a unix socket the agent makes, root's and the edge
  group's, 0660, which takes the edge's uid alone (`SO_PEERCRED`). A dial
  is an HTTP/1.1 upgrade naming an endpoint by project, name and uid, never
  an address or a port. The agent checks the endpoint exists, is not being
  deleted, and reaches a machine running on that node, then dials its port
  on the machine's first interface from the network's gateway in its
  namespace, as `hangar ssh` does (`vm-ssh.md`), and after `101` the
  connection carries the endpoint's bytes. Once the edge's side has ended,
  the endpoint has 30 seconds to finish what it sends back before both
  sides are closed. The node does no RBAC: the
  edge decides who may, and the socket reaches nothing an endpoint does not
  name. Not the apiserver's node tunnel: that would put user traffic on the
  control plane.
- **Security groups do not apply**, as for
  `hangar ssh`: while an endpoint exists, its machine's interface has a rule
  passing TCP from the gateway's address to the endpoint's port
  (`networking.md`). The endpoint is the explicit opening.
- **Cloudflare's side** is DNS alone: `hangar-cloudflare` makes the
  domain's wildcard A and AAAA records, DNS-only, at the host, whose front
  door takes every endpoint's traffic itself (`cloudflare-setup.md`). No
  Access object stands in front of an endpoint.
- **Machines on the host reach endpoints** by their public names, as
  anyone does: the node lets its networks open TCP 80 and 443 at the
  host's public addresses, the front door's, and nothing else of the host
  (`networking.md`).

## Consequences

- An endpoint is `Ready` once its machine runs, which can be a moment
  before the node has written the firewall rule; the edge's first requests
  then fail with 502.
- A request to an endpoint whose machine does not run is 503; to a
  hostname of no endpoint, 404. One the edge ends itself before the endpoint
  answers (the endpoint changed, or the caller's credential or grant ended)
  is 503 too, to try again.
- Endpoints work for one host: the edge dials the local node's socket. More
  hosts need the edge to pick the node of the endpoint's machine
  (`status.node`), and the socket a network transport.
- Every request to a PRIVATE endpoint asks the apiserver about its
  credential, one read on the edge socket: endpoints need the apiserver up,
  and an apiserver away is 503 there.
- The sign-in pages of the handoff, endpoint-login's and the callback's,
  are the plain placeholders in `internal/auth/pages`, as sign-in's are.
- Tests: `internal/edge` runs the whole handoff against a fake provider
  (`internal/auth/oidctest`), with the console and the edge on TLS; codes
  used twice, late, for another host or from another browser; open
  redirects; a session replayed on another host; lookalike cookie names;
  revoked grants and credentials ending upgrades; what is stripped; and no
  secret in the logs. `internal/apiserver` covers endpoint-login and
  `EndpointAuthService`. On a testbed, `test/testbed/flow`'s
  `TestTheDirectPath` runs a handoff and a public endpoint through the
  real front door to a real machine.
