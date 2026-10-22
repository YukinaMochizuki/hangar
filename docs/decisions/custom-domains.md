# Custom domains: a tenant's own domain, delegated for DNS-01, served by the edge

Status: accepted, 2026-10.

## Context

A PublicEndpoint is served at `<endpoint>-<project>.<endpoint domain>`
(`public-endpoints.md`). Tenants want their sites at their own names, as
Vercel and Fly let them. The front door gets every certificate over ACME
DNS-01 alone, with a Cloudflare token that writes only the platform's two
zones, and serves nothing for ACME on :80 (`front-door.md`); Hangar cannot
write a tenant's DNS. Many tenants share the host, so a name must be
proven to be a tenant's before anything is served or certified for it:
otherwise a stale record pointing at the host would let anyone take it.

## Decision

- **A `CustomDomain`** (group `network`, global, as a Project is) is a
  tenant's domain, such as `example.com`: `spec.tenant` and `spec.domain`,
  both immutable, and `spec.projects`, the tenant's projects whose
  endpoints may serve under it (empty is all of them). Writes are at the
  tenant: whoever may write the tenant's objects there, its owner, writes
  its domains; project roles do not. Reads are at the tenant too, and also
  at each project the domain lets serve under it (owner's decision), so a
  project's people, its viewers included, see the domains they may use
  and why one does not serve yet, and no other (the kind's `readableBy`).
  A project's watch follows the allow-list as it is when each change is
  sent: a domain that stops letting the project in sends it nothing more,
  not its deletion. Domains count toward the plan's
  `domains` limit, which only a Plan sets. The kind is not `Domain`:
  `registry.Domain` is the API group's suffix.
- **What a domain may be** (`internal/customdomain`): a hostname in lower
  case with no trailing dot, not a public suffix, and not in the site of
  the endpoint domain or the control plane's. A tenant has a domain once;
  it may also have one under it (`example.com` and `b.example.com`), so a
  name two labels down is servable.
- **The first tenant to verify a name has it** (owner's decision). A
  domain not verified keeps no one from anything: other tenants may have
  the same domain, one under it or one above it, unverified too, so no one
  can hold a name they never proved theirs. Verifying is done under the
  `customDomains` lock, and refused (`HeldByAnotherTenant`) while another
  tenant has a verified domain that is it, under it or above it; a new
  domain overlapping one another tenant verified is refused outright. A
  verified domain is its tenant's until they delete it: there is no
  transfer to a later owner of the name, who needs an administrator.
- **Proof and certificates are one record.** The domain's status names a
  challenge target, `<label>.acme.<endpoint domain>`, whose label is made
  from the CustomDomain's uid. The tenant makes
  `_acme-challenge.<domain>` a CNAME of it. The apiserver looks for that
  CNAME when the domain changes and every minute until it finds it, then
  sets `Verified`, which stays true. The same CNAME delegates the domain's
  DNS-01 challenges into the endpoint domain's zone, which the edge's
  Cloudflare token already writes: no new credential, and :80 still serves
  no ACME. A target is one CustomDomain's alone, so a CNAME left behind by
  a deleted one proves nothing for the next.
- **It asks the domain's own nameservers** (`internal/dnscheck`), found
  through `--custom-domain-resolver` or the system's resolvers, without
  recursion, following delegations below the zone; only when none of them
  answers does it take a resolver's answer, and the status says whose it
  was. A resolver keeps "no such record" for the zone's negative TTL, often
  half an hour, and a tenant usually adds the record after making the
  domain, so a resolver's answer would hold the domain back that long: it
  did on the first production domain, whose host's resolver and OVH's
  behind it both remembered the record missing.
- **Checking now.** `CustomDomainCheckService.CheckCustomDomain` (a
  service of its own, as the token issuer is) does one such check at once,
  writes the status and returns the domain; a verified one is returned as
  it is, nothing looked up. It takes `network.customdomains.apply` at the
  tenant, as changing the domain does: it writes the status and makes the
  apiserver ask the domain's nameservers, so a domain's readers in its
  projects may not. `hangar check customdomain NAME` calls it, exiting 1
  while not verified, and `--wait` asks again every 10 seconds; the
  console's domain page has Check now.
- **What is served.** A verified domain gets two certificates, the
  domain's and its wildcard's, kept by the front door like its own
  (`internal/edge/front/certs.go`: one certmagic config per domain,
  answering its challenges at the target, on the shared cache and
  storage). Undelegating a domain also ends an order for it in progress.
  An endpoint names its hostnames in `spec.hostnames`: each the domain or
  one label below one of its tenant's domains that lets its project, held
  by no other endpoint. A hostname is under the longest of its tenant's
  domains that serves it, verified or not (`customdomain.Of`), so a domain
  made under another takes the hostnames under it. Only a PUBLIC endpoint
  may have them: the sign-in handoff of a PRIVATE one is bound to its own
  hostname (`public-endpoints.md`). Its status lists the URLs that serve,
  those whose domain is verified and lets the project, and a `Hostnames`
  condition says why the others wait.
- **The edge decides by the same rule, itself.** It routes a custom
  hostname only while a PUBLIC endpoint asks for it and the domain it is
  under, of the endpoint's project's tenant, is verified and lets the
  project; it reads the project's tenant from its own view. It does not
  rely on admission alone: the endpoints of a tenant being deleted can
  outlive its domains while its projects empty, and the name can be
  another tenant's by then. It ends requests in flight on a hostname that
  stops serving. Until it has read the custom domains no custom hostname
  serves, and nothing else waits on them.
- **Admission takes one lock**, `customDomains`: every write of a
  CustomDomain, every apply of an endpoint with hostnames (the kind's
  `applyLocks`) and every verification hold it, so no two tenants have
  overlapping verified domains, a hostname has one endpoint, and a domain
  is not deleted, nor made or changed to leave out a project, while an
  endpoint of it serves under it (refused, saying which). A tenant going
  takes its domains with it.
- **What the tenant's DNS needs**, which the status lists, each record
  saying whether it is required: the challenge CNAME, always, kept for
  renewals; to serve hostnames below the domain, `*.<domain>` or each
  hostname a CNAME of `cname.<endpoint domain>`; and to serve the domain
  itself, a CNAME of it at the domain. Its type is given as CNAME, not
  ALIAS or ANAME, which are some providers' names alone: Cloudflare takes
  a CNAME there and flattens it, and the record's text names Route 53's
  Alias, ALIAS, ANAME and the A and AAAA records of `cname.` for providers
  that refuse it. Every record is DNS only: behind a proxy (Cloudflare's
  orange cloud) the challenge CNAME is not seen and traffic does not come
  straight to the host, and the console says so above the records.
  `cname.` resolves through the endpoint domain's wildcard record; it has
  no `-`, so no endpoint's label is it.
- **Cookies.** At a custom hostname the edge keeps an endpoint's
  `Set-Cookie` `Domain` attribute when it is the custom domain the
  hostname is under, or under it: that reaches only the tenant's own
  names. Any other `Domain` goes, as at the endpoint's own hostname: a
  parent such as `example.com` can hold sibling domains of other tenants
  (`a.example.com`, `b.example.com`). Within a tenant, a cookie for its
  domain reaches every project's hostnames under it; `spec.projects` says
  who may serve there, not who is kept apart. Hangar's own cookie names,
  its tokens and headers are still kept from the endpoint, and `/.hangar/`
  is still the edge's.

## Consequences

- Certificates come before the traffic: once the challenge CNAME is in
  place, a tenant can move a live site without a gap.
- Whether a domain's certificates have been issued is not in the API: the
  edge writes nothing. It is in the edge's log and its certificates'
  expiry metric.
- A domain whose challenge CNAME goes away keeps serving until its
  certificates expire, then fails its handshakes; it stays verified.
- PRIVATE endpoints at custom hostnames would need the handoff's
  endpoint-login and callback to take them, which is later work.
- One Let's Encrypt account holds every tenant's certificates, so its rate
  limits are shared: two orders a domain, at its creation and at each
  renewal.
- A connection opened for one custom domain's certificate may carry
  requests for any endpoint hostname, as one for the endpoints' wildcard
  may: the front door's 421 tells its sites apart, not endpoints.
- Tests: `internal/customdomain` (the rules); `internal/apiserver`
  (`customdomains_test.go`: admission, who verifies first and two tenants
  verifying at once, racing hostname claims, nested domains, who may read
  and write, verification against a DNS server, the plan);
  `internal/edge` (`domains_test.go`: serving only once verified and only
  under the endpoint's own tenant's domain, requests ended when a hostname
  stops, cookies); and `internal/edge/front` (certificates from pebble
  through a delegating CNAME, renewal answering there, custom hosts'
  routing). An L4 test on a real host does not cover custom domains yet:
  the testbed's DNS, inside hangar-edge, cannot be given a domain's CNAME.
