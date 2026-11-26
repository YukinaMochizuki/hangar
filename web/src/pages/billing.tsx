// Billing: a tenant's own page (what the period open has come to, what it
// is made of, what is owed, and its statements), a statement's page (its
// lines by item, and whether it is paid), and the platform's page for its
// administrators (what every tenant owes and has come to, and the price
// lists). Hangar collects nothing: a payment is recorded by root
// (docs/decisions/billing.md).

import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";
import { BillingType } from "@hangar/api/hangar/billing/v1alpha1/billing_account_pb";
import type { StatementLine } from "@hangar/api/hangar/billing/v1alpha1/line_pb";
import { EquivalentCard } from "@/components/object";
import { PaymentAction } from "@/components/payment-dialog";
import { DetailBody, DetailFrame, DetailHead, LoadingTable, PageHead } from "@/components/page";
import { Topbar, useTenantName } from "@/components/shell";
import { EmptyState, ErrorNotice, Skeleton, SkeletonRows } from "@/components/states";
import { tbodyRow, Th, theadRow } from "@/components/table";
import { Card, CardHeader, Field } from "@/components/ui";
import { cn } from "@/lib/cn";
import {
  agreementOf,
  date,
  endLabels,
  entriesAt,
  formatMoment,
  formatMoney,
  formatPeriod,
  formatQuantity,
  groupLines,
  itemLabels,
  itemUnits,
  listAccountsPermission,
  outstanding,
  payPermission,
  periodShare,
  pricesOf,
  projected,
  typeLabel,
  useBillingAccount,
  useBillingAccounts,
  usePriceList,
  usePriceLists,
  useStatement,
  useStatements,
  type BillingAccount,
  type LineGroup,
  type Statement,
} from "@/lib/billing";
import { usePlatformCan } from "@/lib/permissions";

/** Shades of the foreground an item takes in a breakdown, largest first: states and the accent keep their colours. */
const shades = ["bg-foreground/85", "bg-foreground/55", "bg-foreground/35", "bg-foreground/20", "bg-foreground/12", "bg-foreground/8"];

/** Paid, or due, as a pill. */
function PaidPill({ s, zone, withDate }: { s: Statement; zone?: string; withDate?: boolean }) {
  const paid = date(s.status?.paidTime);
  return paid ? (
    <span className="inline-flex items-center gap-1.5 whitespace-nowrap rounded-full bg-ok/12 px-2 py-0.5 text-[11.5px] font-medium text-ok">
      <span className="size-1.5 rounded-full bg-ok" />
      Paid{withDate && <span className="font-normal opacity-80"> {formatMoment(paid, zone)}</span>}
    </span>
  ) : (
    <span className="inline-flex items-center gap-1.5 whitespace-nowrap rounded-full bg-pending/15 px-2 py-0.5 text-[11.5px] font-medium text-pending">
      <span className="size-1.5 rounded-full bg-pending" />
      Due
    </span>
  );
}

/** One figure the page leads with: what it is, the amount, and a line under it. */
function Stat({ label, value, sub, tone, loading, children }: { label: string; value: ReactNode; sub?: ReactNode; tone?: "pending" | "ok"; loading?: boolean; children?: ReactNode }) {
  return (
    <Card className="flex flex-col px-5 py-4">
      <div className="flex items-center gap-2 text-[12px] text-muted">
        {tone && <span className={cn("size-1.5 rounded-full", tone === "pending" ? "bg-pending" : "bg-ok")} />}
        {label}
      </div>
      {loading ? (
        <Skeleton className="mt-2.5 h-7 w-36" />
      ) : (
        <div className="mt-1.5 font-mono text-[26px] font-semibold leading-tight tracking-tight tabular">{value}</div>
      )}
      {sub && <div className="mt-1 text-[12px] text-muted">{sub}</div>}
      {children}
    </Card>
  );
}

/** How far through its period a moment is: the way covered solid, the way ahead a track. */
function PeriodTrack({ share }: { share: number }) {
  return (
    <div className="mt-3 h-1.5 overflow-hidden rounded-full bg-surface-2" aria-label={`${Math.round(share * 100)}% of the period passed`}>
      <div className="h-full rounded-full bg-foreground/70" style={{ width: `${share * 100}%` }} />
    </div>
  );
}

/** What an amount is made of: one bar of its items, then a row each with its share. */
function Breakdown({ groups, currency }: { groups: LineGroup[]; currency?: string }) {
  const total = groups.reduce((n, g) => n + g.amount, 0);
  if (total <= 0) return <p className="px-5 pb-5 text-[13px] text-muted">Nothing held has come to anything yet.</p>;
  const sorted = [...groups].sort((a, b) => b.amount - a.amount);
  return (
    <div className="px-5 pb-3 pt-4">
      <div className="flex h-2.5 gap-0.5 overflow-hidden rounded-full">
        {sorted.map((g, i) => (
          <div key={g.item} className={shades[Math.min(i, shades.length - 1)]} style={{ width: `${(g.amount / total) * 100}%` }} title={itemLabels[g.item]} />
        ))}
      </div>
      <table className="mt-4 w-full text-[13px]">
        <tbody>
          {sorted.map((g, i) => (
            <tr key={g.item} className="border-b last:border-0">
              <td className="py-2.5 pr-3">
                <span className="flex items-center gap-2.5">
                  <span className={cn("size-2.5 shrink-0 rounded-sm", shades[Math.min(i, shades.length - 1)])} />
                  {itemLabels[g.item] ?? g.item}
                </span>
              </td>
              <td className="hidden py-2.5 pr-3 text-right font-mono text-[12px] text-muted tabular sm:table-cell">
                {formatQuantity(String(g.quantity))} <span className="text-subtle">{g.unit}</span>
              </td>
              <td className="w-14 py-2.5 pr-3 text-right font-mono text-[12px] text-subtle tabular">{Math.round((g.amount / total) * 100)}%</td>
              <td className="w-36 py-2.5 text-right font-mono tabular">{formatMoney(String(g.amount), currency)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/** The statements, newest first, each in its account's zone; a row opens its statement. */
function StatementsList({ statements, zoneOf, showTenant }: { statements: Statement[]; zoneOf: (tenant: string) => string | undefined; showTenant?: boolean }) {
  return (
    <table className="w-full text-[13px]">
      <thead>
        <tr className={theadRow}>
          <Th>Period</Th>
          {showTenant && <Th className="hidden sm:table-cell">Tenant</Th>}
          <Th className="hidden md:table-cell">Ended</Th>
          <Th className="text-right">Amount</Th>
          <Th className="w-28">Status</Th>
        </tr>
      </thead>
      <tbody>
        {statements.map((s) => {
          const name = s.metadata?.name ?? "";
          const t = s.spec?.tenant ?? "";
          const zone = zoneOf(t);
          return (
            <tr key={name} className={tbodyRow}>
              <td className="px-4 py-3">
                <Link to="/tenants/$tenant/billing/$statement" params={{ tenant: t, statement: name }} className="block">
                  <span className="block font-medium group-hover:text-accent">{formatPeriod(date(s.spec?.periodStartTime), date(s.spec?.periodEndTime), zone)}</span>
                  <span className="block font-mono text-[11.5px] text-subtle">{name}</span>
                </Link>
              </td>
              {showTenant && <td className="hidden px-4 py-3 font-mono text-[12px] sm:table-cell">{t}</td>}
              <td className="hidden px-4 py-3 text-muted md:table-cell">{s.spec ? endLabels[s.spec.end] : "—"}</td>
              <td className="px-4 py-3 text-right font-mono font-medium tabular">{formatMoney(s.spec?.total, s.spec?.currency)}</td>
              <td className="px-4 py-3">
                <PaidPill s={s} zone={zone} />
              </td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}

/** The prices that rate the account now, and those that take effect next. */
function PricesRows({ list: name, currency }: { list: string; currency?: string }) {
  const { list, loading, error } = usePriceList(name);
  const { current, next } = entriesAt(list, new Date());
  if (loading) return <SkeletonRows rows={3} />;
  if (error) return <ErrorNotice error={error} title="The prices did not load" className="p-0" />;
  const rows = (entry: typeof current) =>
    pricesOf(entry?.prices).map(({ item, price }) => (
      <div key={item} className="flex items-baseline justify-between gap-3 py-1.5 text-[13px]">
        <span className="text-muted">{itemLabels[item]}</span>
        <span className="font-mono text-[12.5px] tabular">
          {formatMoney(price, currency)}
          <span className="text-subtle"> / {itemUnits[item].replace("-hour", "")}·h</span>
        </span>
      </div>
    ));
  return (
    <>
      {current ? rows(current) : <p className="py-1.5 text-[13px] text-muted">No price is in effect yet.</p>}
      {next && (
        <div className="mt-2 border-t pt-2">
          <div className="py-1 text-[11.5px] text-subtle">From {formatMoment(date(next.effectiveTime), undefined)}</div>
          {rows(next)}
        </div>
      )}
    </>
  );
}

/**
 * The agreement, and, for a tenant paying as it goes, the prices that rate
 * it. A fixed-fee tenant sees its plan and fee alone: what its holdings
 * would cost at list prices is the platform's to know, not a bill.
 */
function AgreementCard({ a }: { a: BillingAccount }) {
  const spec = a.spec;
  const st = a.status;
  const fixed = (st?.periodType || spec?.type) === BillingType.FIXED;
  const list = st?.periodPriceList || spec?.priceList;
  const months = Math.max(st?.periodMonths || spec?.periodMonths || 1, 1);
  return (
    <Card>
      <CardHeader title="Agreement" />
      <dl className="divide-y px-4">
        <Field label="Type">{typeLabel(st?.periodType || spec?.type)}</Field>
        {fixed && (
          <Field label="Plan" mono>
            {st?.periodPlan || "—"}
          </Field>
        )}
        {fixed && (
          <Field label="Monthly fee" mono>
            {formatMoney(st?.periodMonthlyFee, spec?.currency)}
          </Field>
        )}
        <Field label="Billed every">{months === 1 ? "month" : `${months} months`}</Field>
        <Field label="Periods from">{formatMoment(date(st?.periodAnchorTime ?? spec?.anchorTime), spec?.timeZone)}</Field>
        <Field label="Time zone" mono>
          {spec?.timeZone || "UTC"}
        </Field>
      </dl>
      {!fixed && list && (
        <div className="border-t px-4 pb-3 pt-3">
          <div className="mb-1 flex items-baseline justify-between text-[12px]">
            <span className="font-medium">Prices</span>
            <span className="font-mono text-[11.5px] text-subtle">{list}</span>
          </div>
          <PricesRows list={list} currency={spec?.currency} />
        </div>
      )}
    </Card>
  );
}

/** The account's promotions not ended yet. */
function activePromotions(a: BillingAccount | undefined, now: Date) {
  return (a?.spec?.promotions ?? []).filter((p) => (date(p.untilTime)?.getTime() ?? 0) > now.getTime());
}

/** A day in the account's zone, as "Dec 15, 2026". */
function formatDay(d: Date | undefined, zone: string | undefined): string {
  return d ? new Intl.DateTimeFormat("en", { timeZone: zone || "UTC", year: "numeric", month: "short", day: "numeric" }).format(d) : "—";
}

function daysLeft(end: Date | undefined, now: Date): string {
  if (!end) return "";
  const days = Math.ceil((end.getTime() - now.getTime()) / 86_400_000);
  return days <= 0 ? "ends today" : days === 1 ? "1 day left" : `${days} days left`;
}

export function TenantBilling() {
  const tenant = useTenantName();
  const { account: a, loading, error, none, denied, refetch } = useBillingAccount(tenant);
  const stmts = useStatements(tenant, !denied);
  const zone = a?.spec?.timeZone;
  const currency = a?.spec?.currency;
  const st = a?.status;
  const now = new Date();
  const start = date(st?.periodStartTime);
  const end = date(st?.periodEndTime);
  const fixed = st?.periodType === BillingType.FIXED;
  const owed = outstanding(stmts.statements);
  const groups = groupLines(st?.usageLines ?? []);
  const projection = fixed ? undefined : projected(st?.accrued, start, end, date(st?.accruedTime));
  return (
    <>
      <Topbar crumbs={["Billing"]} />
      <PageHead title="Billing" description={a ? `${agreementOf(a)}, billed in ${currency}.` : undefined} />
      <div className="space-y-5 px-4 pb-10 md:px-8">
        {loading ? (
          <LoadingTable />
        ) : denied ? (
          <p className="text-[13px] text-muted">This account's roles do not include reading the tenant's billing.</p>
        ) : error ? (
          <Card>
            <ErrorNotice error={error} title="The billing account did not load" onRetry={() => void refetch()} />
          </Card>
        ) : none && stmts.statements.length === 0 ? (
          <Card>
            <EmptyState title="Not billed">
              <span className="font-mono">{tenant}</span> has no billing account: what it holds is not metered or billed.
            </EmptyState>
          </Card>
        ) : (
          <>
            {activePromotions(a, now).map((p, i) => (
              <div key={i} className="flex flex-wrap items-center gap-2 rounded-lg border border-ok/30 bg-ok/8 px-4 py-2.5 text-[13px]">
                <span className="size-1.5 rounded-full bg-ok" />
                <span className="font-medium">{p.label}</span>
                <span className="text-muted">
                  {p.percent}% off what is billed until {formatDay(date(p.untilTime), zone)}
                </span>
              </div>
            ))}
            <div className="grid grid-cols-1 gap-4 md:grid-cols-3">
              {!a || !start ? (
                <Stat label="This period" value="—" sub={a ? `Starts ${formatMoment(date(a.spec?.anchorTime), zone)}` : "The account is closed"} />
              ) : fixed ? (
                <Stat
                  label="Monthly fee"
                  value={formatMoney(st?.periodMonthlyFee, currency)}
                  sub={st?.discount ? `Plan ${st?.periodPlan} · this period ${formatMoney(st?.accrued, currency)} after promotions` : `Plan ${st?.periodPlan}`}
                />
              ) : (
                <Stat
                  label="This period so far"
                  value={formatMoney(st?.accrued, currency)}
                  sub={
                    st?.discount
                      ? `${formatMoney(String(Number(st.accrued) + Number(st.discount)), currency)} before promotions · ${daysLeft(end, now)}`
                      : `${formatPeriod(start, end, zone)} · ${daysLeft(end, now)}`
                  }
                >
                  <PeriodTrack share={periodShare(start, end, now)} />
                </Stat>
              )}
              {a && start && fixed ? (
                <Stat label="Next statement" value={<span className="font-sans text-[22px]">{formatDay(end, zone)}</span>} sub={`For ${formatPeriod(start, end, zone)} · ${daysLeft(end, now)}`}>
                  <PeriodTrack share={periodShare(start, end, now)} />
                </Stat>
              ) : (
                <Stat label="Projected for the period" value={projection === undefined ? "—" : formatMoney(projection.toFixed(2), currency)} sub={projection === undefined ? "Known once a day has passed" : "At this period's pace so far"} />
              )}
              <Stat
                label="Outstanding"
                tone={owed.count > 0 ? "pending" : "ok"}
                loading={stmts.loading}
                value={owed.count > 0 ? formatMoney(owed.total.toFixed(2), currency) : formatMoney("0", currency)}
                sub={owed.count > 0 ? `${owed.count} statement${owed.count === 1 ? "" : "s"} due` : "Every statement is paid"}
              />
            </div>
            <div className="grid grid-cols-1 gap-5 xl:grid-cols-[minmax(0,1fr)_360px]">
              <div className="min-w-0 space-y-5">
                {a && start && !fixed && groups.length > 0 && (
                  <Card>
                    <CardHeader
                      title="This period by item"
                      action={st?.accruedTime && <span className="text-[11.5px] text-subtle">as of {formatMoment(date(st.accruedTime), zone)}</span>}
                    />
                    <Breakdown groups={groups} currency={currency} />
                  </Card>
                )}
                <Card className="overflow-x-auto">
                  <CardHeader
                    title={
                      <span className="flex items-baseline gap-2">
                        Statements
                        {!stmts.loading && <span className="font-mono text-[11.5px] font-normal text-subtle">{stmts.statements.length}</span>}
                      </span>
                    }
                  />
                  {stmts.loading ? (
                    <SkeletonRows rows={3} />
                  ) : stmts.error ? (
                    <ErrorNotice error={stmts.error} title="The statements did not load" onRetry={() => void stmts.refetch()} />
                  ) : stmts.statements.length === 0 ? (
                    <p className="px-4 py-3 text-[13px] text-muted">None yet: the first is made as the period ends.</p>
                  ) : (
                    <StatementsList statements={stmts.statements} zoneOf={() => zone} />
                  )}
                </Card>
              </div>
              <div className="min-w-0 space-y-5">{a && <AgreementCard a={a} />}</div>
            </div>
          </>
        )}
      </div>
    </>
  );
}

/** One item of a statement: its total, and, when its price changed in the period, each price's part under it. */
function ItemRows({ g, currency, zone }: { g: LineGroup; currency?: string; zone?: string }) {
  const split = g.lines.length > 1;
  const part = (l: StatementLine) => (
    <span className="font-mono text-[12px] text-muted tabular">
      {formatQuantity(l.quantity)} {l.unit} × {formatMoney(l.unitPrice, currency)}
    </span>
  );
  return (
    <>
      <tr className={cn(!split && "border-b")}>
        <td className="px-5 pt-3 pb-3 align-top">
          <div className="font-medium">{itemLabels[g.item] ?? g.item}</div>
          {!split && <div className="mt-0.5">{part(g.lines[0])}</div>}
        </td>
        <td className="px-5 py-3 text-right align-top font-mono font-medium tabular">{formatMoney(String(g.amount), currency)}</td>
      </tr>
      {split &&
        g.lines.map((l, i) => (
          <tr key={i} className={cn(i === g.lines.length - 1 && "border-b")}>
            <td className="py-1 pl-9 pr-5 last:pb-3">
              {part(l)}
              {l.priceEffectiveTime && <span className="ml-2 text-[11.5px] text-subtle">from {formatMoment(date(l.priceEffectiveTime), zone)}</span>}
            </td>
            <td className={cn("px-5 py-1 text-right font-mono text-[12px] text-muted tabular", i === g.lines.length - 1 && "pb-3")}>{formatMoney(l.amount, currency)}</td>
          </tr>
        ))}
    </>
  );
}

export function StatementPage({ statement: name }: { statement: string }) {
  const tenant = useTenantName();
  const { statement: s, loading, error, missing, refetch } = useStatement(name);
  const { account } = useBillingAccount(tenant);
  const zone = account?.spec?.timeZone;
  const mayPay = usePlatformCan([payPermission]).can(payPermission);
  const currency = s?.spec?.currency;
  const groups = groupLines((s?.spec?.lines ?? []).filter((l) => l.item !== "promotion"));
  const promotions = (s?.spec?.lines ?? []).filter((l) => l.item === "promotion");
  const paid = !!s?.status?.paidTime;
  return (
    <DetailFrame
      crumbs={[
        <Link key="b" to="/tenants/$tenant/billing" params={{ tenant }}>
          Billing
        </Link>,
        name,
      ]}
      state={{ loading, error, refetch: () => void refetch() }}
      found={!missing && !!s}
      noun="statement"
      name={name}
      where="This tenant"
    >
      {s && (
        <>
          <DetailHead
            name={formatPeriod(date(s.spec?.periodStartTime), date(s.spec?.periodEndTime), zone)}
            kind="Statement"
            facts={
              <>
                <PaidPill s={s} zone={zone} withDate />
                <span>{typeLabel(s.spec?.type)}</span>
                <span>{s.spec ? endLabels[s.spec.end] : ""}</span>
                <span className="font-mono">{name}</span>
              </>
            }
            actions={mayPay && <PaymentAction s={s} />}
          />
          <DetailBody
            main={
              <Card className="overflow-hidden">
                <div className="flex flex-wrap items-end justify-between gap-4 border-b bg-surface-2/40 px-5 py-5">
                  <div>
                    <div className="text-[12px] text-muted">Amount</div>
                    <div className="mt-1 font-mono text-[30px] font-semibold leading-none tracking-tight tabular">{formatMoney(s.spec?.total, currency)}</div>
                  </div>
                  <div className="text-right text-[12px] text-muted">
                    <div>{s.spec?.tenant}</div>
                    <div>{formatPeriod(date(s.spec?.periodStartTime), date(s.spec?.periodEndTime), zone)}</div>
                  </div>
                </div>
                {groups.length > 0 && groups.some((g) => g.item !== "monthly_fee") && (
                  <div className="px-5 pt-4">
                    <div className="flex h-2 gap-0.5 overflow-hidden rounded-full">
                      {[...groups]
                        .sort((a, b) => b.amount - a.amount)
                        .map((g, i) => (
                          <div key={g.item} className={shades[Math.min(i, shades.length - 1)]} style={{ width: `${(g.amount / Math.max(groups.reduce((n, x) => n + x.amount, 0), 1e-9)) * 100}%` }} />
                        ))}
                    </div>
                  </div>
                )}
                <table className="mt-2 w-full text-[13px]">
                  <tbody>
                    {groups.map((g) => (
                      <ItemRows key={g.item} g={g} currency={currency} zone={zone} />
                    ))}
                    {promotions.map((l, i) => (
                      <tr key={`promotion-${i}`} className="border-b">
                        <td className="px-5 py-3">
                          <div className="font-medium text-ok">{l.description || "Promotion"}</div>
                          <div className="mt-0.5 font-mono text-[12px] text-muted">{formatQuantity(l.quantity)}% off</div>
                        </td>
                        <td className="px-5 py-3 text-right align-top font-mono font-medium text-ok tabular">{formatMoney(l.amount, currency)}</td>
                      </tr>
                    ))}
                    {groups.length === 0 && (
                      <tr className="border-b">
                        <td colSpan={2} className="px-5 py-6 text-center text-muted">
                          Nothing was billed in this period.
                        </td>
                      </tr>
                    )}
                  </tbody>
                  <tfoot>
                    <tr>
                      <td className="px-5 py-4 text-right font-medium">Total</td>
                      <td className="w-44 px-5 py-4 text-right font-mono text-[16px] font-semibold tabular">{formatMoney(s.spec?.total, currency)}</td>
                    </tr>
                  </tfoot>
                </table>
                {mayPay && s.spec?.metered && (
                  <div className="border-t px-5 py-2.5 text-[12px] text-subtle">
                    At list prices this period's holdings came to {formatMoney(s.spec.metered, currency)}; not billed.
                  </div>
                )}
              </Card>
            }
            side={
              <>
                <Card>
                  <CardHeader title="Payment" />
                  <dl className="divide-y px-4">
                    <Field label="Status">
                      <PaidPill s={s} zone={zone} />
                    </Field>
                    {paid && <Field label="Paid">{formatMoment(date(s.status?.paidTime), zone)}</Field>}
                    {paid && (
                      <Field label="Recorded by" mono>
                        {s.status?.paidRecordedBy}
                      </Field>
                    )}
                    <Field label="Made">{formatMoment(date(s.metadata?.createTime), zone)}</Field>
                  </dl>
                </Card>
                <EquivalentCard cli={[`hangar get statement ${name} -o yaml`, ...(mayPay ? [`hangar admin pay ${name}${paid ? " --undo" : ""}`] : [])]} />
              </>
            }
          />
        </>
      )}
    </DetailFrame>
  );
}

/** The platform's billing, for its administrators. */
export function PlatformBilling() {
  const perms = usePlatformCan([listAccountsPermission]);
  const may = perms.can(listAccountsPermission);
  const { accounts, loading, error, refetch } = useBillingAccounts(may);
  const stmts = useStatements(undefined, may);
  const lists = usePriceLists(may);
  const unpaid = stmts.statements.filter((s) => !s.status?.paidTime);
  const zoneOf = (t: string) => accounts.find((a) => a.metadata?.name === t)?.spec?.timeZone;
  const now = new Date();
  // The figures add up amounts of one currency only; another is counted apart.
  const currency = accounts[0]?.spec?.currency;
  const same = accounts.every((a) => a.spec?.currency === currency);
  const owed = outstanding(unpaid);
  const accrued = accounts.reduce((n, a) => n + Number(a.status?.accrued || 0), 0);
  return (
    <>
      <Topbar platform crumbs={["Platform", "Billing"]} />
      <PageHead title="Billing" />
      <div className="space-y-5 px-4 pb-10 md:px-8">
        {!perms.settled ? (
          <LoadingTable />
        ) : !may ? (
          <p className="text-[13px] text-muted">This account's roles do not include reading every tenant's billing.</p>
        ) : (
          <>
            <div className="grid grid-cols-1 gap-4 md:grid-cols-3">
              <Stat
                label="Outstanding"
                tone={owed.count > 0 ? "pending" : "ok"}
                loading={stmts.loading || loading}
                value={same ? formatMoney(owed.total.toFixed(2), currency) : `${owed.count}`}
                sub={owed.count > 0 ? `${owed.count} statement${owed.count === 1 ? "" : "s"} due, from ${new Set(unpaid.map((s) => s.spec?.tenant)).size} tenant${new Set(unpaid.map((s) => s.spec?.tenant)).size === 1 ? "" : "s"}` : "Every statement is paid"}
              />
              <Stat label="Open periods so far" loading={loading} value={same ? formatMoney(accrued.toFixed(2), currency) : "—"} sub={`Across ${accounts.length} account${accounts.length === 1 ? "" : "s"}`} />
              <Stat
                label="Accounts"
                loading={loading}
                value={String(accounts.length)}
                sub={`${accounts.filter((a) => a.spec?.type === BillingType.PAY_AS_YOU_GO).length} pay as you go · ${accounts.filter((a) => a.spec?.type === BillingType.FIXED).length} fixed`}
              />
            </div>
            <Card className="overflow-x-auto">
              <CardHeader title="Accounts" />
              {loading ? (
                <SkeletonRows rows={3} />
              ) : error ? (
                <ErrorNotice error={error} title="The accounts did not load" onRetry={() => void refetch()} />
              ) : accounts.length === 0 ? (
                <p className="px-4 py-3 text-[13px] text-muted">No tenant is billed.</p>
              ) : (
                <table className="w-full min-w-[760px] text-[13px]">
                  <thead>
                    <tr className={theadRow}>
                      <Th>Tenant</Th>
                      <Th>Agreement</Th>
                      <Th className="w-56">This period</Th>
                      <Th className="text-right">So far</Th>
                      <Th className="hidden text-right lg:table-cell">At list prices</Th>
                      <Th className="text-right">Due</Th>
                    </tr>
                  </thead>
                  <tbody>
                    {accounts.map((a) => {
                      const t = a.metadata?.name ?? "";
                      const theirs = outstanding(unpaid.filter((s) => s.spec?.tenant === t));
                      const zone = a.spec?.timeZone;
                      const start = date(a.status?.periodStartTime);
                      const end = date(a.status?.periodEndTime);
                      return (
                        <tr key={t} className={tbodyRow}>
                          <td className="px-4 py-3">
                            <Link to="/tenants/$tenant/billing" params={{ tenant: t }} className="font-mono font-medium group-hover:text-accent">
                              {t}
                            </Link>
                          </td>
                          <td className="px-4 py-3 text-muted">{agreementOf(a)}</td>
                          <td className="px-4 py-3">
                            <div className="text-[12px] text-muted">{formatPeriod(start, end, zone)}</div>
                            <PeriodTrack share={periodShare(start, end, now)} />
                          </td>
                          <td className="px-4 py-3 text-right font-mono tabular">{formatMoney(a.status?.accrued, a.spec?.currency)}</td>
                          <td className="hidden px-4 py-3 text-right font-mono text-muted tabular lg:table-cell">
                            {a.status?.periodType === BillingType.FIXED && a.status?.periodPriceList ? formatMoney(a.status.metered, a.spec?.currency) : "—"}
                          </td>
                          <td className={cn("px-4 py-3 text-right font-mono tabular", theirs.count > 0 ? "font-medium text-pending" : "text-subtle")}>
                            {theirs.count === 0 ? "—" : formatMoney(theirs.total.toFixed(2), a.spec?.currency)}
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              )}
            </Card>
            <div className="grid grid-cols-1 gap-5 xl:grid-cols-[minmax(0,1fr)_360px]">
              <Card className="min-w-0 overflow-x-auto">
                <CardHeader title={<span className="flex items-baseline gap-2">Due{!stmts.loading && <span className="font-mono text-[11.5px] font-normal text-subtle">{unpaid.length}</span>}</span>} />
                {stmts.loading ? (
                  <SkeletonRows rows={3} />
                ) : stmts.error ? (
                  <ErrorNotice error={stmts.error} title="The statements did not load" onRetry={() => void stmts.refetch()} />
                ) : unpaid.length === 0 ? (
                  <p className="px-4 py-3 text-[13px] text-muted">Every statement is paid.</p>
                ) : (
                  <StatementsList statements={unpaid} showTenant zoneOf={zoneOf} />
                )}
              </Card>
              <Card>
                <CardHeader title="Price lists" />
                <div className="divide-y px-4 pb-1">
                  {lists.loading ? (
                    <Skeleton className="my-3 h-4 w-40" />
                  ) : lists.lists.length === 0 ? (
                    <p className="py-3 text-[13px] text-muted">None.</p>
                  ) : (
                    lists.lists.map((l) => {
                      const { current, next } = entriesAt(l, now);
                      const users = accounts.filter((a) => (a.status?.periodPriceList || a.spec?.priceList) === l.metadata?.name).length;
                      return (
                        <div key={l.metadata?.name} className="py-3 text-[13px]">
                          <div className="flex items-baseline justify-between gap-3">
                            <span className="font-medium">{l.spec?.displayName || l.metadata?.name}</span>
                            <span className="font-mono text-[11.5px] text-subtle">{l.spec?.currency}</span>
                          </div>
                          <div className="mt-0.5 text-[12px] text-muted">
                            {pricesOf(current?.prices).length} prices in effect · rates {users} account{users === 1 ? "" : "s"}
                            {next && ` · changes ${formatMoment(date(next.effectiveTime), undefined)}`}
                          </div>
                        </div>
                      );
                    })
                  )}
                </div>
              </Card>
            </div>
          </>
        )}
      </div>
    </>
  );
}
