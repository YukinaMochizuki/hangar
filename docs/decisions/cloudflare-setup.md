# Cloudflare's side is DNS alone, converged by a tool that owns what it created, recorded in a state file

Status: accepted, 2026-10. Revised 2026-10, when Cloudflare left the
traffic path.

## Context

HiNet routes traffic to Cloudflare through the US west coast, so the
project owner took Cloudflare out of the traffic path: the front door,
hangar-edge on the host, takes every connection itself
(`front-door.md`), and the apiserver signs people in itself
(`authentication.md`). What is left of Cloudflare is DNS: the records that
point the platform's names at the host, and the DNS API that answers the
front door's ACME challenges. Before, the tool also ran a Cloudflare Tunnel
and an Access application in front of the API; that path is gone, in one
switch, with no time with both in use. The records must still be
reproducible, checkable for drift, and safe to keep in an account that holds
other things.

## Decision

- **`hangar-cloudflare plan | apply | verify`** (`cmd/hangar-cloudflare`),
  idempotent like `deploy/host`, run by hand from a privileged box with a
  token that may read the zones and edit their DNS, nothing else.
  `internal/cloudflare` is a small `net/http` client for exactly the zone
  and DNS endpoints used; the official SDK would pull in every Cloudflare
  product for a handful of endpoints.
- **DNS-only records at the host.** `api.<zone>` and `console.<zone>`, and
  `*.<user-content zone>` for public endpoints, each an A and an AAAA
  record at the host's addresses, proxied off, TTL automatic. Nothing of
  Cloudflare stands between a client and the host.
- **Ownership is a state file of IDs.** The tool records each record it
  creates in `state.json` in its output directory and changes nothing whose
  ID is not there. An unrecorded A, AAAA or CNAME at one of its names makes
  it refuse before any change. Not tags or comments: a marker anyone can
  copy proves nothing. The records still carry a comment, for people
  reading the dashboard.
- **The switch from the tunnel changes the API's record in place.** The
  state the tunnel's version wrote (its `version: 1`) is read: its CNAME to
  the tunnel becomes the API's A record by one update of the same record,
  so the name is never without an answer, and the endpoints' wildcard
  CNAME the same. The tunnel, the Access application and policies and the
  smoke service token are kept in the state as retired, and named in every
  run's notes until someone deletes them.
- **The zones are fixed for a state file.** Changing one is refused rather
  than converged, since the old names' records would have to be taken down
  by hand.
- **It deletes nothing.** Removing a record, or a retired object, is a
  human's decision, in the dashboard.
- **The front door's DNS-01 token is not the tool's.** The edge holds a
  token of its own that may edit DNS in the two zones
  (`docs/runbook/cloudflare.md`).

## Consequences

- Losing `state.json` makes every record look foreign, so the tool refuses
  until the file is restored or rebuilt by hand
  (`docs/runbook/cloudflare.md`). That failure is loud and changes nothing,
  which is the side to fail on.
- Going back to the tunnel is the previous release's tool, with the state
  from before the switch: it turns the API's record back into the CNAME
  (`docs/runbook/cloudflare.md`, the rollback).
- The host's address is public in DNS, and nothing filters traffic before
  it: the front door's limits and the host's firewall are all there is
  (`front-door.md`).
