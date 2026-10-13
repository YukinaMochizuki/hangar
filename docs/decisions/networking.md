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
  (`<name>.<project>.hangar.internal`), and a metadata service at
  169.254.169.254.
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
- **The network driver (`internal/node/driver/hostnet`) builds a node's
  side** with netlink and nftables, never by running `ip` or `nft`, and keeps
  no state: its namespaces and links are named with the prefix `hn` and its
  nft tables `hangar`, it reads its objects back by name and link alias, and
  it changes nothing else. Of the links with its prefixes, only veths and
  taps are taken for its own.
  - Each network's namespace is `hn-<network uid>` in `/run/netns`, where
    `ip netns` sees it. Inside, everything is the driver's, so names are
    fixed: the veth end `hn-up`, the bridge `hn-br` holding the gateway.
    The bridge's MAC is derived from the network uid rather than taken from
    its ports, so guests' ARP entries for the gateway survive the bridge or
    the namespace being made again. So are both ends' of the veth, which
    keeps the namespace's ARP entry for the host valid too: a veth made
    without a MAC gets a random one, which udev's `MACAddressPolicy=persistent`
    replaces a moment later, and a namespace that has already resolved the
    old one then sends nothing out for about 30 s.
  - A veth's root end is `hnv` and a tap `hnt`, then 12 hex digits of a
    hash of the network uid, or of the machine uid and interface name, which
    fits Linux's 15 characters. A tap lives in its network's namespace, but
    its name is unique on the host. A name another link holds is skipped for
    the hash of the key and a counter. The link alias holds the uid, and for
    a tap the interface as ensured; listing reads it back from there.
  - **Default deny around the networks** (decided by the project owner): a
    network's traffic leaves only as an address of its range, NATed, and only
    to the internet; nothing reaches a network or the host from one but
    replies. Table `ip hangar` in the root namespace has five rules however
    many networks there are:
    - prerouting, before conntrack: from `hnv*`, drop a source not routed
      back out of the link it came in on (strict reverse path, `fib saddr .
      iif oif missing`), so a network sends only as its own transit address;
    - postrouting: 100.64.0.0/10 leaving the uplink is SNATed to the
      configured public address (masqueraded if none is set);
    - forward: into `hnv*`, drop all but replies, which isolates networks
      from each other; from `hnv*`, drop what leaves by any link but the
      uplink, so docker bridges, VPNs and the like, and the host's DNAT into
      them, stay out of reach. With the rule before, no connection into a
      network exists whose replies could leave by another link;
    - input: from `hnv*`, drop all but replies, so a network cannot reach
      the host.
  - Table `ip hangar` in each namespace has two: in prerouting, before
    conntrack, drop what comes in from `hn-br` with a source outside the
    network's range, so a guest cannot forge one, nor make the gateway answer
    one; and SNAT the range leaving `hn-up` to the namespace's transit
    address. A guest taking an address of the namespace's own, its transit
    address or the gateway, is dropped by the kernel as a martian. Each
    layer holds without the other.
  - Each rule's comment says what the rule does and ends with a hash of its
    expressions. A table whose comments differ from the wanted rules',
    including one written for other rules under the same words, is replaced
    in one transaction; a table as wanted is not written at all.
  - Taps are persistent, single-queue, with virtio-net headers, so a VM
    opens its tap again each time it starts. The hypervisor driver opens it
    with `hostnet.OpenTap`, in its namespace. A tap has carrier while open,
    and the driver refuses to delete a tap a VM holds open: when the
    interface is deleted, ensured on another network, or has a duplicate.
  - The driver watches links, IPv4 addresses and routes, and its nft tables,
    in each network's namespace and, for its own links and the uplink, in the
    root namespace, and signals `Changes`. A link going down takes its
    routes with it, and the driver's own changes are signalled too; the agent
    ensures again, which changes nothing the second time. A link's carrier
    and IPv6 are not watched: they change on their own, and an ensure of
    what is whole then changes nothing and signals nothing. A subscription
    that fails is signalled, and its namespace is watched again at the next
    ensure; one that cannot start fails the ensure. A namespace unmounted
    from under the driver (`ip netns del`) is not signalled; it is made
    again at the next ensure.
  - What the driver needs of the host is deploy/host's: IPv4 forwarding on
    (it refuses to make a network otherwise), the uplink's name and,
    optionally, the public address as `hangar-node` flags (`--uplink`,
    `--public-ipv4`), nftables in the kernel, `/run/netns` shared with the
    agent's mount namespace, no networkd config matching `hn*` links, and no
    host firewall dropping forwarded traffic to or from `hnv*`.
  - **The node serves each network from inside its namespace**
    (`internal/node/netsvc`): the driver opens a service's socket on a
    thread in the namespace, bound to `hn-br` before its address, so of
    what arrives from outside the namespace only the network's machines
    reach it, never what comes in by `hn-up`, routed by the host or not. A
    socket keeps the namespace it was opened in, so the driver names each
    instance, its namespace's nsfs inode and its bridge's index (a bridge
    made again is another link), and the agent opens a network's sockets
    again once the instance changes, and closes them before the sweep
    removes the network. A process inside the namespace reaches the
    sockets too, as the kernel delivers local traffic to them; only root
    on the host runs one there. The sockets live in the agent's memory
    only; a restarted agent opens them again from the API.
- **Each network has its own resolver, on its gateway, port 53, UDP and
  TCP** (`internal/node/resolver`), embedded in the node agent rather than
  a dnsmasq or CoreDNS to supervise. It must live in the namespace anyway:
  the root table drops every new connection from `hnv*` to the host.
  - It answers `<name>.<project>.hangar.internal` for the machines with an
    interface on the network, whichever node runs them, by the name of the
    object a machine was made from (its Container) and with a TTL of 5
    seconds. A machine of another network or project is NXDOMAIN; only A
    records exist for now. `hangar.internal` is never forwarded.
  - The agent opens a network's sockets before it reads the network's
    machines from the API, and until it has, internal names fail with
    SERVFAIL, not NXDOMAIN, so a client retries rather than caching a
    wrong negative answer. Before the sockets are open (the agent starting)
    nothing answers, which a client takes as no answer too.
  - Every other name is forwarded to the node's upstream resolvers
    (`hangar-node --dns-upstreams`), from the host, two tries of 2 seconds,
    then SERVFAIL; answers are cached for their TTLs (negative ones for
    their SOA's minimum, at most an hour) and a question asked many times
    at once goes upstream once. UDP answers are cut at 1232 bytes, with TC
    set, as DNS Flag Day 2020 settled. AAAA questions are forwarded too,
    though guests have no IPv6 yet (decided by the project owner): as on
    any IPv4-only network, clients fall back to IPv4.
  - No network takes what the others need: one network's questions in
    flight are bounded (128), past which they fail with SERVFAIL at once,
    as are its TCP connections (32), past which they wait to be accepted;
    the forwards of every network together are bounded too (1024).
    Stopping a network's resolver closes its connections, so no client
    holds the agent up.
  - A guest's resolv.conf lists the gateway of its first interface's
    network, and searches `<project>.hangar.internal`, through the boot
    disk; the guest needs nothing of its own for it. A resolver knows only
    its own network and a guest does not ask the next resolver after a
    NXDOMAIN, so a machine on several networks resolves the names of its
    first.
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
