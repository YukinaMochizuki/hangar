# A machine's status says what hardware it runs on, never the host's load

Status: accepted, 2026-10.

## Context

The console shows where a machine runs: a "runs on" panel and the host card
of a container's or VM's Overview, with the region and site, the CPU, the
storage and a picture of the chassis. Tenants cannot read `Node`
(`docs/decisions/machine-model.md`), and a Node holds what no tenant may
see: capacity, allocatable, the IPv6 prefix, the agent's version, and
through its machines what other tenants run.

## Decision

- **The description is on the machine's status, not in a new service.**
  `ContainerStatus.placement` and `VirtualMachineStatus.placement` are a
  `Placement` (`proto/hangar/compute/v1alpha1/placement.proto`):

  | Field | Example | From |
  |---|---|---|
  | `region` | `ap-southeast` | the node's configuration, `--region` |
  | `site` | `SGP02` | the node's configuration, `--site` |
  | `cpu_model` | `AMD EPYC 7313` | `/proc/cpuinfo`'s model name, without its core count |
  | `cpu_generation` | `Zen 3 (Milan)` | the agent's table of EPYC generations by CPUID family and model, or `--cpu-generation` |
  | `storage` | `2 × NVMe SSD` | the host's physical disks in `/sys/block`, by kind and count, or `--storage` |
  | `chassis` | `generic-1u-2x-u2` | the node's configuration, `--chassis` |

  Every field is empty when the node does not know it.
- **The node describes itself; the apiserver copies.** The agent reports a
  `Placement` in `NodeStatus.placement` at every heartbeat, from what it
  reads of the host and what its flags say (a CPU generation or storage
  given wins over what it reads). A failed read keeps the CPU and storage
  it last reported. The container and VM controllers set their owner's
  placement to a copy of the node's whenever they report, and they
  reconcile on every Node change, so the placement appears once the machine
  is placed and follows the node's description when it changes. A machine
  on no node, or whose node is gone, has none.
- **Only `Placement` reaches tenants.** It is a message of its own holding
  hardware alone, and the controllers copy it whole: nothing else of the
  Node (load, capacity, allocatable, addresses, version, other tenants'
  machines) is written to a tenant's object. A field added to `Placement`
  is one every tenant reads. The core count is cut from the CPU model, since
  it is the host's size; the disk count is how the host is built, not what
  is free on it.
- **The chassis is an identifier, not a description.** It is a DNS-1123
  label the console maps to an illustration; one it does not know, and an
  empty one, it draws as a generic server. The agent refuses a chassis that
  is not a label.
- **Production values are the deploy's.** `deploy/host` passes
  `HANGAR_REGION`, `HANGAR_SITE` and `HANGAR_CHASSIS` from the host env
  file to the agent when they are set, and nothing when they are not
  (`docs/runbook/host-bootstrap.md`). The values in this file are examples.

## Consequences

- A CPU the table does not know, any Intel CPU among them, has an empty
  generation until its host's `--cpu-generation` names it; the table grows
  as hosts do.
- The storage line counts every physical disk of the host, the boot disks
  included, not only the pool's: a host whose pool is on some of its disks
  says what it means with `--storage`.
- Every heartbeat that changes nothing in the placement changes no
  container or VM: their status is written only when it differs.
