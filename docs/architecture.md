# Architecture

The target system. Parts that do not exist yet are described as they are
planned; the code is the authority wherever it exists.

## Shape

```
                                   the internet, :443 and :80
                                   (Cloudflare: DNS alone)
                                                 │
  ┌──────────────────────────────────────────────┼──────┐ host
  │                                              ▼      │
  │  hangar-apiserver ◀── reads, the API ── hangar-edge │
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
| `hangar-apiserver` | unprivileged user | The API (Connect for unary and server-streaming, WebSocket for bidirectional streams), authentication and authorization, admission, the object store, and every controller. It signs people in through Google and issues Hangar's own sessions and tokens (`decisions/authentication.md`) |
| `hangar-node` | root | Turns `Machine` objects into running Cloud Hypervisor processes; owns host storage, networking, and what it serves each network from inside its namespace: its resolver, the metadata service, and the edge's connections to public endpoints |
| `hangar-edge` | unprivileged user | The front door: the only listener on :443 and :80, which systemd binds for it, ending TLS with certificates it gets over ACME DNS-01 and routing by `Host` to the API, the console's files and the public endpoints, within per-client limits (`decisions/front-door.md`). For public endpoints (`*.hangarlab.dev`) it signs private ones' callers in itself, through a handoff from the console or a Hangar token, and reaches machines through the node agent's dial socket (`decisions/public-endpoints.md`) |
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
administrators add to the catalogue by URL and sha256. A `FileShare` is a
filesystem of a project on one to eight of its networks, which the
machines that declare it, a container as a volume and a VM in its spec,
are to mount over NFS from a server microVM of the platform's: a system
`Machine` the share owns, which the scheduler places with the share's
volume, binding the share to that node. Its API and that machine are
built so far, not what the machine boots nor the mounts
(`decisions/file-shares.md`). A global
`MachineSize`, also the administrators' and read by all, is a template of
cpu, memory and disk that clients fill a new machine's from, and that
admission holds tenants' machines to; a machine does not record its size
(`decisions/machine-sizes.md`). A `PublicEndpoint`
exposes one port of a container or VM over HTTPS at
`<endpoint>-<project>.hangarlab.dev`, private to those its tenant allows
unless made public; a container's port marked `public` gets one. A
`Secret` holds a project's credentials, such as registry logins that a
container names to pull its image with: its values are write-only and
stored sealed under a key outside the database, and a node gets a login
only for a pull of a machine placed on it (`decisions/secrets.md`).

Resources live under `projects/{project}/<plural>/{name}`. A `Project` belongs
to a `Tenant`; tenants hold people, projects hold resources. A person is a
global `User`, which they create by signing up once a platform-role
`RoleBinding` on their email has invited them, starting with what their
`Invitation` says, and which comes with a tenant and project of their own
(`decisions/tenancy-and-iam.md`).
An agent is a project's `ServiceToken`, whose secret it sends as a bearer
token; bindings name it as `<project>/<name>`, and it holds only what they
grant (`decisions/authentication.md`).
A tenant's budget is a global `Plan`, which admission checks every write
that would raise the tenant's usage against; a project may have a smaller
budget of its own, and both report their usage in `status`.

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
minutes (`decisions/vm-ssh.md`). From the internet, everything comes in
through `hangar-edge` on :443, which hands the API to the apiserver over a
unix socket of its own, saying each client's address
(`decisions/front-door.md`); Cloudflare is the DNS alone, pointing every
name at the host (`decisions/cloudflare-setup.md`). Machines on the host
reach the front door at the host's public addresses too, :80 and :443
alone, so they call the platform by its public names. A public
endpoint's traffic never passes the apiserver: `hangar-edge` takes it on
:443, reads only endpoints and who may use them from the API, and asks the
node agent, over a socket of its own, for a connection to the endpoint's
port
(`decisions/public-endpoints.md`).

## Metrics

Given `--metrics-listen`, `hangar-apiserver`, `hangar-node` and
`hangar-edge` each serve `/metrics`, and nothing else, on a loopback address
of their own, for the host's Prometheus: never on an API's mux, so no route
reaches it through the front door (`internal/metrics`). The apiserver counts
its Connect calls by procedure and code, each Node's `Ready`, and machines
by phase and reason and in a crash loop, never naming one. The node agent
counts its tunnel, its failed reports, its image pulls and its image cache.
The edge counts the front door's requests by route (control plane, console,
endpoints) and status class, what it answered itself when the apiserver or
an endpoint did not, and when each of its certificates expires. A metric's
labels hold only values the platform defines and bounds; a test holds every
registered metric to the list in `internal/metrics`
(`decisions/monitoring.md`). `deploy/host` gives them `127.0.0.1:9101`,
`:9102` and `:9103`, and its Prometheus scrapes them there and alerts on
them (`deploy/host/monitoring/rules/services.yml`).

The other way round, `MonitoringService` (`hangar.monitoring.v1alpha1`)
asks the host's Prometheus and Alertmanager, given `--prometheus-url` and
`--alertmanager-url` (loopback only), with fixed queries whose PromQL is in
`internal/apiserver/monitoringqueries.go`, and answers only callers holding
`platform.monitoring.read`. Without them its calls fail with `Unavailable`.
It answers reachability and machines for the platform, and the host and its
services for each node, by the `node` label the host's series carry. The
services step passes both flags (`deploy/host/steps/80-services.sh`).

## Where the reasoning lives

`docs/decisions/` records why each of these choices was made.
