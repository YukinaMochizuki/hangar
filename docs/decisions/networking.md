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
- **Addresses are the apiserver's.** A `Network` carries its IPv4 range; the
  container controller hands each machine interface the lowest free address
  after the gateway (the range's first usable address), and the network
  controller gives each network a /31 transit link from 100.64.0.0/10 to the
  host. Both are decided under one lock and derived from what is stored, so
  an address is free again once the machine holding it is gone; a machine
  being deleted keeps its address until its node releases it. Every project
  gets a `default` network (10.0.0.0/24), recreated if deleted. A user cannot
  delete a network any machine is on, as clouds refuse to delete a subnet
  still in use: a network being deleted could take no new machines and could
  not be brought back. A project being deleted takes its networks with it,
  each kept until its last machine is gone.
- **Inside a node**, each network's namespace reaches the host through a veth
  pair on its transit link. IPv4 egress is two NATs: the namespace maps its
  range to its transit address, and the host maps 100.64.0.0/10 to the public
  address. Overlapping tenant ranges never meet on the host.
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
