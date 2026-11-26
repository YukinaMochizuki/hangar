# Billing: metered by the minute, rated by a price list, billed per period, paid outside Hangar

Status: accepted, 2026-11.

## Context

Tenants pay in one of two ways. Some pay a fixed fee each month, whatever
they hold. Others pay as they go: what they hold, by the minute, billed once
every period agreed with them, from one to six months. Hangar collects no
money itself; the owner is paid outside it and needs to know what each tenant
owes.

Nothing measured what a tenant held over time. A tenant's quota usage
(`tenancy-and-iam.md`, "Quotas") is what it holds now. A machine's CPU and
memory samples (`machine-usage.md`) live in its node's memory for an hour.
Prometheus may carry no tenant in a label (`monitoring.md`), and the store's
events are compacted (`sqlite-store.md`).

## Decision

- **What is billed is what is held, not what is used.** A running machine
  is billed its vCPUs and memory, as asked for, whatever its load: memory is
  reserved in full and never overcommitted (`machine-sizes.md`), so an idle
  machine costs what a busy one does. While its tenant has a memory pool,
  a machine is billed its reservation, `resources.memory_reserved`, which
  is what the quota counts as its memory; its memory past that is a
  ceiling, never billed (`memory-pool.md`). A tenant without a pool, every
  PAY_AS_YOU_GO one among them, is billed its machines' memory, which the
  quota then counts too. Disks, addresses, public endpoints,
  port forwards, file shares, network endpoints and custom domains are
  billed while they exist. Each object counts as the quota counts it
  (`quotaCounters` in `internal/apiserver/quota.go`), but for cpu and
  memory, which count only while the object's Machine is running
  (`MACHINE_PHASE_RUNNING`), as its node reports it, not while its spec asks
  it to run: a machine that cannot start is not billed.
- **The apiserver meters every minute, into a table of its own.** For each
  tenant with a `BillingAccount`, the apiserver records, for each minute,
  each billable object and what it held then, in the store's meter
  (`store.Meter`), a table outside objects and events, as credentials are.
  It reads a minute once, at its first tick in it, within 20 seconds of
  the minute's start; a reading recorded again, after a restart, replaces the
  minute's rows. A minute in which the apiserver did not run is not
  recorded, and so not billed. Once a period is billed, the whole hours
  inside it are folded into a row an hour, a sixtieth of the rows; the
  minutes of an hour the period only partly holds stay minutes, so a
  period read again reads the same.
- **Prices are data, in a `PriceList`, and never change what was used.** A
  price list is a currency and a list of entries, each a set of prices per
  unit-hour from its effective time on. Admission refuses a change to, or
  the removal of, an entry whose time has come: a new price is a new entry.
  A minute is rated at the entry in effect in it. Amounts are kept as whole
  micro-units of the currency, and only a Statement's total is rounded, to
  the currency's minor unit.
- **A `BillingAccount` is one tenant's agreement.** Its name is the
  tenant's. It says the type (`FIXED` or `PAY_AS_YOU_GO`), the currency,
  the price list, the months a period lasts (always 1 for `FIXED`, 1 to 6
  otherwise) and the anchor the periods are cut from, in a time zone of its
  own. Periods are whole months from the anchor: each starts on the
  anchor's day of the month, or the month's last day if it has fewer, and
  none is prorated. A new account may not start before its tenant's last
  Statement ends, so a period is never billed twice.
- **A fixed fee is the tenant's `Plan`'s.** A Plan, which sets a tenant's
  limits, may also have a price: a monthly fee and its currency. A FIXED
  account pays its tenant's Plan's fee, so a plan of no fee gives the
  platform's own tenants, or a trial, an account like any other. Admission
  keeps a FIXED tenant on a priced plan in its currency, and keeps that
  price on its plan. A FIXED account may name a price list too; what its
  holdings would cost then is answered only to whoever reads billing at
  the platform, and cleared from what its tenant's people read: they pay
  the fee whatever they hold.
- **Two changes end the period at once; the rest wait for the next one.**
  A change of type, or of a FIXED tenant's Plan, as a trial becoming a
  paying tenant, ends the open period in the minute the apiserver sees it,
  which it records in the account's status before billing anything, so a
  repeated reconcile bills the same minute. Its Statement bills the part
  that passed, a fixed fee in its share of the period, and the next
  periods are cut from that minute. A new price of the Plan, a new price
  list or a new length applies from the next period, whose start does not
  move; a new length keeps the anchor's day of the month. Until the first
  period starts, the account is simply as its spec is.
- **A `Statement` is made as a period ends, and only its payment changes.**
  It holds one line per thing billed and price in effect, the total, and
  the uid of the account that made it. Its name is the tenant's and its
  period's start, in the account's zone; a name another account of the
  tenant made already is followed by the account's uid. The apiserver alone
  writes it (`systemManaged`). It is not owned by its tenant, so it
  outlives it: a deleted tenant may still owe. The account carries a
  finalizer, so deleting it, alone or with its tenant, first bills the
  period it had open, up to the deletion.
- **A promotion is a discount until a time, shown against the price.** An
  account's `promotions` each take a percent off what is billed for the
  time before their end, such as a trial's 100% until it ends. A Statement
  lists every item at its price and each promotion on a line of its own
  with a negative amount, so a tenant sees what it would pay and what it
  was spared; together they never take more than the period came to. They
  are read as a period is billed, so a change reaches every period not yet
  billed, and the open period's status says what they take off.
- **Payment is recorded, not collected.** `StatementPaymentService` records
  a Statement paid, or takes that back, under `billing.statements.pay`,
  which only `platform.root` holds.
- **Only platform administrators write billing; a tenant reads its own.**
  The three kinds are the platform's, as `Plan` is: no tenant role writes
  them, as `tenant.owner` would write every kind at its tenant. A
  `BillingAccount` and a `Statement` are also read by whoever holds the
  kind's get, list or watch at the tenant they name: its owners and
  viewers, not its projects' people. Price lists are read by everyone.
- **The open period is shown as it goes.** The account's status holds the
  open period and what it has come to so far, updated every few minutes.

## Consequences

- An apiserver that is down bills nobody for the time it is down. That
  errs towards the tenant.
- A tenant's own quota usage and its metered holdings come from the same
  counting, so they cannot drift apart. The meter adds one read of each
  billed tenant's objects a minute.
- Stopping a machine stops its cpu and memory being billed once its node
  reports it stopped; its disks are still billed.
- Taxes, invoices sent by mail, payment through a provider, and pricing by
  measured load are not part of this.
