# Endpoint services: one machine's port, reached from other networks

Status: accepted, 2026-10.

## Context

Each `Network` is its own namespace, ranges may overlap, and private IPv4
between networks is dropped (`networking.md`). That isolation is what a
multi-tenant client wants: a control layer over Hangar puts one network
per environment so environments cannot see each other. Some services must
still be reached from many networks at once, as a git mirror every
environment clones from. Joining every network does not scale: a machine
has at most 8 interfaces, a file share serves at most 8 networks, and
changing either restarts what changes. Peering does not fit either, as
ranges may overlap (GitHub issue #219).

## Decision

- **Two kinds, as AWS PrivateLink and GCP Private Service Connect have
  them.** An `EndpointService` offers one TCP or UDP port of a
  VirtualMachine or Container: `spec.target`, `spec.protocol` (immutable)
  and `spec.port`. A `NetworkEndpoint` is one network's way to one service
  (`spec.network`, `spec.service`, both immutable). Adding or removing a
  network's way to a service creates or deletes its own object, never an
  edit of a shared list, so a client opening and closing environments
  never contends on the service. One service carries one port; several
  ports are several services.
- **A service reaches its machine's first interface (eth0) alone.** The
  machine's replies follow its default route, which is eth0's: a service on
  another interface would be reached and never answer.
- **The service decides which networks of its project may have a way in**
  by `spec.networkSelector`, a label selector over the networks' labels in
  the API's own syntax (`internal/apimachinery/labels`); empty is every
  network of the project. A network whose labels stop matching loses its
  way in, and its open connections end. Nothing crosses projects.
- **A network endpoint is an address of its own network's range**, drawn
  by the apiserver as a machine's is (the lowest free one, under the same
  lock, `ipam`), held until it is deleted and its node has let go of it, so
  overlapping ranges do not matter. Its network's resolver answers its name,
  `<endpoint>.<project>.hangar.internal`, unless a machine of the network
  has that name, which keeps it; the endpoint is then reached at its
  address alone (`status.hostname` empty). The name is answered once the
  endpoint has its address, whether or not it is `Ready`: a name that comes
  and goes with the service would be cached as gone by clients. The endpoint is its network's
  object (an owner reference): deleting the network deletes it.
- **The host carries the traffic with NAT, three layers, as port forwards
  do** (table `ip hangar_endpoint` in each layer, apart from `ip hangar`):
  - in the endpoint's network's namespace, the bridge holds the endpoint's
    address as a /32 labelled `hn-br:ep`, so the bridge answers ARP for it.
    Its port, from the bridge, is marked and DNATed to the service's
    network's transit address and the service's transit port; it leaves by
    `hn-up` SNATed to the endpoint network's transit address, as anything
    else does. Anything else to the address is dropped in the namespace's
    input, so it reaches none of the namespace's sockets;
  - in the root namespace, a chain before the driver's forward chain marks
    a new connection from that network's veth to that transit address and
    port, for each network with an endpoint of the service, and `ip hangar`
    lets across from one network's veth to another's what carries that mark
    and not the NAT log's tag (one more rule, always there). Nothing else
    crosses from one network to another;
  - in the service's network's namespace, the transit port coming in by
    `hn-up` is marked and DNATed to the machine's address and port, the
    source kept.
  Each layer's conntrack is its own, so each marks its own entries
  (`ct mark` bit 29, apart from the forwards' bit 30 and the NAT log's
  bit 31). A network that bypasses its endpoint's address and sends to the
  service's transit address and port itself is let through only if it has
  an endpoint of the service, as it would be at the address.
- **A transit port is drawn by the apiserver when the service is made**,
  from 1024 to 9999 (below the ports PortForwards draw, which the service's
  namespace takes at the same transit address), unique per protocol across
  the platform rather than within the service's network, which its machine
  may leave, and kept until the service is deleted.
- **The producer's security groups decide.** The machine sees each
  connection come from the transit address of the network it came from,
  never from a machine of it. A group's ingress rule names such a network
  with a `network` peer, which the node resolves to that address; a CIDR
  peer of 100.64.0.0/10 stands for every network. Only a network's own
  namespace sends from its transit address: the root namespace drops that
  range coming in by the uplink, where a forged source could otherwise
  reach a machine through a port forward and pass for a network. Ingress being denied
  unless a rule allows it, a new service reaches no one until a group of
  its machine's eth0 lets its networks in. A `network` peer is for ingress
  alone, and a network not made yet matches no one, as a group does.
- **The node agent ensures the whole set at once**
  (`driver.Network.EnsureEndpoints`), from the NetworkEndpoints the
  apiserver says are `Ready` and the services they reach, on changes to
  them, to machines and networks and on the driver's change signals. The
  tables and addresses are written only when they differ. Conntrack would
  keep an open flow on the NAT it was given past any change, and the root
  table's "only replies into a network" passes an established one, so the
  driver deletes, in each of the three layers, the entries an endpoint let
  in (its mark) that the set no longer accounts for, before it returns; in
  the service's namespace that is by the network the entry came from too,
  as another may still reach the service. The root table is written first,
  and whatever becomes of the namespaces, so a way taken away goes even
  while another network's namespace cannot be written; a namespace not
  whole loses every endpoint it held.
- **Nodes.** A service's `status.node` is its machine's, and a network
  endpoint's its service's, kept when the service goes. A node reads only
  the services and endpoints it carries, and reports `Released` on one
  being deleted once it no longer carries it; until then the apiserver
  keeps it, its transit port and its address. With one node, as now, the
  endpoint's network and the service's are on the same host; endpoints
  across nodes are not built.
- Plans limit network endpoints (`limits.network_endpoints`, 20 by
  default), each holding an address and a rule set on the host; services
  hold nothing of their own on the host and are not counted.

## Consequences

- A connection through an endpoint never leaves the host and is in no NAT
  log; the abuse protection's rate limit counts it as a new connection out
  of its network, and TCP to ports 25 and 465 is dropped across networks
  as it is to the internet.
- A machine's security groups see the network, not the machine, that
  connected: what the producer can tell apart is networks.
- The service's machine may answer on any interface's default route but
  eth0's only by luck; services on eth1 and later are not offered.
- Each endpoint adds a rule to its network's namespace, one to the
  service's, one to the root's, and an address to its network's bridge; the
  root's forward chain grows by one rule whatever there is.
