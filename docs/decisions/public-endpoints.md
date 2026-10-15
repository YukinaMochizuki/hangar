# Public endpoints: one hostname each under a user-content domain, served by an edge of its own

Status: accepted, 2026-10.

## Context

Machines have private IPv4 addresses and public IPv6 ones, so an
IPv4-only client cannot reach one, and ordinary HTTPS should go through
Cloudflare anyway (`networking.md`). User content must not share a
registrable domain with the console and the API, so it lives under its own
domain, `hangarlab.dev`, which goes on the Public Suffix List. Cloudflare's
free certificate covers one level of wildcard only, and Access admits a
fixed set of people and service tokens (`authentication.md`).

## Decision

- **A `PublicEndpoint`** (group `network`) exposes one TCP port of a
  Container or VirtualMachine, on its first interface, at
  `https://<endpoint>-<project>.<domain>`. It is `PRIVATE` unless set
  `PUBLIC`. Its status says its URL, the machine and node it reaches, and
  whether it is `Ready`: its machine runs, and a `PUBLIC` one's Access
  application is in place. The domain is the apiserver's
  `--endpoint-domain`, `hangarlab.dev` by default. The backend speaks
  plain HTTP; Cloudflare holds the TLS.
- **The hostname is claimed for good** (decided by the project owner). `-`
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
  reading or editing the endpoint (decided by the project owner).
  It is one of the reach permissions (`tenancy-and-iam.md`): the owner and
  editor roles and `platform.root` hold it, viewers do not, and it can be
  bound to service tokens. People are held to platform membership as
  everywhere.
- **The edge is a separate, unprivileged `hangar-edge`** (decided by the
  project owner): cloudflared sends it every request for a hostname under the
  domain, on loopback, and it routes each by `Host`. User traffic never
  passes the apiserver, and the edge can scale apart from it.
  - It reads the API as the `Edge` principal (`authentication.md`): get,
    list and watch public endpoints, role bindings, projects and users,
    everywhere, and nothing else; it writes nothing. From them it keeps its
    own view of the endpoints and an authorizer (`authz.NewFromSource`), the
    same rules the apiserver runs, and serves nothing until both are read.
  - For a `PRIVATE` endpoint it verifies the Access token
    (`Cf-Access-Jwt-Assertion`) against the domain's wildcard application,
    and checks the permission; no token is 401, no permission 403. A
    `PUBLIC` endpoint takes anyone.
  - It strips the Access token, the `CF_Authorization` cookie (by name,
    in any case and spacing) and a service token's headers
    (`Cf-Access-Token`, `Cf-Access-Client-Id`, `Cf-Access-Client-Secret`)
    before the request goes on. Every endpoint shares one Access
    application, so an endpoint handed a visitor's token could replay it
    to any other endpoint that visitor may use.
  - It streams responses as they come and passes upgrades (WebSockets)
    through. Nothing it let through outlives what let it through: an
    endpoint's connections are closed, and its requests ended, when it goes
    or anything it stands on changes (its port, target, machine or
    access), and the transport keeps its connections under a key of all of
    those, so a request after a change never takes one opened before. A
    request to a PRIVATE endpoint, a stream or an upgrade included, ends
    when its caller's Access token expires, and when, at any change to the
    bindings, projects or users the edge follows, its caller may no longer
    use the endpoint. A PUBLIC endpoint's requests have no token to end with.
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
- **Security groups do not apply** (decided by the project owner), as for
  `hangar ssh`: while an endpoint exists, its machine's interface has a rule
  passing TCP from the gateway's address to the endpoint's port
  (`networking.md`). The endpoint is the explicit opening.
- **Cloudflare's side.** `hangar-cloudflare` makes the domain's wildcard
  DNS record, the tunnel's wildcard ingress rule to the edge, an Access
  application for the wildcard (the same people and service tokens as the
  API's), and a reusable bypass policy (`cloudflare-setup.md`). The
  wildcard ingress sets no `originRequest.access`, which would turn away a
  `PUBLIC` endpoint's requests: the edge checks a `PRIVATE` one's token
  itself.
  - Access cannot let one hostname of an application through without a
    login, so each `PUBLIC` endpoint gets an Access application of its own,
    for its hostname, with the bypass policy (decided by the project owner).
    Being more specific, Access applies it instead of the wildcard's. The
    apiserver makes and deletes it with a Cloudflare API token, a 0600 file
    (`--cloudflare-api-token-file`), named for no one use: service tokens
    will take it too. Without the token, `PUBLIC` endpoints are not `Ready`
    (`CloudflareNotConfigured`).
  - The application is named `hangar public endpoint: <hostname>`, so it is
    found again by name. Before one is made, the endpoint's status records
    that it is being made (`AccessApplication`, `Creating`); an apiserver
    that stops between making it and recording its id finds it by name
    and adopts it, and one that is deleted or made `PRIVATE` meanwhile has
    it deleted all the same. Every endpoint carries a finalizer until no
    application of it can be left.
  - The account holds 500 Access applications by default (Cloudflare One
    account limits, as of 2026-09; more on Enterprise only), shared with the
    platform's own, which bounds `PUBLIC` endpoints until quotas do.

## Consequences

- An endpoint is `Ready` once its machine runs, which can be a moment
  before the node has written the firewall rule; the edge's first requests
  then fail with 502.
- A request to an endpoint whose machine does not run is 503; to a
  hostname of no endpoint, 404.
- Endpoints work for one host: the edge dials the local node's socket. More
  hosts need the edge to pick the node of the endpoint's machine
  (`status.node`), and the socket a network transport.
