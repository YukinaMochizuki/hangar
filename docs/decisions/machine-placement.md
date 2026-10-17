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
  | `memory` | `DDR4-3200 ECC` | the SMBIOS table (`/sys/firmware/dmi/tables/DMI`): the type of the installed modules (Memory Device), the slowest speed they run at, and ECC if a system memory array (Physical Memory Array) corrects errors; or `--memory` |
  | `network` | `2 × 10 GbE` | every physical Ethernet port in `/sys/class/net`, its link up or not, by the most speed it supports: the fastest of the link modes its driver lists over ethtool netlink (`LINKMODES_GET`), never the speed its link runs at; or `--network` |

  Every field is empty when the node does not know it.
- **Nothing in a placement follows use or state.** It says what the host
  is built of, so nothing in it changes because of what the host does: no
  load, no free space, no link state, no negotiated speed, no disk dropping
  off the bus. A port counts whether its cable is in or not, at the speed
  it is built for.
- **The node describes itself; the apiserver copies.** The agent reports a
  `Placement` in `NodeStatus.placement` at every heartbeat, from what it
  reads of the host and what its flags say (a CPU generation, storage,
  memory or network given wins over what it reads). Storage, memory and
  network are read once, as the agent starts, and kept until it starts
  again: what the host does meanwhile never changes them. A read that
  could read only some of the host, such as a port whose speed cannot be
  read, gives what it could and logs the rest; one that reads nothing is
  logged, and the field is what the configuration says, or empty. The CPU
  is read at every heartbeat; a read that fails keeps what was last
  reported, and one that keeps failing is logged when it starts failing
  and when it works again. The container and VM controllers set
  their owner's placement to a copy of the node's whenever they report, a
  report of a change they cannot apply included, and they reconcile on
  every Node change, so the placement appears once the machine is placed
  and follows the node's description when it changes. A machine on no
  node, or whose node is gone, has none.
- **Only `Placement` reaches tenants, and it is bounded.** It is a message
  of its own holding hardware alone, and the controllers copy it whole:
  nothing else of the Node (load, capacity, allocatable, addresses,
  version, other tenants' machines) is written to a tenant's object. Every
  field is a string every tenant with a machine on the node reads, so an
  operator must never put load, capacity or anything else that changes
  with use into one. The apiserver refuses a Node status whose placement
  has a field over 64 characters or holding a character that is not
  printable (`internal/computeapi/placement.go`). Fields say what the
  hardware is, never how much of it: the core count is cut from the CPU
  model, since it is the host's size, and memory is described by kind,
  never by size or module count; the disk and port counts are how the host
  is built, not what is free on it.
- **The chassis is an identifier, not a description.** It is a DNS-1123
  label the console maps to an illustration; one it does not know, and an
  empty one, it draws as a generic server. The apiserver refuses a chassis
  that is not a label.
- **A bad description never stops a node.** The agent reports a configured
  value the apiserver would refuse as empty, and logs that once when it
  starts; a chassis that is not a label is then drawn generic. A value read
  from the host that the apiserver would refuse counts as a failed read.
- **Production values are the deploy's, checked by the agent's own rule.**
  `deploy/host` passes `HANGAR_REGION`, `HANGAR_SITE`, `HANGAR_CHASSIS`,
  `HANGAR_MEMORY` and `HANGAR_NETWORK` from the host env file to the agent
  when they are set, and nothing when they are not. Before it writes the
  agent's unit it runs `hangar-node check-placement` on them with the
  hangar-node the run installs (the release's, once it matches the
  release's `SHA256SUMS`, with `--release`; else the installed one), which
  applies the rule the agent and the apiserver apply
  (`internal/computeapi/placement.go`), and refuses the run if any fails;
  no second implementation of the rule lives in the deploy. A hangar-node
  from before `check-placement`, which its usage shows, is never run with
  it, since it would start an agent: the step warns that the values were
  not checked. Each value goes into the unit quoted, with what systemd
  reads in quotes escaped (`\`, `"`, `%`, `$`)
  (`docs/runbook/host-bootstrap.md`). The values in this file are examples.

## Consequences

- A CPU the table does not know, any Intel CPU among them, has an empty
  generation until its host's `--cpu-generation` names it; the table grows
  as hosts do.
- The storage line counts every physical disk of the host, the boot disks
  included, not only the pool's: a host whose pool is on some of its disks
  says what it means with `--storage`.
- Memory is read from the SMBIOS table with no tool (dmidecode reads the
  same table); a type the agent has no name for, such as a virtual
  machine's `RAM`, leaves memory empty, and firmware that misreports its
  modules is corrected with `--memory`.
- A port whose driver lists no link mode with a speed (a virtual
  machine's virtio port, among others) cannot be rated: it is left out of
  the network line, and logged, as the agent starts. With no port rated,
  the line is what `--network` says, or empty.
  Ports bonded together count one by one. A line that would pass 64
  characters is summed up as how many ports there are and the fastest,
  such as `13 Ethernet ports, up to 100 GbE`.
- Every heartbeat that changes nothing in the placement changes no
  container or VM: their status is written only when it differs.
