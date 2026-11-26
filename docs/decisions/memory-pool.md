# A tenant's machines may share its memory as a pool

Status: accepted, 2026-11. The apiserver's part is built; the node's
squeeze, described below, is not yet.

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
- **Each machine has a guarantee and a memory.** `resources.memory` of a
  Container or a VirtualMachine is what the guest can grow to;
  `resources.memory_guaranteed` is what it is never squeezed below. Empty
  is its memory, so every machine made before this is guaranteed all of
  it. A guarantee is a whole number of 256Mi, at least the kind's least
  memory and at most its memory, for everyone; below its memory only for a
  tenant whose plan's percent is above 100 and whose `BillingAccount` is
  not PAY_AS_YOU_GO (none, or FIXED), whoever writes it, as it is about the
  tenant and not the caller. A write that leaves a guarantee below memory
  as it was is not judged again, so a manifest applied again passes after
  the tenant's terms change. The shape rules keep reading `memory`.
- **The quota counts both.** `limits.memory` holds the sum of the
  guarantees of machines asked to run, and `memory_limit_percent` of it
  their memory: two checks, each refusing a write that raises its sum past
  its limit, with `ResourceExhausted` naming it. A tenant's and a
  project's `status.usage.memory` is the first sum and `memory_ceiling`
  the second. A guarantee counts only while its tenant has a pool (percent
  above 100, account not PAY_AS_YOU_GO), the predicate the controllers
  stamp `memory_pool` by; a tenant without one counts its machines' memory
  in full, whatever guarantees they keep. At 100 with no guarantee
  anywhere both are what memory counted before.
- **The Machine carries its pool.** `Machine.spec.memory_pool` names the
  tenant and gives its size, the Plan's `limits.memory`, set by the
  container, VM and FileShare controllers for the machines of a pooled
  tenant and absent otherwise; a Machine carries `memory_guaranteed` only
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
  pool's size; a machine in no pool holds its memory, as before. Placing,
  starting or growing a pooled machine needs only what it raises its
  pool's hold on that node by, and a machine changing pools, as when its
  tenant's plan changes, is held to the node's room as a resize is: one
  that does not fit keeps its pool and says `ResizeDoesNotFit`. A pool
  split across nodes holds up to its size on each, more than it can use:
  safe, not exact.
- **What is billed for a pooled machine is its guarantee.** The meter
  counts a running machine at its guarantee while its tenant has a pool,
  by the same predicate as the quota rather than by the machine's
  `memory_pool`, which may wait for room on its node, and at its memory
  otherwise, so the quota and the meter never disagree (`billing.md`). A
  PAY_AS_YOU_GO tenant has no pool, so its bills are as before; a FIXED
  tenant pays its plan's fee whatever it holds.
- **The node squeezes a pool's machines, never another tenant's.** This is
  the node agent's part, and not built yet: the units of a pool's machines
  run in a slice of their own whose `MemoryMax` is the pool's size plus
  their overheads, a backstop only. When the pool's actual use (the units'
  `memory.current`) passes about 90% of its size, the node inflates the
  balloon of its machines in proportion to how far each is above its
  guarantee, first only down to what its guest uses plus a margin, read
  from its balloon statistics, then, if still over, down to its guarantee,
  where the guest may kill a process of its own; it deflates them under
  about 80%. As the backstop, a pooled VM's unit kills as a whole
  (`memory.oom.group`), a machine with a guarantee below its memory before
  one without, and the node reports it on the machine. A machine's CPU
  weight follows its guarantee rather than its memory, so a pool of 200%
  does not double its tenant's share of the CPUs.

## Consequences

- The host is never overcommitted: what the scheduler reserves for a
  tenant's machines is at most its plan's memory per node, and only that
  tenant's machines are squeezed to keep within it.
- A tenant whose plan is lowered to 100, or who moves to PAY_AS_YOU_GO,
  keeps the guarantees it set until it changes them, but from then on its
  machines count, and are billed, their memory in full, which may put it
  over its memory limit: as over a lowered limit, it can still shrink and
  change anything else (`tenancy-and-iam.md`). Its machines leave the
  pool as their node's room allows.
- A FileShare's server is in its tenant's pool like any of its machines,
  though it counts toward no quota.
- Until the node squeezes, a pooled machine's memory is held by the
  scheduler as above but nothing on its node keeps the pool's machines
  within its size.
