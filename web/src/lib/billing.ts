// A tenant's billing as the console shows it: its account, its statements
// and the prices that rate it, read from the API, and the words and numbers
// they are shown in (docs/decisions/billing.md). Amounts stay the API's
// decimal strings until they are formatted, so nothing is rounded twice.

import { useQuery } from "@connectrpc/connect-query";
import { timestampDate, type Timestamp } from "@bufbuild/protobuf/wkt";
import {
  BillingAccountService,
  BillingType,
  type BillingAccount,
} from "@hangar/api/hangar/billing/v1alpha1/billing_account_pb";
import { PriceListService, type PriceEntry, type PriceList, type Prices } from "@hangar/api/hangar/billing/v1alpha1/price_list_pb";
import { StatementEnd, StatementService, type Statement } from "@hangar/api/hangar/billing/v1alpha1/statement_pb";
import type { StatementLine } from "@hangar/api/hangar/billing/v1alpha1/line_pb";
import { isNotFound, isPermissionDenied } from "@/lib/api";
import { usePlatformCan } from "@/lib/permissions";

/** What recording a statement paid needs, at the platform. */
export const payPermission = "billing.statements.pay";

/** What the platform's Billing page needs, at the platform. */
export const listAccountsPermission = "billing.billingaccounts.list";

/** Whether the caller reads every tenant's billing; not while the answer is unknown. */
export function useMayReadBilling(): boolean {
  return usePlatformCan([listAccountsPermission]).can(listAccountsPermission);
}

/** A tenant's billing account, asked while `enabled`; `none` once the API said it has none, `denied` once it refused. */
export function useBillingAccount(tenant: string, enabled = true) {
  const q = useQuery(BillingAccountService.method.getBillingAccount, { name: tenant }, { refetchInterval: 60_000, retry: false, enabled });
  const none = !!q.error && isNotFound(q.error);
  const denied = !!q.error && isPermissionDenied(q.error);
  return { account: q.data?.billingAccount, loading: q.isPending, error: none || denied ? undefined : q.error, none, denied, refetch: q.refetch };
}

/** Every billing account the caller reads. */
export function useBillingAccounts(enabled = true) {
  const q = useQuery(BillingAccountService.method.listBillingAccounts, {}, { refetchInterval: 60_000, retry: false, enabled });
  return { accounts: q.data?.items ?? [], loading: q.isPending && enabled, error: q.error, refetch: q.refetch };
}

/** The statements the caller reads, of one tenant if named, newest first. */
export function useStatements(tenant?: string, enabled = true) {
  const q = useQuery(StatementService.method.listStatements, {}, { refetchInterval: 60_000, retry: false, enabled });
  const items = (q.data?.items ?? []).filter((s) => !tenant || s.spec?.tenant === tenant);
  items.sort((a, b) => time(b.spec?.periodStartTime) - time(a.spec?.periodStartTime));
  return { statements: items, loading: q.isPending && enabled, error: q.error, refetch: q.refetch };
}

export function useStatement(name: string) {
  const q = useQuery(StatementService.method.getStatement, { name }, { retry: false });
  const missing = !!q.error && (isNotFound(q.error) || isPermissionDenied(q.error));
  return { statement: q.data?.statement, loading: q.isPending, error: missing ? undefined : q.error, missing, refetch: q.refetch };
}

export function usePriceList(name: string | undefined) {
  const q = useQuery(PriceListService.method.getPriceList, { name: name ?? "" }, { retry: false, enabled: !!name });
  return { list: q.data?.priceList, loading: q.isPending && !!name, error: q.error };
}

export function usePriceLists(enabled = true) {
  const q = useQuery(PriceListService.method.listPriceLists, {}, { retry: false, enabled });
  return { lists: q.data?.items ?? [], loading: q.isPending && enabled, error: q.error, refetch: q.refetch };
}

function time(ts?: Timestamp): number {
  return ts ? timestampDate(ts).getTime() : 0;
}

export function date(ts?: Timestamp): Date | undefined {
  return ts ? timestampDate(ts) : undefined;
}

/** The digits after the point of a currency's minor unit, as the browser knows it. */
function minorDigits(currency: string): number {
  try {
    return new Intl.NumberFormat("en", { style: "currency", currency }).resolvedOptions().maximumFractionDigits ?? 2;
  } catch {
    return 2;
  }
}

/**
 * An amount in its currency, as "TWD 1,280.40": at least the currency's
 * minor digits, and up to six when the amount carries them, as a line's or
 * a price's does.
 */
export function formatMoney(amount: string | undefined, currency: string | undefined): string {
  if (amount === undefined || amount === "") return "—";
  const n = Number(amount);
  if (!Number.isFinite(n)) return amount;
  const frac = amount.includes(".") ? amount.split(".")[1].length : 0;
  const cur = currency || "";
  const minor = cur ? minorDigits(cur) : 2;
  const digits = new Intl.NumberFormat("en", { minimumFractionDigits: minor, maximumFractionDigits: Math.max(minor, Math.min(frac, 6)) }).format(n);
  return cur ? `${cur} ${digits}` : digits;
}

/** A quantity of units, as "1,440.5". */
export function formatQuantity(q: string | undefined): string {
  if (!q) return "—";
  const n = Number(q);
  return Number.isFinite(n) ? new Intl.NumberFormat("en", { maximumFractionDigits: 6 }).format(n) : q;
}

/** What each item billed is called, in the order a statement lists them. */
export const itemLabels: Record<string, string> = {
  monthly_fee: "Monthly fee",
  cpu: "vCPU",
  memory: "Memory",
  disk: "Disk",
  addresses: "IPv6 addresses",
  public_endpoints: "Public endpoints",
  port_forwards: "Port forwards",
  file_shares: "File shares",
  network_endpoints: "Network endpoints",
  domains: "Custom domains",
};

/** What a price is per, as the price list's card says it. */
export const itemUnits: Record<string, string> = {
  cpu: "vCPU-hour",
  memory: "GiB-hour",
  disk: "GiB-hour",
  addresses: "address-hour",
  public_endpoints: "endpoint-hour",
  port_forwards: "port-forward-hour",
  file_shares: "file-share-hour",
  network_endpoints: "endpoint-hour",
  domains: "domain-hour",
};

/** A price entry's prices, item by item, those it sets alone. */
export function pricesOf(p: Prices | undefined): { item: string; price: string }[] {
  if (!p) return [];
  const all: [string, string][] = [
    ["cpu", p.cpu], ["memory", p.memory], ["disk", p.disk], ["addresses", p.addresses],
    ["public_endpoints", p.publicEndpoints], ["port_forwards", p.portForwards], ["file_shares", p.fileShares],
    ["network_endpoints", p.networkEndpoints], ["domains", p.domains],
  ];
  return all.filter(([, price]) => price !== "").map(([item, price]) => ({ item, price }));
}

/** The entry of a price list in effect at a time, and the next after it. */
export function entriesAt(list: PriceList | undefined, at: Date): { current?: PriceEntry; next?: PriceEntry } {
  const entries = list?.spec?.entries ?? [];
  let current: PriceEntry | undefined;
  for (const e of entries) {
    const t = date(e.effectiveTime);
    if (!t) continue;
    if (t.getTime() <= at.getTime()) current = e;
    else return { current, next: e };
  }
  return { current };
}

/**
 * The agreement of the period open as a phrase: "Pay as you go, every 3
 * months" or "Plan standard, TWD 300.00 a month".
 */
export function agreementOf(a: BillingAccount | undefined): string {
  const spec = a?.spec;
  const st = a?.status;
  if (!spec) return "";
  if ((st?.periodType || spec.type) === BillingType.FIXED) {
    return st?.periodPlan ? `Plan ${st.periodPlan}, ${formatMoney(st.periodMonthlyFee, spec.currency)} a month` : "A fixed fee a month";
  }
  const months = Math.max(st?.periodMonths || spec.periodMonths, 1);
  return `Pay as you go, every ${months === 1 ? "month" : `${months} months`}`;
}

export function typeLabel(t: BillingType | undefined): string {
  return t === BillingType.FIXED ? "Fixed" : t === BillingType.PAY_AS_YOU_GO ? "Pay as you go" : "—";
}

export const endLabels: Record<StatementEnd, string> = {
  [StatementEnd.UNSPECIFIED]: "—",
  [StatementEnd.PERIOD]: "Period ended",
  [StatementEnd.AGREEMENT_CHANGED]: "Agreement changed",
  [StatementEnd.ACCOUNT_DELETED]: "Account closed",
};

function dateIn(d: Date, zone: string | undefined, withTime: boolean): string {
  return new Intl.DateTimeFormat("en", {
    timeZone: zone || "UTC",
    year: "numeric",
    month: "short",
    day: "numeric",
    ...(withTime ? { hour: "2-digit", minute: "2-digit", hourCycle: "h23" } : {}),
  }).format(d);
}

function midnightIn(d: Date, zone: string | undefined): boolean {
  const parts = new Intl.DateTimeFormat("en", { timeZone: zone || "UTC", hour: "2-digit", minute: "2-digit", hourCycle: "h23" }).format(d);
  return parts === "00:00";
}

/**
 * A period, [start, end), in the account's zone: by days, the last day
 * included, as "Dec 1, 2026 – Feb 28, 2027", when both bounds are at
 * midnight there; to the minute otherwise.
 */
export function formatPeriod(start: Date | undefined, end: Date | undefined, zone: string | undefined): string {
  if (!start || !end) return "—";
  if (midnightIn(start, zone) && midnightIn(end, zone)) {
    const last = new Date(end.getTime() - 60_000);
    return `${dateIn(start, zone, false)} – ${dateIn(last, zone, false)}`;
  }
  return `${dateIn(start, zone, true)} – ${dateIn(end, zone, true)}`;
}

/** A moment in the account's zone, to the minute. */
export function formatMoment(d: Date | undefined, zone: string | undefined): string {
  return d ? dateIn(d, zone, true) : "—";
}

/** How far through its period a moment is, from 0 to 1. */
export function periodShare(start: Date | undefined, end: Date | undefined, now: Date): number {
  if (!start || !end || end <= start) return 0;
  return Math.min(Math.max((now.getTime() - start.getTime()) / (end.getTime() - start.getTime()), 0), 1);
}

/** One item's lines, a line for each price in effect, and what they come to together. */
export type LineGroup = { item: string; unit: string; quantity: number; amount: number; lines: StatementLine[] };

/** Lines by item, in the order the items first appear. */
export function groupLines(lines: StatementLine[]): LineGroup[] {
  const out: LineGroup[] = [];
  for (const l of lines) {
    let g = out.find((x) => x.item === l.item);
    if (!g) {
      g = { item: l.item, unit: l.unit, quantity: 0, amount: 0, lines: [] };
      out.push(g);
    }
    g.quantity += Number(l.quantity) || 0;
    g.amount += Number(l.amount) || 0;
    g.lines.push(l);
  }
  return out;
}

/** What statements not paid come to, and how many there are. */
export function outstanding(statements: Statement[]): { count: number; total: number } {
  const due = statements.filter((s) => !s.status?.paidTime);
  return { count: due.length, total: due.reduce((n, s) => n + (Number(s.spec?.total) || 0), 0) };
}

/**
 * What a period would come to at the pace of what has passed of it: none
 * before a day has, when the pace says too little.
 */
export function projected(accrued: string | undefined, start: Date | undefined, end: Date | undefined, asOf: Date | undefined): number | undefined {
  if (!accrued || !start || !end || !asOf) return undefined;
  const passed = asOf.getTime() - start.getTime();
  if (passed < 86_400_000) return undefined;
  return (Number(accrued) * (end.getTime() - start.getTime())) / passed;
}

export type { BillingAccount, PriceList, Statement };
