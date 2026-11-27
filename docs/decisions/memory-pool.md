# A tenant's machines may share its memory as a pool

Status: accepted, 2026-11. Built: the apiserver's part, the node's
(`node-agent.md`, "Memory pools"), and the console's, the Overviews'
pool card among it (`console-design.md`, "Memory pools").

## Context

Hangar reserves every machine's whole memory on its node and never
overcommits it (`machine-sizes.md`, `machine-model.md`). Guests use about
half of what they are given, measured on the host, so most of what is
reserved sits idle, and its price is paid for memory no one uses.

Overcommitting across tenants would let one tenant's load squeeze
another's machines. Merging identical pages (KSM) was the other way to
fit more, and it was rejected: it cannot be limited to one tenant, and
pages merged across tenants open a side channel between them (page
deduplication timing, Flip Feng Shui with Rowhammer), for a gain of a few
GiB nobody measured.

## Decision

- **A tenant's machines may share its memory as one pool, never another
  tenant's.** A Plan's `limits.memory_limit_percent`, from 100 to 400 and
  only with `limits.memory`, says how far the memory of the tenant's
  machines asked to run may add up, as a percent of `limits.memory`. Above
  100, the tenant's machines share its memory as a pool. Unset is 100,
  which is no pool: everything is as before. A project may not set it;
  its own memory limit holds both sums below with its tenant's percent.
- **Each machine has a reservation and a memory.** `resources.memory` of a
  Container or a VirtualMachine is what the guest can grow to;
  `resources.memory_reserved` is what it is never squeezed below:
  "reserved" is a machine's floor within its pool, the memory set aside
  for it whatever the others do. Empty is its memory, so every machine
  made before this has all of it reserved. A reservation is a whole number of 256Mi, at least the kind's least
  memory and at most its memory, for everyone; below its memory only for a
  tenant whose plan's percent is above 100 and whose `BillingAccount` is
  not PAY_AS_YOU_GO (none, or FIXED), whoever writes it, as it is about the
  tenant and not the caller. A write that leaves a reservation below memory
  as it was is not judged again, so a manifest applied again passes after
  the tenant's terms change. The shape rules keep reading `memory`.
- **The quota counts both.** `limits.memory` holds the sum of the
  reservations of machines asked to run, and `memory_limit_percent` of it
  their memory: two checks, each refusing a write that raises its sum past
  its limit, with `ResourceExhausted` naming it. A tenant's and a
  project's `status.usage.memory` is the first sum and `memory_ceiling`
  the second. A reservation counts only while its tenant has a pool (percent
  above 100, account not PAY_AS_YOU_GO), the predicate the controllers
  stamp `memory_pool` by; a tenant without one counts its machines' memory
  in full, whatever reservations they keep. At 100 with no reservation
  anywhere both are what memory counted before.
- **The Machine carries its pool.** `Machine.spec.memory_pool` names the
  tenant and gives its size, the Plan's `limits.memory`, set by the
  container and VM controllers for the machines of a pooled tenant and
  absent otherwise, and never on a FileShare's server, which counts toward
  no quota, so its memory would come out of its tenant's reservations
  unseen: the scheduler holds a server's memory on its own; a Machine carries `memory_reserved` only
  when below its memory. They follow the Plan, the tenant's `spec.plan` and
  its `BillingAccount` changing. Changing the pool restarts nothing: a
  machine is restarted when what its node runs it with changes, and the
  pool is not part of that.
- **The pool's size is both what is reserved and what may be used: R = C.**
  One `size` serves as both what a node reserves for the pool's machines
  and the most they may use there together. Keeping them as one value is
  for now: a later cross-tenant tier, where a tenant may use past what is
  reserved for it, splits them, and nothing here makes memory past the
  pool free or billed, so that stays an extension and not a migration.
- **The scheduler holds a pool once per node.** On a node, the running
  machines of one pool hold the least of their memory together and the
  pool's size, but never less than their reservations together, which a plan
  lowered while they run can leave past its size (the quota is not judged
  again for machines running); a machine in no pool holds its memory, as
  before. Placing,
  starting or growing a pooled machine needs only what it raises its
  pool's hold on that node by, and a machine changing pools, as when its
  tenant's plan changes, is held to the node's room as a resize is: one
  that does not fit keeps its pool and says `ResizeDoesNotFit`. A pool
  split across nodes holds up to its size on each, more than it can use:
  safe, not exact.
- **What is billed for a pooled machine is its reservation.** The meter
  counts a running machine at its reservation while its tenant has a pool,
  by the same predicate as the quota rather than by the machine's
  `memory_pool`, which may wait for room on its node, and at its memory
  otherwise, so the quota and the meter never disagree (`billing.md`). A
  PAY_AS_YOU_GO tenant has no pool, so its bills are as before; a FIXED
  tenant pays its plan's fee whatever it holds.
- **The node squeezes a pool's machines, never another tenant's**
  (`node-agent.md`, "Memory pools"). The units of a pool's machines run in
  a slice of their own whose `MemoryMax` is the pool's size plus their
  overheads, a backstop only. The node shares the pool's size among them,
  or their reservations together if more, as the scheduler holds it; a
  machine counts in its pool as it was started until its VM is gone. A
  machine with all its memory reserved (Dedicated) is never squeezed nor
  slowed, and its whole memory is held for it, used or not. Each machine with less reserved (Shared) is held by its
  balloon at an allowance of what that leaves, so the pool cannot be
  overrun: what it uses, read from its balloon statistics, and a margin,
  and a share of what is left, given first to those whose guests are short
  of memory; when the pool is short, down to its reservation, where the guest
  may kill a process of its own. A balloon is given back only into room
  the others' balloons have already freed. A Shared machine boots at its
  reservation; a container's guest waits for that before it runs anything of
  the container's. As the backstop, a pooled VM's unit kills as a whole (`memory.oom.group`), a
  Shared machine before a Dedicated one, and the node reports it on the
  machine as `OOMKilled`, `OutOfMemory`, whatever its restart policy. A machine's CPU weight follows its reservation
  rather than its memory, so a pool of 200% does not double its tenant's
  share of the CPUs. A machine joins a pool's slice, or leaves it, as it
  starts; its reservation's effects follow while it runs.
- **Who uses a pool now is read from its nodes, never stored.**
  `TenantCapabilityService.MemoryPoolUsage` asks each connected node the
  tenant's running pooled machines are on what its squeeze last read
  (`node-agent.md`), as `CPUPoolUsage` does for a CPU pool, and answers the
  pool's size and each machine: its memory, its reservation, what its guest
  has now (its memory less its balloon, unset when its node cannot tell),
  what its guest uses of that (its total less its available memory, from
  its balloon statistics, unset when it reports none or not lately), its
  last OOM kill and since when its balloon has not followed. What a guest
  has is not what it needs: the squeeze hands the pool's spare room to its
  Shared guests, up to their limits, so whenever their limits add up past
  the pool what they have fills it. What they use says how much of the
  pool is wanted, which is what the console draws (owner's decision,
  2026-11-27). It needs the get of Containers or VirtualMachines in some
  project of the tenant, not the Tenant's, which no project role grants:
  the project Overview shows the pool to a member of that project alone. A
  machine whose object the caller may not get is summed into one `other`
  total (memory, reservation, now, in use), with no name, kind or count.
  Such a caller learns the pool's size, which is the plan's memory, and
  those totals of the rest of the tenant; that was accepted for a project
  member, since the pool is what its own machines share. `CPUPoolUsage`
  keeps the Tenant's get.

## Consequences

- The host is never overcommitted: what the scheduler reserves for a
  tenant's machines is at most its plan's memory per node, and only that
  tenant's machines are squeezed to keep within it.
- A tenant whose plan is lowered to 100, or who moves to PAY_AS_YOU_GO,
  keeps the reservations it set until it changes them, but from then on its
  machines count, and are billed, their memory in full, which may put it
  over its memory limit: as over a lowered limit, it can still shrink and
  change anything else (`tenancy-and-iam.md`). Its machines leave the
  pool as their node's room allows.
- A FileShare's server is in no pool: its node holds its memory whole,
  as before pools.
- A Shared machine's guest sees its memory move as its tenant's other
  machines use the pool: past its reservation, none of it is sure, page
  cache or not. A Shared container starts some seconds later than it
  would (3.9 to 5.7 s measured), its guest waiting for its boot balloon.
- A Shared firmware VM has a boot window: its operating system does not
  wait for its boot balloon, so for the few seconds it takes (4.2 s
  measured, `node-agent.md`), a fill fast enough can reach the pool's
  slice's limit, where the backstop kills a Shared VM whole, by score.
