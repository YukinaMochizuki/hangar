# A tenant's machines may share its CPUs as a pool

Status: accepted, 2026-11. Built: the apiserver's part, the node's
(`node-agent.md`, "CPU pools"), how often a pool slowed its machines, and
the console's showing it.

## Context

A machine's vCPUs are shared, never reserved: the scheduler holds a
machine's memory and disk, not its CPUs, and under load each VM's share
follows its memory (`node-agent.md`, "CPU shares"). A Plan's `limits.cpu`
caps the vCPUs of the tenant's machines asked to run, which is also the
most CPU they can use together, as no VM uses more than its vCPUs. Most
machines are idle most of the time, so a tenant that buys four CPUs and
wants six small machines of one vCPU each has to buy six.

The memory pool (`memory-pool.md`) lets a tenant's machines' memory add up
past what it bought, within one pool of its own. CPU is the simpler half:
a CPU held back only slows a guest, it never kills a process.

## Decision

- **A tenant's machines may share its CPUs as one pool, never another
  tenant's.** A Plan's `limits.cpu_limit_percent`, from 100 to 400 and
  only with `limits.cpu`, says how far the vCPUs of the tenant's machines
  asked to run may add up, as a percent of `limits.cpu`. Above 100, and
  for a tenant whose `BillingAccount` is not PAY_AS_YOU_GO (none, or
  FIXED), the tenant's machines share `limits.cpu` CPUs as a pool. Unset
  is 100, which is no pool: everything is as before. It is its own
  percent, apart from `memory_limit_percent`, so either pool may be had
  without the other. A project may not set it; its own `cpu` limit holds
  its vCPUs with its tenant's percent.
- **The pool is a hard limit.** On each node, the tenant's machines use at
  most `limits.cpu` CPUs together, whether or not the node's other CPUs
  are idle: that is what the tenant bought. A tenant at 100 is held only
  by its machines' vCPUs, which add up to no more than `limits.cpu`, so it
  loses nothing. Within the pool a machine has no CPU of its own
  reserved: while the host has CPUs to spare, the pool's CPUs go to its
  busy vCPUs alike, whatever their machines' weights (`node-agent.md`,
  "CPU pools"). The limit is the baseline a later burst, earned while
  idle, would lift.
- **The quota counts vCPUs, as before.** A pooled tenant's vCPUs of
  machines asked to run may come to `limits.cpu` times its percent;
  `ResourceExhausted` names the percent. `status.usage.cpu` is their sum,
  which may read past the plan's `cpu`. A tenant paying as it goes has no
  pool, so `limits.cpu` holds it as before, and what it runs past it, as
  after its account changed, stays as over any lowered limit.
- **The Machine carries its pool.** `Machine.spec.cpu_pool` names the
  tenant and gives the Plan's `limits.cpu`, set by the container and VM
  controllers for the machines of a pooled tenant, absent otherwise, and
  never on a FileShare's server, which serves every client of its share.
  It follows the Plan, the tenant's `spec.plan` and its `BillingAccount`
  changing, as `memory_pool` does, and changing it restarts nothing.
- **Billing is unchanged.** A FIXED tenant pays its plan's fee whatever it
  holds, and a PAY_AS_YOU_GO one has no pool, so the meter still counts a
  running machine's vCPUs (`billing.md`). The scheduler is unchanged too:
  it holds no CPU.
- **The node holds a pool's slice to its CPUs** (`node-agent.md`, "CPU
  pools"). A CPU-pooled machine's VM runs in its tenant's pool's slice,
  the one its memory pool uses when it has one, whose `cpu.max` is the
  pool's CPUs each period. The limit is set as a VM of the pool starts,
  and again from the machines' specs as they change, while they run.
- **How often the pool slowed its machines is read, not stored.** The
  console says it as "slowed 12% of the last hour", on the tenant's
  vCPU quota and on a pooled machine's page. Every 10 seconds, beside the
  machines' usage (`machine-usage.md`), the node reads each pool slice's
  `cpu.stat`, `nr_periods` and `nr_throttled`, and keeps per pool one
  point a minute of how many periods passed and how many of them the
  limit cut short, at most 60, in memory alone. A sample more than two
  polls after the one before, or with a counter gone back, as when the
  slice was made again, counts nothing, so a gap is minutes with no
  point. The slowed share is the throttled periods over all the periods
  of the points kept, and has no value with no period, as for a pool
  with no limit or none sampled yet. Not `throttled_usec`: it adds up
  the wait of each CPU, so it reads past the time passed.
  `MachineCapabilityService.Usage` answers a pooled machine's pool's
  share on its node and the CPUs the node holds the pool to, under the
  object's get. `TenantCapabilityService.CPUPoolUsage` answers the
  tenant's, under the tenant's get, from each connected node its pool's
  running machines are on: the largest share and CPUs of them, as the
  pool is held on each node apart, and how many nodes it could not read.
  It is a service of its own, not an RPC of `TenantService`, whose
  methods are only the generated standard ones (`api-rpc-shape.md`). A
  tenant with no CPU pool answers nothing, and asks no node.

## Consequences

- A pooled tenant's busy machines wait for each other: with all of them
  busy, each busy vCPU gets its part of the pool's CPUs, whatever the node
  has idle. Within a period of 20 ms, once the pool's CPU time is used,
  all its VMs wait for the next one, which a guest sees as a pause of up
  to the rest of the period: 12 ms at the 99th percentile, measured with
  twice the demand the pool may run.
- A pool split across nodes is held to its CPUs on each: more than the
  tenant bought, never less. With one node it is exact.
- A VM running when its tenant's pool begins joins the pool's slice only
  when it next starts; until then its CPUs are its own, as before.
