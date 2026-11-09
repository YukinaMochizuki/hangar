# A machine's usage: sampled by its node, kept in memory, read under the object's get

Status: accepted, 2026-11.

## Context

The console shows each Container's and VirtualMachine's CPU and memory over
the last hour on its detail page. Nothing per machine existed: the host's
Prometheus may carry no tenant in a label, and a machine's status carries
its node's hardware and nothing of its load (`monitoring.md`,
`machine-placement.md`). The numbers are only on the host: the cgroup of
each VM's unit, and, for what a guest uses of its memory, the guest itself.

## Decision

- **The node agent samples, and keeps it in memory alone.** Every 10
  seconds it reads each running machine's unit's cgroup
  (`hangar.slice/hangar-vm-<uid>.service`): `cpu.stat`'s `usage_usec` and
  `memory.current`. Per machine it keeps one point a minute, at most 60,
  and the latest sample with a CPU value, which it answers only while it is
  at most three samples old. Nothing is stored, written to a status, or
  exported to Prometheus: a restart of the agent empties it, and a machine
  that stops or leaves the node is forgotten. An hour of history is what
  the page shows; keeping it where it is measured needs no storage, no
  schema and no cleanup, and costs a few kilobytes a machine.
- **CPU is a share of the machine's vCPUs, 0 to 1.** A sample's is the CPU
  time the unit used since the one before, over the time passed times the
  vCPUs; a minute's point is the minute's CPU time over its time. It counts
  the VMM's own work on the host too, and is clamped at 1. A sample more
  than two polls after the one before gives no CPU, so a gap shows as
  minutes with no point rather than as its whole CPU time averaged into the
  minute after it; so does a counter read in another boot, or going back.
- **Memory depends on the kind.** A container's is what its guest says it
  uses, MemTotal less MemAvailable of its `/proc/meminfo`, asked of
  hangar-init (`MemoryUsage`): it is what the tenant's processes take, cache
  the guest could free not counted. A guest that cannot say, from before it
  could, gives no memory value, and is asked again only in its next boot;
  one that does not answer in time is not asked again for a minute, so a
  hung guest does not hold up the samples of the others. A virtual
  machine's is its unit's `memory.current` on the host, since the platform
  has no agent in its guest: it includes the guest's page cache and the
  VMM's own overhead, and seldom shrinks. That is accepted, and the answer
  says which of the two it is.
- **The apiserver reads it from the node, under the object's get.**
  `MachineCapabilityService.Usage` resolves kind and name to the machine
  as `Logs` does and asks its node over the tunnel (`node-tunnel.md`). It
  needs the object's own get, such as `compute.containers.get`, and no
  capability's permission: the numbers are what the object's status could
  say, were it allowed to. A machine not on a node, or not running, is
  FailedPrecondition, a node not connected Unavailable.

## Consequences

- The graph starts empty after a node agent restarts, and after a sample
  finds the machine not running, as once it is stopped. A restart by its
  restart policy between two samples keeps its points: the first sample of
  the new boot has no CPU, and the minute it falls in counts the rest.
- A VM's memory graph shows what the host holds for it, which a guest that
  fills its page cache brings close to its whole memory; it says nothing of
  what the guest's processes use.
- The machine list, the CLI and anything beyond an hour are later work, and
  would need storage of their own.
