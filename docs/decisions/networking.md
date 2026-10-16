# Networking

Status: accepted, 2026-10.

## Context

One OVH host with one IPv4 address and an IPv6 /64. OVH's IPv6 is on-link with
MAC filtering at the switch. Ordinary HTTPS goes through the host's front
door, hangar-edge (`front-door.md`); anything else needs a reachable
address.

## Decision

- **A simple VPC first**: `Network` (each project gets a `default`), each
  one user-chosen IPv4 range that may overlap other networks', multiple
  NICs per machine, security groups, NAT egress, internal DNS
  (`<name>.<project>.hangar.internal`), and a metadata service at
  169.254.169.254. There is no `Subnet` (decided by the project owner):
  on one host without zones a subnet buys nothing another network does
  not, as DigitalOcean's VPCs have none; one can come later as a network's
  default subnet. Peering, route tables and shared VPCs come later.
- **A Container or VirtualMachine names its networks** in
  `spec.interfaces` (`NetworkInterface`), at most 8, each network once;
  none is one interface on `default`. They are eth0, eth1… in order, each
  with an address of its network's range and no address of the user's
  choosing; the first holds the default route, and so the machine's
  internet and its resolver. Changing them restarts the machine.
- **Each `Network` is its own network namespace** (that is what allows
  overlapping CIDRs) with a bridge and nftables. The node creates tap devices
  and hands their file descriptors to Cloud Hypervisor. The apiserver does IPAM
  and assigns static addresses; DHCP only delivers them, and has no pool
  of its own. All of it sits behind a network driver interface so VXLAN or
  OVN can replace it.
- **Addresses are the apiserver's.** A `Network` carries its IPv4 range; the
  container controller hands each machine interface the lowest free address
  after the gateway (the range's first usable address), and the network
  controller gives each network a /31 transit link from 100.64.0.0/10 to the
  host. Both are decided under one lock and derived from what is stored, so
  an address is free again once the machine holding it is gone; a machine
  being deleted keeps its address until its node releases it. A machine
  interface on a node whose host has an IPv6 prefix also gets a public
  /128 of it (`ipv6`), when the machine is placed, kept as long as the
  prefix holds it: its interface ID is random, so neighbours cannot be
  enumerated and a freed address is not handed straight to the next
  machine, and never below 2^32, which leaves `::` and the low addresses,
  where deploy/host puts the host's own (`HANGAR_IPV6_ADDR`), to the host.
  A node whose prefix changes moves its machines to the new one; one that
  loses it takes their IPv6 away. A new address restarts no machine: the
  interface is changed in place on the host, at once, and is no part of
  what a VM runs (its spec's hash). A VM learns it by DHCPv6, at its next
  renewal, within minutes (below). A container's guest is told it by the
  guest agent (`SetIPv6`), which hangar-init applies by netlink, as the
  container shares hangar-init's network namespace. The boot disk carries
  the address too, so a container that boots later starts right; but a
  guest that boots again by its restart policy reuses the disk written when
  its VM started. So the agent tells a guest its address the first time
  it sees each boot, and again whenever the address changes. A reboot of
  the guest and a restart of the agent both count as a new sighting, and
  telling is the only way the agent knows what the guest holds.
  A container whose hangar-init predates `SetIPv6`, started before an
  upgrade, is left alone if its boot disk already carries the address it
  should hold. Otherwise it is started again, once, with the address on its
  boot disk, and the agent logs it. Upgrading the node restarts no
  machine: a VM an older node started keeps the hash that node gave, which
  counted the IPv6 addresses, and the agent accepts it
  (`VMSpec.LegacyHash`). Every project
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
    replies. The one exception is public IPv6 (decided by the project
    owner): a new connection to a live /128 of one network from the
    internet or from another network on the host passes the host, and the
    receiving interface's security groups decide, as they would for the
    same packet from anywhere; a public address is no more trusted from a
    neighbouring network than from anywhere else, and the host is not the
    one place it cannot be reached from. Private IPv4 between networks
    stays blocked, the host stays unreachable but for the front door's
    ports at its public addresses, which anyone reaches (decided by the
    project owner), and a network sends only from its own /128s. Table
    `ip hangar` in the root namespace has five rules, seven with a public
    address set, however many networks there are:
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
    - input: from `hnv*`, accept TCP to 80 and to 443 at the configured
      public address, the front door's ports, which anyone reaches there
      (two rules, none with no address set), then drop all but replies, so
      a network reaches nothing else of the host. A machine calling the
      platform by its public names, `api.` or an endpoint, so reaches the
      front door without leaving the host.
  - With an IPv6 prefix, table `ip6 hangar` in the root namespace has
    seven, nine with the host's own address set (`--public-ipv6`), however
    many networks there are:
    - prerouting, before conntrack: from `hnv*`, drop a source not routed
      back out of the link it came in on, so a network sends only from its
      own /128s (each is routed to its network's veth) or link-local;
    - forward: from `hnv*`, drop TCP to port 25 (the abuse protection
      below); from `hnv*`, drop what leaves by a link that is neither the
      uplink nor another network's veth; into `hnv*`, drop what is not for
      the prefix; into `hnv*` from anything but the uplink or another
      network, drop all but replies. A destination reaches a network's
      veth only by its /128's route, so only live addresses are let in;
    - input: from `hnv*`, take neighbour solicitations and advertisements,
      for the transit link's link-local addresses, accept TCP to 80 and to
      443 at the host's own address, the front door's, as for IPv4, and
      drop all but replies.
  - Table `ip hangar` in each namespace has four: in prerouting, before
    conntrack, accept DHCP from `hn-br` sent from no address to the
    broadcast (UDP 68 to 67), which a guest without an address yet sends
    and which is never routed on; drop what else comes in from `hn-br` with
    a source outside the network's range, so a guest cannot forge one, nor
    make the gateway answer one; drop anything coming in by a tap's own
    stack, where the bridge passes frames for the 802.1D group addresses,
    past the bridge's hooks; and SNAT the range leaving `hn-up` to the
    namespace's transit address. The namespace also sets its own strict
    reverse-path filter rather than inherit the host's default, and turns
    IPv6 off on its links. With an IPv6 prefix, IPv6 is turned on for the
    bridge and the veth's end alone, each with the one link-local address
    the driver gives it and none of the kernel's (`fe80::1` on `hn-br`,
    the gateway, and `fe80::2` facing the host's `fe80::1`), taking no
    router advertisement; taps keep IPv6 off, as bridge ports need none.
    Table `ip6 hangar` then drops, before conntrack, a source from `hn-br`
    outside the prefix and link-local, anything in by a tap's own stack,
    and what would go back out of `hn-up`. Without a prefix nothing of IPv6
    exists in a namespace. A guest taking an address of the
    namespace's own, its transit address or the gateway, is dropped by the
    kernel as a martian.
  - Table `bridge hangar` in each namespace binds each interface, in its
    prerouting, before the bridge forwards a frame or delivers it to the
    namespace: a tap sends only from its interface's MAC; IPv4 only from
    its assigned address, but for DHCP from no address to the broadcast;
    ARP only for that MAC and address; and nothing but IPv4 and ARP, and
    IPv6 if its interface has a /128. Then it sends IPv6 only from that
    address or a link-local one, guests choosing their own, or from none
    only as duplicate address detection and listener reports do; it
    advertises only those addresses as a neighbour, never the gateway's,
    and a solicitation from an address, or an advertisement, carries at
    most one option, its link-layer address, holding only its MAC (the
    kernel takes the first link-layer option wherever it is, past others,
    so more than one is refused);
    and it sends no router advertisement, redirect or renumbering, and
    nothing a DHCPv6 server or relay sends (from port 547, or to port
    546). A guest can claim another's link-local address on its network;
    the worst it does is keep that guest's DHCPv6 replies from it, never
    its /128. The kernel takes a VLAN tag
    off before the bridge's hooks, so a tagged frame is held to the same
    rules on what it carries. A frame too short for a rule's field passes
    that rule, and the receiving stack drops it. A tap without
    rules of its own sends nothing. Its rules are written from the taps'
    aliases, before a tap joins the bridge, and go with it. So a guest
    cannot pass as another of its network, which the metadata service
    relies on to know who asks.
  - Each of the three layers holds without the others: the binding, the
    namespace's filter and the host's. A frame to a group address skips
    the binding, and meets the namespace's filter and its reverse-path
    filter.
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
    root namespace, and signals `Changes`; with an IPv6 prefix, the IPv6
    addresses and routes of its own links too. Each /128 is routed from
    the root namespace to its network's veth and in the namespace to its
    bridge, and has a proxy NDP entry on the uplink, which OVH's on-link,
    MAC-filtered /64 needs: the upstream asks for each address by neighbour
    solicitation and delivers only to the MAC that answers, the host's.
    In the namespace each /128 also has a permanent neighbour entry on the
    bridge at its tap's MAC, and the driver removes any other entry of the
    prefix there. When an Address moves to another interface of the same
    network, the namespace then sends to the new holder at once. Without
    these entries, the namespace would keep sending to the old holder's MAC
    until its cache entry went stale, and the old holder answers for the
    address until its lease is renewed.
    All of it is derived from the taps' aliases; every proxy entry of the
    prefix on the uplink and every /128 route by an `hnv*` link is the
    driver's. The kernel signals no change to proxy entries, so the driver
    compares them with the taps' addresses every 5 seconds. A link going down takes its
    routes with it, and the driver's own changes are signalled too; the agent
    ensures again, which changes nothing the second time. A link's carrier,
    and without a prefix its IPv6, are not watched: they change on their own, and an ensure of
    what is whole then changes nothing and signals nothing. A subscription
    that fails is signalled, and its namespace is watched again at the next
    ensure; one that cannot start fails the ensure. A namespace unmounted
    from under the driver (`ip netns del`) is not signalled; it is made
    again at the next ensure.
  - What the driver needs of the host is deploy/host's: IPv4 forwarding on
    (it refuses to make a network otherwise), the uplink's name and,
    optionally, the public address as `hangar-node` flags (`--uplink`,
    `--public-ipv4`), and for IPv6 the prefix (`--public-ipv6-prefix`, the
    /64 of `HANGAR_IPV6_ADDR`), IPv6 forwarding and proxy NDP on the uplink
    (it refuses to make a network with a prefix otherwise), nftables in the kernel, `/run/netns` shared with the
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
    seconds: A for its IPv4 addresses on the network and AAAA for its
    public IPv6 ones. A machine of another network or project is
    NXDOMAIN. `hangar.internal` is never forwarded.
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
    set, as DNS Flag Day 2020 settled. AAAA questions are forwarded too
    (decided by the project owner), which a guest without IPv6 falls back
    from as on any IPv4-only network.
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
- **Each network has a DHCP server** (`internal/node/dhcp`), beside its
  resolver, on UDP 67 of any address of `hn-br`, for images that configure
  themselves by DHCP, as cloud images do; container guests never ask, as
  their boot disk carries their network. It offers each machine interface,
  known by its MAC, the address the apiserver assigned it, with the
  gateway as router and resolver, the project's search domain, the
  machine's name as hostname and a lease of a day; a MAC it does not know
  is not answered, and a request for another address is refused. A client
  without an address is answered by broadcast, one renewing at its
  address.
- **Each network with IPv6 advertises its gateway and leases each machine
  its /128 by DHCPv6**, as AWS, GCP and Azure do (decided by the project
  owner), for images that configure themselves by router advertisement and
  DHCPv6; container guests take theirs, and the default route through
  `fe80::1`, from the boot disk, and hangar-init sets them. Both are the
  agent's (`internal/node/ra`, `internal/node/dhcp6`), on packet sockets of
  the bridge (`ListenPackets`) that see which MAC a packet came from, which
  the binding makes the interface's own: DHCPv6 knows a client by it, not
  by its DUID, as clients choose those freely. A virtio guest leaves its
  UDP checksum to its NIC, so a packet the socket marks as such has its
  checksum completed before it is read.
  - Router advertisements come from `fe80::1` with M set (addresses by
    DHCPv6), no prefix to configure an address from, and a router lifetime
    of 30 minutes, every 200 seconds or so and when a machine solicits one,
    never more than once in 3 seconds however many solicit (RFC 4861).
  - DHCPv6 leases each interface's MAC its /128 (IA_NA), valid and
    preferred for a day, so a guest keeps it through a long outage of the
    agent, but with T1 2 minutes and T2 3 minutes, so a guest renews often
    enough that an address that moved reaches it within minutes: a reply
    gives it its address now and, with lifetimes of 0, any address it
    held that is no longer its own, which it drops. RFC 8415 asks only
    that T1 not exceed T2, nor T2 the preferred lifetime; its 0.5 and 0.8
    of the preferred lifetime are a recommendation. No Reconfigure is sent:
    systemd-networkd's DHCPv6 client (systemd 255, Ubuntu 24.04) takes none,
    as it drops any message not of its own transaction, and RFC 8415 asks a
    reconfigure key besides. A MAC it does not know is not answered; it answers at most 10 requests a
    second from one MAC and 100 from one network. DHCPv6 hands out the
    address only; DNS stays the network's IPv4 resolver (decided by the
    project owner): networks are always dual-stack, the resolver answers
    AAAA too, and DHCPv6 cannot carry a link-local resolver's scope.
- **Each network has a metadata service** (`internal/node/metadata`) on
  169.254.169.254, port 80, an address its bridge holds beside the
  gateway, in the form of cloud-init's NoCloud datasource, which a VM is
  pointed at by its SMBIOS serial number
  (`ds=nocloud;s=http://169.254.169.254/nocloud/`). It serves
  `/nocloud/meta-data` (the instance id, its name as hostname),
  `user-data` and `vendor-data`, each for the machine whose address the
  request comes from, which the binding above makes sound; any other
  address or path is 404. NoCloud sends no token, so nothing but the
  binding stands between a guest and another's user-data. A container's
  instance id is its machine's uid, and it has no user-data or
  vendor-data; a VM's are its own (`vm-boot.md`), carried on its
  machine's `firmware.cloud_init`, which the apiserver shows a node agent
  only for the machines on that node (and a VM's `userData` and keys to
  none), so the node serving a guest is the only one that reads its
  data. It takes GET only, with bounded headers and timeouts, at most 16
  connections a network and 4 a machine, so no machine keeps the others'
  cloud-init waiting, and stops with a deadline, as the resolver does.
- **`SecurityGroup`** works as in AWS (`internal/secgroup`): a group belongs
  to one network; an interface names up to 5 of its network's groups in
  `spec.interfaces[].securityGroups`; rules are allow-only, stateful, by
  protocol, ports and a peer that is a CIDR or a group of the network.
  Ingress is denied unless a rule of one of the interface's groups lets it
  in; egress is open unless one of its groups has egress rules, and then
  only what some rule of its groups lets out leaves (decided by the project
  owner, for agents' sandboxes: one restricting group locks an interface
  even beside `default`). Here groups differ from AWS's, where egress is
  the union of the groups' rules and every group allows all out until
  edited; restricting is closer to Hetzner, whose firewalls without
  outbound rules allow all out, and to a GCP deny rule by tag.
  - Every network has an implicit group `default`, which is no object and
    cannot be changed (decided by the project owner): it lets in its own
    members, and lets everything out. An interface naming no group is in
    it alone; naming groups without `default` takes it out; `[default,
    web]` keeps it in both. So machines naming nothing behave as before
    groups existed. Other groups' rules may name `default` as a peer.
  - A group an interface names that does not exist lets nothing in and,
    so that a missing group never opens anything, nothing out. A rule
    naming a group that does not exist, or has no members, allows nothing,
    even to interfaces naming that group. Admission lets such names
    through, so objects can be applied in any order, but refuses one naming
    a group of another network. A group an interface is in cannot be
    deleted, as in AWS.
  - A CIDR peer is IPv4 or IPv6 and matches only its own family, so
    `::/0` is not `0.0.0.0/0`, as in AWS; a group peer stands for its
    members' IPv4 and IPv6 addresses, `default` included; `icmp` is ICMPv6
    for an IPv6 peer. Inbound IPv6 from the internet is thus denied unless
    a rule allows it.
  - The node resolves each interface's policy from the network's groups and
    every machine on it, whichever node runs them, in the same reconcile
    as the network's resolver, and enforces it in table `bridge hangar_sg`
    in the network's namespace: what a tap sends in prerouting, what is
    sent to it in forward (from another tap) and output (from the namespace,
    or routed there), each through chains of its own. A connection's
    replies always pass, as do ARP, DHCP (to the broadcast address or the
    gateway, and back from the gateway), neighbour discovery and listener
    reports on the link, DHCPv6 (to the servers' group, and back from
    `fe80::1`), and the network's own resolver and metadata service, which
    a sandbox still needs; ICMPv6 errors of an admitted flow pass as its
    replies. On the link means as no router forwards it: neighbour
    discovery and router advertisements with hop limit 255 (RFC 4861),
    listener messages with hop limit 1, to a tap only from a link-local
    address or, for duplicate address detection, none. So the internet's
    ICMPv6, of any type, is the groups' to let in, as its echo is. So does
    the platform's own SSH path from the gateway, TCP from its address to a
    tap's port 22, gated by RBAC and minutes-long certificates instead, as
    AWS SSM is (`vm-ssh.md`): only the namespace's own sockets send from the
    gateway's address. So does the edge's path to a public endpoint (decided
    by the project owner): while an endpoint exists, TCP from the gateway's
    address to its port passes on the first interface of the machine it
    reaches, that tap alone, gated by Access and the endpoint's permission
    in the edge (`public-endpoints.md`); the rule goes with the endpoint. The bridge and the IP
    stack share one conntrack entry per flow. A tap with no policy yet,
    such as one just made, sends and receives nothing else, and the table
    is made, closed, with the network, before any tap can join it. The
    whole table is replaced in one transaction when a policy changes,
    which keeps the connections already admitted. A network's firewall is
    bounded to 20,000 rules (each rule is written once per peer and port
    range); policies past it, or a firewall that cannot be written, close
    the network's interfaces on that node to all but its services rather
    than leave older rules in force, and the resolver, DHCP and metadata
    service keep following the network's machines.
- **Public IPv6**: every machine interface has a public /128 of its host's
  /64 (above), its identity both ways, with no NAT, so one machine's abuse
  does not taint the shared IPv4. It is routed to the machine with proxy
  NDP on the host (no bridging, because of the MAC filtering).
- **An `Address` reserves a public /128** and binds it to one interface of
  a VirtualMachine or Container of its project, as an AWS Elastic IP is
  associated: it names its target (`spec.target`, decided by the project
  owner), and moving it changes only the Address. Its address is drawn
  when it is made, from a node's prefix, at random as an automatic one,
  and kept until it is deleted; no interface draws a reserved address as
  its own. Bound, it replaces the interface's automatic address, which is
  given up; the interface it leaves, unbound, retargeted or deleted, draws
  a new automatic one. The hand-off is enforced twice, so no two
  interfaces hold the address, in the store or on the host:
  - In the API, the machine it moves to is given it only once the address
    is gone from the spec of the machine it leaves. The machine it moves to
    waits for that, reported as waiting, not failing, and is reconciled
    again when the other machine changes.
  - On the host, the interface it moves to takes it only once the one it
    leaves has let go. The agent lets go of an address before anything a
    machine's reconcile may wait on, such as its image, and the network
    driver refuses an address another interface holds (`ErrIPv6InUse`).
    Meanwhile the interface it moves to has none, and its machine runs on.
    That gap lasts as long as the machine it leaves takes to let go. That
    is usually one reconcile. When the machine it leaves is being stopped,
    the gap lasts as long as the stop: 12 s in the hermetic tests, and up to
    15 s for a container or 65 s for a VM on a real host. It is the price
    of never holding one address twice.

  The route, proxy NDP entry and the network's neighbour entry follow the
  interface at once, so the machine it leaves stops receiving on the
  address then, whatever its lease says, and neither machine starts again
  (above). A target that does not exist yet is waited for; a
  target deleted leaves the Address reserved and unbound. A deleted Address
  stays, reserving its address, until its interface has let go of it. No
  two Addresses name one interface, which admission checks under a lock.
  Its status says where it is bound (`Bound`, and the machine).
- **HTTPS exposure** is a `PublicEndpoint` (`public-endpoints.md`): a
  wildcard DNS record leads to the host's front door, Hangar's own edge
  proxy, `hangar-edge`, which routes by `Host` and reaches the machine
  through the node agent, which dials it from the network's gateway inside
  its namespace. Endpoints are `private` by default (the edge signs their
  callers in and checks they may use the project's endpoints) and become
  `public` only when set explicitly. User content uses a different domain
  from the console, `<endpoint>-<project>.hangarlab.dev`, only first-level
  wildcard hostnames (the wildcard certificate does not cover deeper
  levels). A container port with `public: true` creates an
  endpoint automatically. Security groups do not apply to the edge's path:
  the endpoint is the explicit opening (above).
- **SSH** to VMs goes through `hangar ssh` and `hangar ssh-proxy` over a
  WebSocket tunnel: the front door, then the apiserver, then the network
  namespace. No new IPv4 ports are opened.
- **Abuse protection**: port 25 is blocked by default, egress is rate-limited,
  and NAT is logged, because abuse from the single IPv4 address could get the
  host shut down.

## Consequences

Routed IPv6 with proxy NDP is proven on the testbed against an emulated OVH
upstream (`test/testbed/network`, `test/testbed/flow`): a router whose route
to the host's /64 is on-link, which asks for each address by neighbour
solicitation and delivers only to the MAC that answers. Guests get their
/128 by router advertisement and DHCPv6 (`dhcpcd`) or the boot disk, reach
the upstream as it, are reached only once their groups allow and only
through the host's proxy answer, and routes and proxy entries removed by
hand come back. Whether OVH's switch behaves as the emulation does is still
the real host's to show; see `testing-strategy.md`.
