// A tenant's CPU pool as the console shows it (docs/decisions/cpu-pool.md):
// whether its machines share its plan's CPUs, and the vCPU tile's figures.

import { skipToken, useQuery } from "@connectrpc/connect-query";
import { TenantCapabilityService } from "@hangar/api/hangar/tenancy/v1alpha1/tenant_capability_pb";
import { BillingType } from "@hangar/api/hangar/billing/v1alpha1/billing_account_pb";
import { ProjectService } from "@hangar/api/hangar/tenancy/v1alpha1/project_pb";
import type { Limits, Usage } from "@hangar/api/hangar/tenancy/v1alpha1/plan_pb";
import type { CpuPoolTile } from "@/components/quota";
import { useBillingAccount } from "@/lib/billing";
import { quantityCPU } from "@/lib/quantity";

/**
 * Whether limits, a tenant's or a project's status.limits, which carry the
 * plan's cpu_limit_percent, give its machines a CPU pool, and its percent:
 * one needs a percent above 100 and a cpu limit, and a BillingAccount not
 * PAY_AS_YOU_GO. The account is read only when the caller may; unknown, it
 * is taken as not.
 */
export function cpuPoolTerms(limits: Limits | undefined, billing?: BillingType): { pooled: boolean; percent: number } {
  const percent = limits?.cpuLimitPercent ?? 100;
  return { pooled: percent > 100 && !!limits?.cpu && billing !== BillingType.PAY_AS_YOU_GO, percent };
}

/**
 * The CPU pool terms of a project's tenant, from the project's status.limits
 * and the tenant's BillingAccount where the caller reads it.
 */
export function useProjectCpuPool(project: string): { pooled: boolean; percent: number } {
  const q = useQuery(ProjectService.method.getProject, { name: project });
  const p = q.data?.project;
  const tenant = p?.spec?.tenant ?? "";
  const billing = useBillingAccount(tenant, !!tenant && (p?.status?.limits?.cpuLimitPercent ?? 100) > 100);
  return cpuPoolTerms(p?.status?.limits, billing.account?.spec?.type);
}

/** The vCPU tile's figures for a pooled status at percent, slowedPercent as the nodes say. */
export function cpuPoolFigures(limits: Limits | undefined, usage: Usage | undefined, percent: number, slowedPercent?: number): CpuPoolTile {
  const planCPU = quantityCPU(limits?.cpu);
  return { planCPU, vcpus: quantityCPU(usage?.cpu || "0"), capCPU: (planCPU * percent) / 100, slowedPercent };
}

/** How often the tile asks its tenant's nodes again: they sample every 10 s, a point a minute. */
const slowedRefetchMs = 60_000;

/**
 * The vCPU tile's figures for a tenant's or a project's status, undefined
 * without a CPU pool, where the tile is as for any limit. How often the pool
 * slowed its machines in the last hour is asked of its nodes, where the
 * caller may get the tenant; unknown, the tile says nothing of it.
 */
export function useCpuPoolTile(tenant: string, limits: Limits | undefined, usage: Usage | undefined) {
  const billing = useBillingAccount(tenant, !!tenant && (limits?.cpuLimitPercent ?? 100) > 100);
  const t = cpuPoolTerms(limits, billing.account?.spec?.type);
  const slowed = useQuery(TenantCapabilityService.method.cPUPoolUsage, t.pooled && tenant ? { tenant } : skipToken, {
    refetchInterval: slowedRefetchMs,
    retry: false,
  });
  const share = slowed.data?.slowed;
  return t.pooled ? cpuPoolFigures(limits, usage, t.percent, share === undefined ? undefined : share * 100) : undefined;
}
