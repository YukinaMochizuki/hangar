# Billing a tenant

How the platform administrator sets up what a tenant pays, reads what it
owes, and records a payment. Why it works this way is in
`docs/decisions/billing.md`. Everything here takes `platform.root`.

## Set the prices

A pay-as-you-go tenant is rated by a `PriceList`. Prices are per unit-hour,
in the list's currency, at most six digits after the point; a field left
out is free.

```yaml
apiVersion: billing.hangarlab.cloud/v1alpha1
kind: PriceList
metadata: {name: standard}
spec:
  displayName: Standard
  currency: TWD
  entries:
    - effectiveTime: "2026-12-01T00:00:00+08:00"
      prices: {cpu: "0.6", memory: "0.2", disk: "0.005", addresses: "0.05"}
```

```sh
hangar apply -f standard.yaml
```

To change a price, add an entry at the end, taking effect now or later.
An entry whose time has come cannot be changed or removed; the apply is
refused and says so. Every minute is rated at the entry in effect in it.

## Price the plans of fixed-fee tenants

A FIXED account pays its tenant's Plan's price. Give each plan fixed-fee
tenants are on a price; a fee of "0" is a plan for the platform's own
tenants, or a trial:

```yaml
apiVersion: tenancy.hangarlab.cloud/v1alpha1
kind: Plan
metadata: {name: standard}
spec:
  limits: {cpu: "8", memory: 16Gi, disk: 200Gi}
  price: {monthlyFee: "300", currency: TWD}
```

A tenant is put on a plan by root, with `spec.plan` on the Tenant. While
its account is FIXED, it can only be moved to a priced plan in the
account's currency, and its plan's price cannot be taken away.

## Open a tenant's account

The account is named after the tenant.

```yaml
apiVersion: billing.hangarlab.cloud/v1alpha1
kind: BillingAccount
metadata: {name: acme}
spec:
  type: BILLING_TYPE_PAY_AS_YOU_GO   # or BILLING_TYPE_FIXED, which pays the tenant's plan's price
  currency: TWD
  priceList: standard                # optional on a FIXED account: what it would have cost, for root alone
  periodMonths: 3                    # 1 to 6; a FIXED account is always 1
  anchorTime: "2026-12-01T00:00:00+08:00"
  timeZone: Asia/Taipei              # periods are cut on the anchor's day and time here
```

`currency`, `anchorTime` and `timeZone` cannot change. Metering starts at
the anchor. A tenant without an account is neither metered nor billed. A
new account starts no earlier than the tenant's last Statement ends.

## Change what a tenant pays

| Change | When it applies |
|---|---|
| `type`, FIXED to PAY_AS_YOU_GO or back | At once: a Statement bills the part of the period that passed (a fixed fee in its share), and the next periods are cut from that minute |
| A FIXED tenant's `spec.plan`, as a trial becoming paid | At once, as above |
| The plan's `price` | From the tenant's next period |
| `priceList`, `periodMonths` | From the next period |

## Give a promotion

A promotion takes a percent off what an account is billed until a time; a
trial is 100% until it ends. Statements list the price and the promotion
on its own line:

```yaml
spec:
  # the rest of the account as it is
  promotions:
    - {label: Early access, percent: 100, untilTime: "2027-01-01T00:00:00+08:00"}
```

Apply the account with it. A promotion changed or removed reaches every
period not billed yet, the open one included; a billed statement keeps
what it said.

## Read what is owed

```sh
hangar get ba acme -o yaml        # status: the open period and what it has come to
hangar get statements             # every Statement; the tenant's owners see their own
hangar get stmt acme-202612010000 -o yaml
```

A Statement is made within a minute of its period's end, named after the
tenant and the period's start in the account's time zone. Its lines give
each thing billed, per price in effect, in unit-hours; its total is rounded
to the currency's minor unit. Nothing changes it after but its payment.

## Record a payment

Hangar collects nothing; record what was paid outside it.

```sh
hangar admin pay acme-202612010000                                  # paid now
hangar admin pay acme-202612010000 --at 2026-12-03T10:00:00+08:00   # paid then
hangar admin pay acme-202612010000 --undo                           # not paid after all
```

## Close an account

Deleting the account, or its tenant, bills the period it had open first,
up to the deletion. Statements are kept after their tenant is gone.

## Traps

- **The apiserver down is a gap in the meter.** The minutes it did not run
  are not billed; the periods that ended meanwhile are each billed once it
  is back.
- **A price list an account names cannot be deleted**, nor one its open
  period is rated by. Name another list on the account; the old one can be
  deleted once the account's next period starts.
