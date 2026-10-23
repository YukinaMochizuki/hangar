# The apiserver calls a node over a tunnel the node opens; logs are one stream; exec is docker exec

Status: accepted, 2026-10.

## Context

`hangar logs`, and next `hangar exec`, need what only the node can reach: a
container's console log is in `/var/lib/hangar/vms/<uid>`, root's alone, and
its guest agent is behind the VM's vsock socket. Until now only the node
called the apiserver, over the node socket that `SO_PEERCRED` authenticates
(`authentication.md`); the apiserver had no way to call a node.

## Decision

- **The node opens a tunnel to the apiserver, and the roles reverse on it**
  (`internal/tunnel`). The agent dials the node socket, the same listener and
  identity as its reports, and upgrades an HTTP/1.1 request
  (`Upgrade: hangar-node-tunnel/1`); after the `101` it serves HTTP/2 on the
  connection and the apiserver is the client. A node never listens, there is
  one identity mechanism, and deploying needs nothing new.
  - The tunnel is the node the listener authenticates the request as, the
    name PeerCred is configured with, never anything the request says. Only
    a node principal of a node this apiserver runs opens one.
  - One tunnel per node: a newer replaces an older, as when an agent
    restarts while its old connection lingers.
  - Both ends ping a tunnel silent for 10 s and drop it if the ping is not
    answered in 10 s more, so a dead tunnel goes within 20 s. The agent
    dials again, from 200 ms, twice as long after each failure, up to 5 s.
  - A call to a node with no tunnel, or on a tunnel that drops under it,
    fails with `Unavailable`, a drop told by the tunnel's HTTP/2
    connection being closed. An agent stopping answers its calls
    `Unavailable`, and gives them 2 s to answer before it closes the
    tunnel.
  - Calls to nodes open at once are capped: 64 per caller, counted by the
    principal the call is authenticated as, so making tenants, as any
    member may, gives no one more; 64 per tenant (the tenant of the
    container's project), so no tenant takes a node's streams from the
    others; and 900 per node, below the 1000 streams an agent takes on its
    tunnel, leaving room for the apiserver's own calls. A call over a cap
    fails at once with `ResourceExhausted`; none waits. Logs, exec and
    uploads share the caps. They are counted in each apiserver process,
    which holds while there is one.
  - Each end of a tunnel receives with a window of 1 MiB per stream and of
    1000 MiB for the connection, every stream's at once. A call whose
    caller stops reading holds its stream's window until it ends; were the
    connection's smaller than the streams' together, a few hundred such
    calls would hold it all, and no call on the tunnel would get data while
    pings still went through. The cost is memory: the apiserver keeps up to
    1 MiB unread per call, so at most 64 MiB for a caller and 900 MiB for a
    node. x/net's HTTP/2 client passes no window to net/http, so the
    apiserver's end is net/http's own, on the connection it took over.
  - Not the node listening and the apiserver dialing: that takes a second
    trust setup, and inbound ports on every node once there are several.
- **The API is apart from its transport.** The node serves an internal
  `hangar.node.v1.NodeService` (versioned and held by buf breaking like
  `hangar.guest.v1`, since the two ends upgrade apart); the apiserver reaches
  it through one function that gives a client for node X. Another transport,
  such as clients reaching a node directly for heavy streams, changes only
  that function. Exec will be a Connect bidirectional call on the same
  tunnel; it never reaches a browser (`streaming-transport.md`).
- **A container's logs are one stream across its boots.** A restart boots the
  same root disk again, as restarting a Docker container does, so one read
  covers every boot the host keeps: stdout, stderr, hangar-init's messages,
  and how each boot ended, each record with its boot's number. `hangar logs`
  prints stdout to stdout, stderr to stderr, and the rest to stderr as
  `hangar:` lines; `-f`, `--tail`, `--since`, `--timestamps`, and `-o json`
  with each record's stream and boot.
  - Boots are numbered by the host: the launcher writes a `Boot` record at
    each start, one past the last in the log, and again at the head of each
    file it rotates to, so whichever files a read finds, it numbers boots
    alike. The guest's own `Boot` records are dropped as any unknown stream
    is, so the numbering cannot be forged. A boot that ends with no exit
    recorded, as in a crash or a guest killed when it is stopped, gets an
    exit of 253 from the launcher in its log (never in `exit.json`), so
    every boot's log ends with an exit.
  - A follow ends with the call, or once the VM, seen running, has exited for
    good or is stopped. It reads through rotations, the file rotated between
    included when the log rotates twice between two reads; a third rotation
    that fast loses the file between.
- **Reading logs is a permission of its own**, `compute.machines.logs`, apart
  from `get`: output can hold what the spec does not, such as secrets a
  process prints. It is the machine's, one for every kind that runs as one
  (`machine-model.md`), read through `MachineCapabilityService.Logs` by the
  kind and name of the object. The owner and editor roles and
  `platform.root` hold it; a sponsor does not.
- **What the console holds is the tenant's**, so every read is bounded: a
  record holds at most 64 KiB, a tail keeps back about 4 MiB of memory,
  each record counted with what it takes beyond its data (the allocator's
  rounding can make it up to about twice that), batches are
  about 256 KiB, a message from a node at most 1 MiB, and the reader holds at
  most a record and a read of a file, whatever is in it. Bytes that are no
  record are skipped; forged records garble only the container's own log.
  Follows wait on the client: a slow reader slows the node's read through
  HTTP/2's flow control, past the stream's window of 1 MiB.
- **An exec runs as `docker exec` does, for as long as its call.** The
  client's WebSocket (`streaming-transport.md`) is relayed to the node's
  `NodeService.Exec`, a bidirectional Connect call on the tunnel, and on to
  the guest agent's `Exec` (`guest-agent.md`).
  - The process inherits the container's environment, user and working
    directory, as its VM was started (the host keeps that spec), with
    what the exec sets instead. It has no stdin unless asked: without a
    terminal its stdin is closed at once; on a terminal nothing is typed.
    On a terminal, stdout and stderr are one stream. The exit, the
    signal that caused it included, comes last, its code 0 to 255
    whatever the guest says; `hangar exec` exits with that code, and with
    125, as `docker exec` does, when the exec itself fails.
  - Each end of the relay ends the next: a client that closes, drops, or
    is taken to be gone ends the apiserver's call to the node, which
    resets the stream on the tunnel; the node's call to the guest ends
    with it, on a vsock connection of its own that is closed, and
    hangar-init kills the process's group. A VM that stops or restarts
    ends its execs as `Unavailable`.
  - The apiserver holds a message each way at a time: what one side does
    not read holds up the other past the WebSocket's buffers and the
    stream's window on the tunnel, never memory in the apiserver. A
    message is at most 256 KiB from a client and 64 KiB of output from a
    node.
  - Data backed up never drops a live client (`internal/execws`). Each
    end pings the other every 30 s, and hears it in anything it sends: a
    pong, its own ping, a message. A reader held up by data backed up
    takes no pong, so silence counts only once nothing has been pending,
    either way, for a minute: no write to the client and no send to the
    node held up. While something is, a dead peer is TCP's to tell: the
    apiserver's connections have keepalive (30 s idle, then a probe every
    10 s, three missed) and a user timeout of 60 s for unacknowledged
    writes, and an empty message written each tick fails on a connection
    that broke. `hangar exec` tells a server gone in the same way.
  - Refused before the WebSocket opens, in this order: whoever is not
    authorized (`compute.machines.exec`, told nothing of the container,
    as a read is not; a kind without exec, such as a VM for now, is
    `Unimplemented` before that), a container without a machine on a node
    or whose machine is not running (`FailedPrecondition`), a node not connected
    (`Unavailable`), and a caller over the caps on calls to nodes
    (`ResourceExhausted`, shared with logs). After it opens, the node can
    still refuse it, as for a VM that stopped since.
  - The apiserver logs every exec: when its process is asked of the node,
    the caller, the container, its machine, the command, whether it has a
    terminal or stdin, and the user, working directory and names of the
    environment it sets; when it ends, how, and how long it took; and
    every attempt refused, by whom, for which container, with which code.
    Never its input, its output, or its environment's values.
- **Running a process in a container is a permission of its own**,
  `compute.machines.exec`, apart from `get` and `logs`: it reaches
  everything the container can. It is the machine's, as logs are.
  The owner and editor roles and `platform.root` hold it; a sponsor does
  not.

- **A chunk of a CustomImage's upload is a client-streaming call.** The
  apiserver asks the node where the upload is
  (`NodeService.GetCustomImageUpload`, unary), refuses a chunk not
  starting there, then relays the request's body to
  `NodeService.UploadCustomImage`: a first message saying where the chunk
  goes, its length and its sha256, then its bytes, 256 KiB a message, the
  apiserver holding no more than one; the answer is the node's new count
  (`docs/decisions/custom-images.md`). Both calls take a slot of the caps
  above, as logs and exec do, for as long as they last. A client that
  stops sending holds its stream's window, not the apiserver's memory; one
  that goes away ends the call, and the node counts nothing of the chunk.

## Consequences

- A client going away ends the node's read, or its exec: the apiserver's
  call is the client's request's, and HTTP/2 resets the stream on the
  tunnel.
- A node's logs are unreadable, and its containers cannot be exec'd in,
  while its agent is down, though the VMs run on and their logs are kept.
- An exec lasts no longer than its connection: an apiserver or agent
  restart, or a tunnel dropping, kills it.
- Times in a log are the guest's clock; `--since` trusts them.
