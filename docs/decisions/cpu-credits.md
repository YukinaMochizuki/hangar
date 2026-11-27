# Burstable machines run on CPU credits, held to a baseline once they are spent

Status: accepted, 2026-11. Built: the API, admission, the meter and prices,
the node's ledger and limit (`node-agent.md`, "CPU credits"), the
machine's usage, burstable machine sizes (`machine-sizes.md`), and the
console (`console-design.md`, "Burstable machines").

## Context

A machine's vCPUs are shared, never reserved (`machine-model.md`,
`node-agent.md`, "CPU shares"), and a tenant paying as it goes is billed
its machines' vCPUs while they run, busy or idle (`billing.md`). Most
machines idle most of the time. A tenant with a FIXED account can share
its plan's CPUs as a pool (`cpu-pool.md`); a PAY_AS_YOU_GO one has no
pool, and nothing let it pay less for a machine that is mostly idle.

AWS's T-series in standard mode is the model: a machine has a baseline
share of each vCPU, earns credits while it runs below it, spends them to
run above it, and once they are spent is held to its baseline.

## Decision

- **A machine may be burstable, at a baseline of 20 or 40 percent of each
  vCPU.** `resources.cpu_baseline` on a Container or VirtualMachine is 20,
  40 or 100; unset is 100, a machine of all its vCPUs as before. 10 was
  measured and dropped: an idle guest's own overhead on the host ate
  about 15% of its credits, and it paused for more than 20 ms.
- **Only a tenant paying as it goes has burstable machines.** Below 100 is
  refused with FailedPrecondition unless the tenant's `BillingAccount` is
  PAY_AS_YOU_GO, whoever writes it. A write that leaves a baseline as it
  was is not judged again, as `memory_reserved` is not (`memory-pool.md`),
  so a manifest applied again passes after the tenant has left
  PAY_AS_YOU_GO, and its machines keep their baselines until changed. A
  PAY_AS_YOU_GO tenant has no CPU pool, so a burstable machine is in none
  when it is made; one whose tenant later gets a pool is held to the
  tighter of its own limit and its pool's, which nest. A FileShare's
  server is never burstable.
- **The baseline is a limit only.** The scheduler reserves no CPU for it,
  as it reserves none for any machine. Under contention a held machine
  gets the lesser of its baseline and its weighted share, as every
  machine gets the lesser of its vCPUs and its share.
- **The quota counts all its vCPUs**, as for any machine.
- **The Machine carries its baseline**, `spec.resources.cpu_baseline`, only
  when below 100. Changing it restarts nothing: the node holds the VM's
  unit to it live.
- **Credits are CPU-minutes**: one vCPU fully busy for one minute is one.
  - A running burstable machine earns vCPUs × baseline each minute and
    spends what its unit used: the unit's `cpu.stat` `usage_usec`, as the
    host counts it, its VMM's work included, not adjusted. An idle 2-vCPU
    guest costs about 0.03 CPUs that way, about twice what the guest
    itself counts.
  - The balance is held between zero and a day of what it earns,
    vCPUs × baseline × 1440.
  - The first time the node holds a balance for a machine, it gets 30
    minutes of what it earns as launch credits, if the apiserver granted
    them, and none otherwise (below).
  - A resize, of vCPUs or of baseline, keeps the balance, clipped to the
    new limit.
- **A tenant gets at most 10 launch grants in any 24 hours.** Past that,
  a new burstable machine starts with no credits, held to its baseline
  until it earns some, so deleting and making machines again farms
  nothing. Every grant follows that one rule. The apiserver decides as
  its owner's controller first writes the machine burstable, a tier
  change from 100 included, and again as the machine is asked to run
  with its balance last accounted more than 7 days before and after its
  decision, so a machine stopped for a week takes a new grant, or none
  once its tenant's are spent. It writes the decision, granted or not and
  when, into `Machine.spec.cpu_launch`, and keeps it between; the node
  only follows it, giving launch credits to a balance gone past 7 days
  only for a grant decided after it was last accounted. Each grant is
  recorded in the tenant's status, `cpu_launch_grants`, by the uid of the Container
  or VirtualMachine and the time, for 24 hours: deleting a machine gives
  none back, and a decision made again for the same object, as after its
  machine's write failed, finds its grant and takes no other. Decisions
  are made one at a time in the apiserver. The limit is a constant of
  the apiserver (`launchGrantsPerDay`), not a Plan's.
- **Its node holds it to its baseline once its credits are spent.** With
  credits, its VM's own unit has no CPU limit, and it may use all its
  vCPUs; with none, its `cpu.max` is vCPUs × baseline each 20 ms, as a
  pool's slice is (`node-agent.md`, "CPU pools"). "None" is a balance
  short of one usage poll at all its vCPUs: a poll never spends more than
  is left, and a machine busy at its baseline, earning what it spends,
  stays held rather than being let go every other poll. The limit is set
  and lifted live, and applied as its VM starts, since systemd keeps it
  where a new unit or a reboot loses it. A running machine that is not
  burstable has its limit lifted whenever it is reconciled, as after its
  baseline was raised to 100 while the agent was down, which leaves it no
  ledger to lift it.
- **The node keeps the ledger; the machine's status keeps a checkpoint.**
  The node accounts each burstable machine at each usage poll, 10 s, in a
  pass of its own that reads only the units' `cpu.stat`: it never waits
  on a guest or on the API, so a slow guest or a slow write never makes
  the spans late. A span dropped anyway is counted in
  `hangar_node_cpu_credit_spans_dropped_total`, by reason, and logged at
  debug. Another loop writes `status.cpu_credits`, the balance and the
  time it is accounted to (`as_of`), every 5 minutes while the machine
  runs, at once once it stops, and for every machine as the agent stops
  cleanly. An agent that starts reads the checkpoint back. Credits
  outlive a stop by 7 days: a balance last accounted longer ago than that
  gives way to launch credits again if the apiserver granted them anew
  (above), or to none.
- **No span of time is counted twice, and none is counted unseen.**
  - A span is accounted only from one read of the unit's CPU time to the
    next, in the same boot, at most two polls apart, and only past the
    `as_of` already accounted. A gap, as while the agent was down, its
    first read after reading a checkpoint back, or a VM's new boot, is
    charged nothing and earns nothing.
  - The agent reads a checkpoint back only for a machine it holds no
    ledger for, once in its life; a ledger outlives the machine's stops,
    so a stale status is never applied over a newer balance.
  - What an agent that crashes loses is the accounting since the last
    checkpoint written: up to the checkpoint interval, 5 minutes, each
    time it restarts. It is more when writes have been failing, as on
    repeated conflicts, since the last one that landed, and it adds up
    over repeated crashes. The error goes both ways: what was spent in it
    is given back, in the tenant's favour, and what was earned in it is
    lost, against it. A clean stop writes every balance, and loses
    nothing unless those writes fail within their 10 s.
  - Launch credits are not spent until the checkpoint holding them is
    written: until then the machine is held to its baseline. An agent
    that crashes before writing them gives them again, but none of the
    first were spent.
  - Each checkpoint replaces the one before, conditional on the Machine's
    resource version, so a write from an older read of the Machine fails
    rather than putting back an older balance.
- **Billing stays held, not used.** A running burstable machine's vCPUs
  are billed at its baseline's price, `Prices.cpu_burstable_20` or
  `cpu_burstable_40`, per vCPU-hour, in place of `cpu`. Left empty, the
  price is `cpu`'s times the baseline, not free, so a price list written
  before burstable machines never gives their vCPUs away. What it uses
  past its baseline costs nothing more: there is no unlimited mode.
- **Its usage says its credits.** `MachineCapabilityService.Usage` answers,
  for a burstable machine, its baseline, its balance, its limit and
  whether its node holds it to its baseline now, from its node's ledger.

## Consequences

- A burstable machine's first boot is held to its baseline until its
  launch credits are written: one usage poll when the write lands at
  once, and for as long as it fails, as while the API is unreachable;
  each poll tries it again.
- A held guest sees the limit as steal time, and pauses for up to the
  rest of each 20 ms period once it has used its quota: about 12 to 19 ms
  at the 99th percentile at 20%, 4 ms at 40%. A bursty I/O workload whose
  mean is below its baseline can still be held back within a period.
- Disk work by ZFS's own threads is not in the unit and is not charged.
- A machine started within moments of its balance's 7 days, which the
  apiserver judges not yet past them and its node past them, starts with
  no credits: the node gives launch credits only for a decision made
  after the balance it replaces.
- A machine whose node is down earns nothing and spends nothing until it
  is back.
