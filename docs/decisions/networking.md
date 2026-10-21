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
  169.254.169.254. There is no `Subnet`:
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
    fits Linux's 15 characters; a file share's server's tap is `hnts` and
    11, `s` being no hex digit. A tap lives in its network's namespace, but
    its name is unique on the host. A name another link holds is skipped for
    the hash of the key and a counter. The link alias holds the uid, and for
    a tap the interface as ensured; listing reads it back from there.
  - **Default deny around the networks**: a
    network's traffic leaves only as an address of its range, NATed, and only
    to the internet; nothing reaches a network or the host from one but
    replies. The one exception is public IPv6: a new connection to a live /128 of one network from the
    internet or from another network on the host passes the host, and the
    receiving interface's security groups decide, as they would for the
    same packet from anywhere; a public address is no more trusted from a
    neighbouring network than from anywhere else, and the host is not the
    one place it cannot be reached from. Private IPv4 between networks
    stays blocked, the host stays unreachable but for the front door's
    ports at its public addresses, which anyone reaches, and a network sends only from its own /128s. Table
    `ip hangar` in the root namespace has nine rules, eleven with a public
    address set, however many networks there are:
    - prerouting, before conntrack: from `hnv*`, drop a source not routed
      back out of the link it came in on (strict reverse path, `fib saddr .
      iif oif missing`), so a network sends only as its own transit address;
    - postrouting: 100.64.0.0/10 leaving the uplink is SNATed to the
      configured public address (masqueraded if none is set);
    - forward, six: from `hnv*`, drop TCP to ports 25 and 465, and what
      conntrack finds invalid (the abuse protection below); into `hnv*`,
      drop all but replies, which isolates networks from each other; from
      `hnv*`, drop what leaves by any link but the uplink, so docker
      bridges, VPNs and the like, and the host's DNAT into them, stay out
      of reach. With the rule before, no connection into a network exists
      whose replies could leave by another link. Last, mark a new
      connection from `hnv*` with its veth (the NAT log, below);
    - input: from `hnv*`, accept TCP to 80 and to 443 at the configured
      public address, the front door's ports, which anyone reaches there
      (two rules, none with no address set), then drop all but replies, so
      a network reaches nothing else of the host. A machine calling the
      platform by its public names, `api.` or an endpoint, so reaches the
      front door without leaving the host.
  - With an IPv6 prefix, table `ip6 hangar` in the root namespace has
    ten, twelve with the host's own address set (`--public-ipv6`),
    however many networks there are:
    - prerouting, before conntrack: from `hnv*`, drop a source not routed
      back out of the link it came in on, so a network sends only from its
      own /128s (each is routed to its network's veth) or link-local;
    - forward, six: from `hnv*`, drop TCP to ports 25 and 465, and what
      conntrack finds invalid (the abuse protection below); from `hnv*`,
      drop what leaves by a link that is neither the uplink nor another
      network's veth; into `hnv*`, drop what is not for the prefix; into
      `hnv*` from anything but the uplink or another network, drop all but
      replies. A destination reaches a network's veth only by its /128's
      route, so only live addresses are let in;
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
  - Table `inet hangar_limit` in each namespace has one rule, in forward:
    a new connection from `hn-br` out of `hn-up` past the network's rate is
    dropped (the abuse protection below). It is written, read back and
    replaced as the driver's other tables are.
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
    and file shares' exports (`file-shares.md`) rely on to know who asks.
  - **A tap receives only its own unicast.** Each tap is a bridge port
    that neither learns nor takes flooded unknown unicast, and its MAC,
    from its alias, is a static entry of the bridge's forwarding database
    on that tap, gone with the tap; broadcast and multicast still flood to
    it, for ARP, DHCP and neighbour discovery. A bridge that has not
    learnt, or has forgotten, where a MAC is otherwise floods a frame for
    it to every tap, so a machine could read its neighbours' traffic; here
    such a frame is dropped. With it, nothing of one machine's unicast
    reaches another, which is what lets a file share's traffic go
    unencrypted on one node. A tap joins the bridge down, and is up only
    once its port is set, so it is never flooded. No two interfaces of a
    network, nor an interface and the gateway, have one MAC: the driver
    refuses the second (`ErrMACInUse`). The driver reads the port's flags
    and the tap's entries back, removes every other entry on a tap but the
    kernel's own, and puts back the bridge's own entry for the gateway,
    which a flush takes too. It watches all of these, and an entry on the
    bridge itself for a tap's MAC, which would take that tap's frames.
  - **A file share's server sends only replies** (`file-shares.md`): in
    `bridge hangar_sg`, its taps take only new TCP connections to 2049
    from the machines declaring the share, whatever their security
    groups say, and send only replies on connections made to it. Each
    base chain sends what is from or for an `hnts` tap to a chain of
    servers' own before any other rule, so nothing every tap has (its
    network's DHCP, DNS and metadata, the gateway's SSH, an edge port, a
    group) reaches a server or lets it out: into one, ARP and TCP to 2049
    from each client by its address, new or established, the way the
    connection was made; from one, ARP and TCP from 2049 on an
    established connection, the way of its replies. A server's tap no
    rule names, as one made since, takes nothing. A server's tap is
    refused on a network whose firewall has no servers' chains yet, as one
    an older agent wrote, until the agent writes it again. A client's own
    chain lets its TCP to its shares' servers' 2049 out before its groups'
    rules. The driver reads both kinds of rule back from the table as they
    stand (`ShareRules`), which the agent waits for before booting a
    container that mounts a share (`file-shares.md`).
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
    set, as DNS Flag Day 2020 settled. AAAA questions are forwarded too, which a guest without IPv6 falls back
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
  its /128 by DHCPv6**, as AWS, GCP and Azure do, for images that configure themselves by router advertisement and
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
    address only; DNS stays the network's IPv4 resolver: networks are always dual-stack, the resolver answers
    AAAA too, and DHCPv6 cannot carry a link-local resolver's scope.
- **Each network has a metadata service** (`internal/node/metadata`) on
  169.254.169.254, port 80, an address its bridge holds beside the
  gateway, in the form of cloud-init's NoCloud datasource, which a VM is
  pointed at by its SMBIOS serial number
  (`ds=nocloud;s=http://169.254.169.254/nocloud/;dsmode=net`, `vm-boot.md`). It serves
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
  only what some rule of its groups lets out leaves (for agents'
  sandboxes: one restricting group locks an interface even beside
  `default`). Here groups differ from AWS's, where egress is
  the union of the groups' rules and every group allows all out until
  edited; restricting is closer to Hetzner, whose firewalls without
  outbound rules allow all out, and to a GCP deny rule by tag.
  - Every network has an implicit group `default`, which is no object and
    cannot be changed: it lets in its own
    members, and lets everything out. An interface naming no group is in
    it alone; naming groups without `default` takes it out; `[default,
    web]` keeps it in both. So machines naming nothing behave as before
    groups existed. Other groups' rules may name `default` as a peer. A
    file share's server is the exception: it names no group and is in
    none, `default` included, so no group's rule lets it in, and its own
    policy allows nothing; nor does what the host lets every tap reach
    apply to it (above).
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
    gateway's address. So does the edge's path to a public endpoint: while an endpoint exists, TCP from the gateway's
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
  associated: it names its target (`spec.target`), and moving it changes
  only the Address. Its address is drawn
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
- **Abuse protection**: abuse from the single IPv4 address could get the
  host shut down, so mail ports are closed, new connections are
  rate-limited, and IPv4 NAT is logged, for every network alike, with no
  exception by plan for now.
  - TCP from a network to ports 25 (SMTP) and 465 (SMTP over TLS) is
    dropped on the host, in the root tables' forward chains, over IPv4 and
    IPv6. 587 (submission) stays open: it needs an account at the server,
    so it sends no more spam than the account's owner lets it.
  - A network opens at most 100 new connections a second, after a burst of
    300, IPv4 and IPv6 together from one bucket: `inet hangar_limit` in its namespace drops a packet starting a
    connection out of `hn-up` past it. Only new connections count, so flows
    already admitted carry on, and so does what stays in the network: its
    own services and its machines talking to each other. A dropped
    connection is retried by its client, as any lost SYN is. Each network
    has a bucket of its own, so one network's flood holds back no other;
    the host as a whole is bounded by how many networks it has.
  - What conntrack finds invalid from a network is dropped on the host,
    over both families: no connection, rate limit or log accounts for it.
    Over IPv4 it held already, as the rule now says outright: an invalid
    packet is not NATed in the namespace, so it reaches the host from a
    machine's address, which the reverse-path filter drops.
  - Every new IPv4 connection out of a network is logged, never with a
    payload, and nothing of IPv6, where each machine is its own address
    ("Public IPv6", above). A line holds `time`, `event` (`new`),
    `proto`, `public_ip` and `public_port` (the source the internet sees),
    `dst_ip` and `dst_port`, `project`, `network` and `network_uid`,
    `machine` (its name in the project) and `machine_uid`, `inner_ip` and
    `inner_port` (the machine's own source), and `transit_ip` and
    `transit_port` (the source between the two NATs). A protocol without
    ports, as ICMP, has none. What cannot be told, as the machine of a
    connection its network no longer knows, is `unknown`, never left out.
  - The public port exists only after the host's SNAT, and the machine
    only before the namespace's, so the log joins the two layers
    (`hostnet` `FollowNAT`). The root table marks each new IPv4
    connection from a network with the index of its veth and a tag bit
    (`ct mark`). The agent listens to the host's conntrack events for new
    connections through a socket filter that passes only tagged ones, so
    the host's own traffic never reaches it: ti-mo/conntrack has no filter
    for events, and the kernel none but a socket's (classic BPF, as
    conntrack-tools uses). Each event carries both tuples, the transit
    source and the public source; the veth names the network, and the
    network's own conntrack, asked for the one connection whose reply goes
    to that transit address and port, gives the machine's address and
    port. Link indexes are not reused while the host runs, so a network
    made on the transit link of one deleted is told apart, and the
    socket held in a deleted network's namespace is closed with it. The
    agent then names the machine from what its network services read
    from the API. Nothing is read per packet, from a table dump or from
    another log, and nothing depends on journald or conntrack tools, which
    differ between the testbed's Ubuntu 24.04 and the host's 26.04.
  - Conntrack's events carry no time for a new connection, so a line's
    time is when the agent read the event: one goroutine reads events into
    a bounded queue (4096), another joins and writes them.
  - The log lives on its own dataset, `hangar/natlog` at
    `/var/log/hangar/nat`, root's alone, compressed, with a refquota of 20
    GiB, which deploy/host makes; a refquota,
    so a snapshot cannot keep removed days counted. The agent writes it
    itself (`internal/node/natlog`), a JSON line a connection, a file a UTC
    hour, `nat-YYYY-MM-DD-HH.jsonl`, compressed once the hour is over, so only the current hour is plain; a
    record that comes late for an hour the log has moved on from goes to the
    hour being written, as that one may be compressed. A compression is
    written aside, synced, renamed into place and only then is the plain
    file removed; an hour a crash left with both is settled when the log
    opens, keeping the gzip if it holds the whole hour, else the plain file,
    to be compressed again. It removes a day, all its hours, 30 days after
    the day ended. Past 90% of the refquota
    it removes the oldest days first, never today's, as many as their sizes
    cover the excess, and writes a `retention` line saying how many days the
    log now holds; when the removal was for a write the quota refused, that
    line is held and written before the next one that can be. ZFS frees a
    removed file's space seconds later (on the testbed, 4 to 5 s with a
    quota, at once with the refquota the dataset has, with no file of the
    agent's or anyone's held open), so what was removed counts as free until
    usage shows it, counting what was written since, as a flood writes
    faster than the space shows, and a write refused meanwhile waits for
    that room rather than take another day; if the space has not come back
    within a minute, past what was written since, the agent says so once in
    its own log, and counts it as free until it does: days are then removed
    only for usage past the cap without it, and a write the quota refuses
    takes none. ZFS holds back writes near a quota by a fixed amount, not a
    share of it: on the testbed (OpenZFS 2.2.2), appends slowed below about
    25 MiB free and nearly stopped at 5 MiB, and an 8 MiB write was refused
    with 7.5 MiB free, 88% of a 64 MiB quota. 90% of 20 GiB leaves 2 GiB,
    far above that; a write refused all the same takes the oldest day
    whatever the usage reads, and the log opens on a full dataset. A line is
    about 400 bytes, about 32 compressed (realistic lines, measured with
    Go's gzip); the rate limit bounds a network to 100 new connections a
    second, about 3.5 GB a day plain, 280 MB compressed.
  - What remains: a flood still removes older days early once the log as a
    whole passes 90%, oldest first, and one large enough for today alone to
    pass it removes every day before today, leaving the log holding today.
    That takes 18 GiB of today's compressed hours, about 600 million
    connections in a day, about 7,000 a second all day long: some 70
    networks at the rate limit throughout (a plain day would have taken 48
    million, some 6 networks).
  - A window the log may be missing connections in is written into it as a
    gap, never left silent. The gap since the log's last record (the
    agent stopping writes a stop) closes only once the agent's
    subscription is in place, as does the one after following failed,
    from the last thing written. A full socket or queue writes the window
    since the last event queued. A write that fails is told in the
    agent's log once, as is writing working again, which first writes the
    gap since the first write that failed. The socket's buffer holds every
    network's burst at once (300 events of about 2 KiB each a network,
    within 8 to 128 MiB).
  - `hangar-node natlog --port P --at T` finds the machine for a report
    (`docs/runbook/host-bootstrap.md`). The log records when a connection
    started, not when it ended, so the answer is the last connection from
    that port to start by T. It warns of the gaps in the window, and when
    the window starts before the oldest day the log holds, and shows the
    retention lines in it.

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
