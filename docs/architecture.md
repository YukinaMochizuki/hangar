# Architecture

The target system. Parts that do not exist yet are described as they are
planned; the code is the authority wherever it exists.

## Shape

```
              Cloudflare (Access + one Tunnel)
                          │
  ┌───────────────────────┼─────────────────────────────┐ host
  │  cloudflared ─────────┤                             │
  │                       ▼                             │
  │  hangar-apiserver  ◀── watch / status ──┐           │
  │   Connect + WebSocket, SQLite           │           │
  │   controllers in the same process       │           │
  │          │ streams (exec, console)      │           │
  │          ▼                              │           │
  │  hangar-node (root) ────────────────────┘           │
  │   ├ edge proxy        ├ network driver (netns, nft) │
  │   ├ storage (ZFS)     ├ metadata service, DNS       │
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
| `hangar-node` | root | Turns `Machine` objects into running Cloud Hypervisor processes; owns host storage, networking, the edge proxy, and the metadata service |
| `hangar-init` | PID 1 in the guest | Boots containers, runs exec and log streaming, and later handles snapshot hooks |
| `hangar` | the user | CLI. Agent-friendly: non-interactive, `-o json`, stable exit codes |
| web console | browser | React, using the generated TypeScript client |

## Resource model

Every resource has the same envelope: `api_version`, `kind`, `metadata`,
`spec`, `status`. Users own `spec`; controllers own `status`. Apply replaces the
whole `spec` (the desired state); there is no server-side merge.

Users see `Container` and `VirtualMachine`. Controllers turn both into an
internal `Machine`, the only thing the node understands, so snapshots, disks,
networking and quotas are implemented once. Each host is a global `Node`;
a machine runs on the node named in its `spec.node`. Both are system-managed:
users can read their machines, and only platform administrators read nodes.

Resources live under `projects/{project}/<plural>/{name}`. A `Project` belongs
to a `Tenant`; tenants hold people, projects hold resources.

## Data flow for a change

1. A client applies a resource; the apiserver authenticates, authorizes, admits
   and stores it, bumping the global revision.
2. Controllers watching that kind reconcile it, creating or updating
   lower-level objects (a `Container` produces a `Machine`).
3. `hangar-node` watches `Machine` objects, converges the host, and reports back
   through the status RPC.
4. Clients watch or `hangar wait` until `status.observed_generation` catches up
   with the generation they wrote.

## Where the reasoning lives

`docs/decisions/` records why each of these choices was made.
