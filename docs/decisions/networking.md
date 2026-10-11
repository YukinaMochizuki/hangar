# Networking

Status: accepted, 2026-10.

## Context

One OVH host with one IPv4 address and an IPv6 /64. OVH's IPv6 is on-link with
MAC filtering at the switch. Ordinary HTTPS should go through Cloudflare;
anything else needs a reachable address.

## Decision

- **A simple VPC first**: `Network` (each project gets a `default`), `Subnet`
  with user-chosen CIDRs that may overlap across networks, multiple NICs per
  machine, security groups, NAT egress, internal DNS
  (`<name>.<network>.internal`), and a metadata service at 169.254.169.254.
  Peering, route tables and shared VPCs come later.
- **Each `Network` is its own network namespace** (that is what allows
  overlapping CIDRs) with a bridge and nftables. The node creates tap devices
  and hands their file descriptors to Cloud Hypervisor. The apiserver does IPAM
  and assigns static addresses; there is no DHCP. All of it sits behind a
  network driver interface so VXLAN or OVN can replace it.
- **`SecurityGroup`** works as in AWS: attached to NICs, several per NIC,
  sources are CIDRs or other groups, stateful, allow rules only, inbound denied
  by default. Each network has a `default` group. One nftables set per group.
- **Public IPv6** is an `Address` resource, routed to the machine with NDP proxy
  on the host (no bridging, because of the MAC filtering).
- **HTTPS exposure** is a `PublicEndpoint`: one platform tunnel and a wildcard
  DNS record lead to cloudflared, then to Hangar's own edge proxy, which routes
  by `Host` into the right namespace. Endpoints are `private` by default
  (behind Access, with the proxy checking the tenant) and become `public` only
  when set explicitly. User content uses a different domain from the console,
  and only first-level wildcard hostnames (Cloudflare's free certificate does
  not cover deeper levels). A container port with `public: true` creates an
  endpoint automatically.
- **SSH** to VMs goes through `hangar ssh` and `hangar ssh-proxy` over a
  WebSocket tunnel: Access, then the apiserver, then the network namespace. No
  new IPv4 ports are opened.
- **Abuse protection**: port 25 is blocked by default, egress is rate-limited,
  and NAT is logged, because abuse from the single IPv4 address could get the
  host shut down.

## Consequences

Routed IPv6 with NDP proxy and the OVH upstream behaviour need proving on the
real host; see `testing-strategy.md` for what can be emulated before then.
