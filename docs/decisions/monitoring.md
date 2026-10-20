# Monitoring: Prometheus on the host, alerts to Telegram, no tenant in a label

Status: accepted, 2026-10.

## Context

Hangar runs on one host (`ovh-sgp02-01`) that one operator runs, and friends
use it (M4). Until now nothing watched it: the services log to journald,
systemd restarts them, and `deploy/host/verify.sh` checks the host only when
someone runs it. Nothing tells the operator that a disk is filling, an md
mirror has degraded, a certificate is not renewing, the node agent has stopped
reporting, or the API is failing.

The signals Hangar already has: the apiserver's `GET /healthz` (`ok`, no
dependency checks); a Node's `Ready` condition, which the apiserver turns
Unknown when reports stop (`node.proto`); a Machine's phase and its
`MachineReady` reason (`internal/computeapi`); and the edge's certificate and
`/healthz` checks in `85-edge.sh`, run only by `verify.sh`. No status carries a
restart count.

## Decision

- **What is watched.** Four layers:
  - reachability: the API and console over HTTPS, `/healthz`, days left on
    each certificate;
  - the host: CPU, memory, swap, load; each ZFS pool's health, space and
    scrub errors, and `hangar/images` against its quota; the md mirror swap
    is on (`20-swap-raid.sh`); each NVMe disk's SMART health, wear, media
    errors and temperature; time sync;
  - the platform's services: each `hangar-*` unit up and how often it
    restarts; the API's request rate, errors and latency; each Node's
    `Ready`; the edge's requests and errors; the node tunnel;
  - tenants' machines, counted, never named: machines by phase and reason,
    and machines in a crash loop.
- **Alerts are for what the platform did, never for what a tenant's program
  did.** A machine blocked for a reason only the platform causes
  (`DiskFailed`, `NetworkFailed`, …) alerts. A tenant's `Exited`, `BootFailed`
  or crash loop is recorded and shown to the operator but alerts no one.
- **Prometheus and Alertmanager on the host, no Grafana.** Collected by
  `node_exporter` (zfs, mdadm, systemd, hwmon collectors), `smartctl_exporter`
  and the services' own `/metrics`. Prometheus keeps 30 days. Every one of
  them listens on loopback only, so the host firewall stays at 22, 80 and 443
  (`front-door.md`). Ad hoc queries use Prometheus's own UI through an ssh
  tunnel; the operator's view is the console (below).
- **Installed like the rest of the host.** Each binary is pinned by version
  and sha256 in `deploy/host/versions.env`, as Cloud Hypervisor is, and
  installed by a step of `deploy/host` that `verify.sh` checks: never from
  apt, whose builds lag and sit outside main. The alerting rules are files
  in the repo, checked by `promtool` in CI.
- **Alerts go to one private Telegram chat.** Two severities: `critical` is
  sent at once (a service down, a pool or mirror degraded, a disk failing, a
  filesystem nearly full, a machine the platform has kept from running for a
  quarter of an hour); `warning` is held to one daily window and sent as
  one message (a certificate under 14 days, wear rising). The bot token is
  a file Alertmanager reads (`bot_token_file`), installed from the vault
  like the host's other secrets; the chat id is in the host env file.
- **Shaped for more hosts, collected on one.** Every series a host's
  Prometheus scrapes from that host carries its Node's name as `node`; the
  apiserver's are the platform's and name each node themselves.
  `MonitoringService` answers the host layer per node, and a series names
  the node it is for. How several hosts' series are brought together, and
  how their alerts are kept from repeating, is decided when a second host
  comes: it reopens "loopback only".
- **No outside watcher, for now.** Nothing off the host checks it, so the
  host going down, losing its network, or its monitoring stopping alerts no
  one. The owner accepts this for M4. Alertmanager carries an always-firing
  `Watchdog` alert with no receiver yet, so adding a dead man's switch
  (healthchecks.io or alike) later is one receiver.
- **Hangar's services serve `/metrics` on a listener of their own.**
  `hangar-apiserver`, `hangar-node` and `hangar-edge` each take
  `--metrics-listen`, which must be a loopback address, and serve only
  `/metrics` there, with `prometheus/client_golang`: never on the API's mux,
  so no route change can carry it through the front door. A Prometheus
  cannot scrape a unix socket, hence loopback TCP.
- **No tenant in a label.** A metric's labels hold only values the platform
  defines and bounds: a procedure, a code, a phase, a reason, a service, the
  control plane or the endpoints, a node's name, a certificate's name in the
  edge's configuration. Never a tenant, project, machine or
  endpoint name, a uid, a hostname, an address, or an email. Metrics then
  carry nothing of a tenant's, and tenants cannot grow the series by making
  names. A node's name may look like a hostname, but it is the platform's
  own: the operator gives it (`--node-name`), one per host, so it carries
  nothing of a tenant's and grows only with the hosts; each Node's `Ready`
  needs it. A test holds every registered metric to this. An alert that counts
  machines says how to list them (`sudo hangar --context local get …` on the
  admin socket) instead of naming them.
- **The console shows the operator a fixed view, through the API.** A
  platform-level `MonitoringService` in the apiserver answers with typed,
  fixed queries: an overview (each layer's state now), named series over a
  range (the PromQL lives in the apiserver, the name is an enum), and the
  alerts firing (from Alertmanager). It needs `platform.monitoring.read`,
  which only `platform.root` holds. There is no PromQL passthrough: the
  browser cannot ask Prometheus anything the API does not name, and changing
  a query never changes the API. The console shows it on a Monitoring page
  only an administrator sees.

## Consequences

- An outage of the whole host is noticed by its users first, until a dead
  man's switch is added.
- Telling which machines an alert counts takes the admin socket.
- A new graph in the console is a change to the proto and the apiserver.
- **Usage graphs for tenants are later work, and not on this path.** A
  tenant's machine numbers cannot go into Prometheus (no tenant in a label)
  nor into its status (`machine-placement.md`). They will be a pipeline of
  their own: the node agent sampling each VM's cgroup and Cloud Hypervisor's
  counters, read through the tunnel and answered under the machine's own
  permission. Memory in use needs the guest agent.
