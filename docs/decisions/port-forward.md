# Port forwards: reaching any TCP port of a machine through the API

Status: accepted, 2026-10.

## Context

A machine's addresses are private to its networks. `hangar ssh` reaches a
VM's port 22 through the API (`vm-ssh.md`), and a `PublicEndpoint` serves
one HTTP port to the internet through the edge (`public-endpoints.md`).
Nothing reaches any other port of a machine from outside its network: a
dev server in a container, a database in a VM, an editor's WebSocket. A
client that drives machines for others, such as a control layer over
Hangar that hands its users ports of their environments, needs that
without opening a public endpoint per port or a `PortForward` of the
shared address to the internet.

## Decision

- **A port forward is a machine capability**, as exec and ssh are
  (`machine-model.md`): `GET
  /v1/projects/{project}/{plural}/{name}:portforward?port={port}`, a
  WebSocket with the subprotocol `hangar.portforward.v1alpha1`, whose
  binary messages are one TCP connection's bytes. A Container's and a
  VirtualMachine's alike: unlike ssh, it needs nothing in the guest. The
  query names one port, 1 to 65535; the caller names the object and the
  port, never an address: the node dials the machine's first interface.
  TCP only.
- **It needs `compute.machines.portforward`**, a reach permission the
  owner and editor roles and `platform.root` hold, as they hold exec and
  ssh. It is its own permission, not ssh's or exec's: one who may run a
  process could listen on a port and reach it anyway, but one who may only
  forward ports runs nothing.
- **The path is ssh's**: the apiserver authorizes, refuses before the
  upgrade as an exec does, then calls the machine's node over its tunnel
  (`NodeService.PortForward`, `node-tunnel.md`), which dials the port from
  the network's gateway inside its namespace and relays bytes until either
  side ends. Its bytes pass through the apiserver, as an exec's and an SSH
  tunnel's do. A port nothing answers at is known only once the WebSocket
  is open: it closes as `Unavailable`, naming the port. The audit lines
  `portforward opened` and `portforward closed` say who, which object,
  machine and port, how long and how many bytes, never the bytes.
- **Security groups do not apply, and the opening is the node's socket
  mark.** A port forward reaches whatever listens on the port, as AWS
  SSM's port forwarding does; RBAC is the gate. The node marks a port
  forward's socket (`SO_MARK`, `anyPortMark` in
  `internal/node/driver/hostnet`), and one rule of each network's firewall,
  in its output chain, lets marked TCP from the gateway's address to any
  port of a tap through (`networking.md`). The rules for ssh's port 22 and
  an endpoint's named ports stay as they were: the edge's dial socket
  still reaches only the ports endpoints name, the firewall holding that
  behind the node's own check. No other socket in a namespace sets a mark,
  and a guest's packets never pass the output chain. A file share's
  server's taps go to their own chain before the rule, and get nothing.
- **`hangar port-forward KIND NAME [LOCAL:]REMOTE`** listens on a local
  port, on 127.0.0.1 unless `--address` says otherwise, and opens one port
  forward per connection; `--stdio` relays stdin and stdout as one
  connection instead, as `hangar ssh-proxy` does for port 22.

## Consequences

- A token with the owner or editor role reaches every listening port of
  every machine in its project, whatever the security groups say. Groups
  remain what decides machine-to-machine and internet traffic.
- Every byte of a forward crosses the apiserver and the node tunnel. Heavy
  or long-lived traffic is better served by a `PublicEndpoint` or a
  `PortForward` of the shared address.
- A forward to a port nothing answers at is a WebSocket that opens and then
  closes, not an HTTP error.
- A forward carries no half-close: the client ending its side, as
  `hangar port-forward --stdio` does at the end of its stdin, closes the
  connection whole, and a reply the guest had yet to send is lost. A client
  that sends a request and then shuts its write side, as some HTTP/1.0
  clients and `nc -N` do, gets nothing back; one that keeps its side open
  until it has read the reply, as interactive protocols, HTTP/1.1 and
  WebSockets do, is unaffected.
