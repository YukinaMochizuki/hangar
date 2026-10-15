# Architecture

The target system. Parts that do not exist yet are described as they are
planned; the code is the authority wherever it exists.

## Shape

```
              Cloudflare (Access + one Tunnel)
                          │
  ┌───────────────────────┼─────────────────────────────┐ host
  │  cloudflared ─────────┼───────────────────┐         │
  │                       ▼ the API           ▼ *.dev   │
  │  hangar-apiserver ◀── reads ─────── hangar-edge     │
  │   Connect + WebSocket, SQLite      (unprivileged)   │
  │   controllers in the same process         │         │
  │      ▲   │ logs, exec, ssh: over a        │ dial    │
  │      │   ▼ tunnel the node opens          ▼ socket  │
  │  watch / status ── hangar-node (root) ◀───┘         │
  │   ├ storage (ZFS)     ├ network driver (netns, nft) │
  │   ├ metadata, DNS     ├ dials for the edge          │
  │   └ one systemd unit per Cloud Hypervisor process   │
  │          │ vsock                                    │
  │          ▼                                          │
  │  hangar-init (PID 1 / agent inside every guest)     │
  └─────────────────────────────────────────────────────┘
```

## Components

| Component | Runs as | Responsibility |
|---|---|---|
| `hangar-apiserver` | unprivileged user | The API (Connect for unary and server-streaming, WebSocket for bidirectional streams), authentication and authorization, admission, the object store, and every controller |
| `hangar-node` | root | Turns `Machine` objects into running Cloud Hypervisor processes; owns host storage, networking, and what it serves each network from inside its namespace: its resolver, the metadata service, and the edge's connections to public endpoints |
| `hangar-edge` | unprivileged user | The edge proxy for public endpoints (`*.hangarlab.dev`): takes their traffic from cloudflared, checks Access for private ones, routes by `Host`, and reaches machines through the node agent's dial socket (`decisions/public-endpoints.md`) |
| `hangar-init` | PID 1 in the guest | Boots containers from the platform's initramfs, frames their output on the console, serves exec over vsock, and later handles snapshot hooks (`decisions/guest-agent.md`) |
| `hangar` | the user | CLI. Agent-friendly: non-interactive, `-o json`, stable exit codes |
| web console | browser | React, using the generated TypeScript client |

## Resource model

Every resource has the same envelope: `api_version`, `kind`, `metadata`,
`spec`, `status`. Users own `spec`; controllers own `status`. Apply replaces the
whole `spec` (the desired state); there is no server-side merge.

Users see `Container` and `VirtualMachine`: a container boots the platform's
kernel with hangar-init, a virtual machine its image's own kernel through
firmware (`decisions/vm-boot.md`). Controllers turn both into an
internal `Machine`, the only thing the node understands, so snapshots, disks,
networking and quotas are implemented once. Each host is a global `Node`;
a machine runs on the node named in its `spec.node`. Both are system-managed:
users can read their machines, and only platform administrators read nodes.
A machine's interfaces sit on `Network`s of its project, which get their
addresses from the apiserver; every project has a `default` network. A
`Disk` is a durable block device in a project that a machine lists by name,
one machine at a time; it lives on the node of the first machine it was
attached to, and every later machine with it goes there. A disk is blank or
a copy of a global `Image`, a distribution's disk image that platform
administrators add to the catalogue by URL and sha256. A `PublicEndpoint`
exposes one port of a container or VM over HTTPS at
`<endpoint>-<project>.hangarlab.dev`, private to those its tenant allows
unless made public; a container's port marked `public` gets one.

Resources live under `projects/{project}/<plural>/{name}`. A `Project` belongs
to a `Tenant`; tenants hold people, projects hold resources. A person is a
global `User`, which they create by signing up once a platform-role
`RoleBinding` on their email has invited them (`decisions/tenancy-and-iam.md`).

## Data flow for a change

1. A client applies a resource; the apiserver authenticates, authorizes, admits
   and stores it, bumping the global revision.
2. Controllers watching that kind reconcile it, creating or updating
   lower-level objects (a `Container` produces a `Machine`).
3. `hangar-node` watches the `Machine` objects placed on it, converges the host
   through its drivers, and reports back through the status RPC. It keeps no
   state: the host is read back each time (`decisions/node-agent.md`).
4. Clients watch or `hangar wait` until `status.observed_generation` catches up
   with the generation they wrote.

The node also keeps a tunnel open to the apiserver, through which the
apiserver calls it for what only the host reaches, such as a container's logs
and its execs (`decisions/node-tunnel.md`): a node never listens. A client
reaches an exec through a WebSocket on the apiserver, which relays it to the
node (`decisions/streaming-transport.md`); `hangar ssh` reaches a virtual
machine's port 22 the same way, with a key the apiserver's SSH CA signs for
minutes (`decisions/vm-ssh.md`). A public endpoint's traffic never passes the
apiserver: cloudflared hands it to `hangar-edge`, which reads only endpoints
and who may use them from the API, and asks the node agent, over a socket of
its own, for a connection to the endpoint's port
(`decisions/public-endpoints.md`).

## Where the reasoning lives

`docs/decisions/` records why each of these choices was made.
