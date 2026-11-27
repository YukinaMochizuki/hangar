// A burstable machine as the console shows it (docs/decisions/cpu-credits.md):
// whether its tenant may choose a baseline, what each costs, and the one
// line its Usage card says of its credits.

import { useQuery } from "@connectrpc/connect-query";
import { BillingType } from "@hangar/api/hangar/billing/v1alpha1/billing_account_pb";
import type { Prices } from "@hangar/api/hangar/billing/v1alpha1/price_list_pb";
import { ProjectService } from "@hangar/api/hangar/tenancy/v1alpha1/project_pb";
import { entriesAt, useBillingAccount, usePriceList } from "@/lib/billing";
import { formatNumber } from "@/lib/quantity";

/**
 * Whether a project's tenant may run burstable machines, which needs its
 * BillingAccount to be PAY_AS_YOU_GO, and the prices and currency that rate
 * it now. The account is read only where the caller may; unknown, it is
 * taken as not, and the form offers no choice.
 */
export function useProjectBurstable(project: string): { payg: boolean; known: boolean; prices?: Prices; currency?: string } {
  const q = useQuery(ProjectService.method.getProject, { name: project });
  const tenant = q.data?.project?.spec?.tenant ?? "";
  const billing = useBillingAccount(tenant, !!tenant);
  const payg = billing.account?.spec?.type === BillingType.PAY_AS_YOU_GO;
  const list = usePriceList(payg ? billing.account?.spec?.priceList : undefined);
  return {
    payg,
    known: !!tenant && !billing.loading,
    prices: entriesAt(list.list, new Date()).current?.prices,
    currency: billing.account?.spec?.currency,
  };
}

/** What the Usage card knows of a burstable machine's credits, from its node. */
export type Credits = {
  /** Its baseline in percent, 20 or 40. */
  baseline: number;
  /** Its vCPUs. */
  vcpus: number;
  /** Its credits now in CPU-minutes; unset when its node holds no balance for it yet. */
  balance?: number;
  /** Whether its node holds it to its baseline now. */
  held: boolean;
};

/**
 * About how long its credits last with every vCPU busy, as "about 3 h": it
 * spends its vCPUs a minute and earns its baseline of them back, so a
 * minute at full speed costs vCPUs × (1 − baseline).
 */
export function fullSpeedFor(c: Credits): string | undefined {
  if (c.balance === undefined || c.vcpus <= 0) return undefined;
  const minutes = c.balance / (c.vcpus * (1 - c.baseline / 100));
  if (minutes < 1) return "under a minute";
  if (minutes < 60) return `about ${Math.round(minutes)} min`;
  const hours = minutes / 60;
  // Half hours under two, whole hours past: it is a guess, not a promise.
  return `about ${hours < 2 ? formatNumber(Math.round(hours * 2) / 2) : Math.round(hours)} h`;
}

/**
 * The line under a burstable machine's CPU plot: "Burstable 20% · 412
 * credits · about 3 h at full speed", or once spent "Burstable 20% · out of
 * credits, held to 20%".
 */
export function creditsLine(c: Credits): string {
  const head = `Burstable ${c.baseline}%`;
  if (c.held) return `${head} · out of credits, held to ${c.baseline}%`;
  if (c.balance === undefined) return `${head} · credits not counted yet`;
  const n = Math.floor(c.balance);
  const lasts = fullSpeedFor(c);
  return `${head} · ${formatNumber(n)} credit${n === 1 ? "" : "s"}${lasts ? ` · ${lasts} at full speed` : ""}`;
}
