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
  | `cpu_model` | `AMD EPYC 7313` | `/proc/cpuinfo`'s model name, without the core count AMD ends it with (`16-Core Processor`, `16-Core`, `64-Cores`) |
  | `cpu_generation` | `Zen 3 (Milan)` | the agent's table of EPYC generations by CPUID family and model, or `--cpu-generation` |
  | `storage` | `2 × NVMe SSD` | the host's physical disks in `/sys/block`, by kind and count, or `--storage`: a block device with a device behind it and not removable, the namespaces of one NVMe controller counted once (loop devices, zvols, RAID arrays and device-mapper targets are not disks) |
  | `chassis` | `generic-1u-2x-u2` | the node's configuration, `--chassis` |

  Every field is empty when the node does not know it.
- **The node describes itself; the apiserver copies.** The agent reports a
  `Placement` in `NodeStatus.placement` at every heartbeat, from what it
  reads of the host and what its flags say (a CPU generation or storage
  given wins over what it reads). The CPU and the disks are read apart: one
  whose read fails keeps what it last reported while the other is still
  read, and a read that keeps failing is logged when it starts failing and
  when it works again, not at every heartbeat. The container and VM
  controllers set their owner's placement to a copy of the node's whenever
  they report, a report of a change they cannot apply included, and they
  reconcile on every Node change, so the placement appears once the machine
  is placed and follows the node's description when it changes. A machine
  on no node, or whose node is gone, has none.
- **Only `Placement` reaches tenants, and it is bounded.** It is a message
  of its own holding hardware alone, and the controllers copy it whole:
  nothing else of the Node (load, capacity, allocatable, addresses,
  version, other tenants' machines) is written to a tenant's object. Every
  field is a string every tenant with a machine on the node reads, so an
  operator must never put load, capacity or anything else that changes
  with use into one. The apiserver refuses a Node status whose placement
  has a field over 64 characters or holding a character that is not
  printable (`internal/computeapi/placement.go`). The core count is cut
  from the CPU model, since it is the host's size; the disk count is how
  the host is built, not what is free on it.
- **The chassis is an identifier, not a description.** It is a DNS-1123
  label the console maps to an illustration; one it does not know, and an
  empty one, it draws as a generic server. The apiserver refuses a chassis
  that is not a label.
- **A bad description never stops a node.** The agent reports a configured
  value the apiserver would refuse as empty, and logs that once when it
  starts; a chassis that is not a label is then drawn generic. A value read
  from the host that the apiserver would refuse counts as a failed read.
- **Production values are the deploy's.** `deploy/host` passes
  `HANGAR_REGION`, `HANGAR_SITE` and `HANGAR_CHASSIS` from the host env
  file to the agent when they are set, and nothing when they are not; it
  refuses a region or site that is not one word of at most 64 characters,
  and a chassis that is not a DNS-1123 label, so a bad value fails the
  deploy rather than reaching the agent (`docs/runbook/host-bootstrap.md`).
  The values in this file are examples.

## Consequences

- A CPU the table does not know, any Intel CPU among them, has an empty
  generation until its host's `--cpu-generation` names it; the table grows
  as hosts do.
- The storage line counts every physical disk of the host, the boot disks
  included, not only the pool's: a host whose pool is on some of its disks
  says what it means with `--storage`.
- Every heartbeat that changes nothing in the placement changes no
  container or VM: their status is written only when it differs.
