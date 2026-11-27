// A tenant's CPU pool as the console shows it (docs/decisions/cpu-pool.md):
// whether its machines share its plan's CPUs, and the vCPU tile's figures.

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { skipToken, useQuery } from "@connectrpc/connect-query";
import { useEffect, useState } from "react";
import type { MachinePhase } from "@hangar/api/hangar/compute/v1alpha1/machine_pb";
import { TenantCapabilityService, type CPUPoolUsageResponse } from "@hangar/api/hangar/tenancy/v1alpha1/tenant_capability_pb";
import type { MachineKind } from "@/lib/machines";
import { poolDropMs, poolRefetchMs, poolStaleMs } from "@/lib/memory-pool";
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

/** A running machine in its tenant's CPU pool, as its node last sampled it. */
export type CpuEntry = {
  project: string;
  kind: MachineKind;
  name: string;
  phase: MachinePhase;
  vcpus: number;
  /** The CPUs it used over its latest 10 s sample; undefined when its node has none lately. */
  nowCPU?: number;
};

/** How many minutes the sparkline draws: the hour the nodes keep. */
export const slowedMinutes = 60;

/** How long after the pool last slowed its machines the card keeps the live strip open. */
export const throttledForMs = 5 * 60_000;

/**
 * What the pool card shows of a tenant's CPU pool: the machines the caller
 * reads, by what they use now; what the rest use, summed; the hour's slowed
 * share by minute, slots oldest first, the last the minute before now's,
 * undefined where no node counted one; the hour's share; and when the pool
 * last slowed its machines, the end of the last minute with a share. current
 * as for the memory pool: the nodes' samples are of now, every node was
 * read, and the last ask did not fail.
 */
export type CpuOccupancy = {
  entries: CpuEntry[];
  otherCPU?: number;
  minutes: (number | undefined)[];
  slowed?: number;
  lastSlowedAt?: number;
  current: boolean;
};

const minuteMs = 60_000;

/** The CPU pool's occupancy from CPUPoolUsage's answer at now. */
export function cpuOccupancy(r: CPUPoolUsageResponse, now = Date.now()): CpuOccupancy {
  const sampled = r.sampleTime ? timestampDate(r.sampleTime).getTime() : undefined;
  const read = sampled === undefined ? r.machines.length === 0 : now - sampled <= poolStaleMs;
  const entries: CpuEntry[] = [];
  for (const m of r.machines) {
    if (m.kind !== "Container" && m.kind !== "VirtualMachine") continue;
    entries.push({ project: m.project, kind: m.kind, name: m.name, phase: m.phase, vcpus: m.vcpus, nowCPU: m.cpuNowMilli === undefined ? undefined : Number(m.cpuNowMilli) / 1000 });
  }
  // The slots end with the minute before now's, which the nodes keep
  // once it is over.
  const first = Math.floor(now / minuteMs) * minuteMs - slowedMinutes * minuteMs;
  const minutes: (number | undefined)[] = Array.from({ length: slowedMinutes }, () => undefined);
  let lastSlowedAt: number | undefined;
  for (const m of r.minutes) {
    if (!m.time) continue;
    const at = timestampDate(m.time).getTime();
    const i = Math.floor((at - first) / minuteMs);
    if (i >= 0 && i < slowedMinutes) minutes[i] = m.slowed;
    if (m.slowed > 0) lastSlowedAt = Math.max(lastSlowedAt ?? 0, at + minuteMs);
  }
  return {
    entries,
    otherCPU: r.other ? Number(r.other.cpuNowMilli) / 1000 : undefined,
    minutes,
    slowed: r.slowed,
    lastSlowedAt,
    current: read && r.nodesUnread === 0,
  };
}

/**
 * The pool card's read of the tenant's CPU pool, asked every poolRefetchMs
 * while enabled, as usePoolRead reads the memory pool: pending until the
 * first answer; failed when that fails, or once every ask has failed for
 * poolDropMs; otherwise read. throttled says the pool slowed its machines
 * within throttledForMs: it stays so until that long has passed since the
 * last slowed minute any answer showed, so an answer missing that minute,
 * as from a node unread, does not close the strip, nor does every poll
 * open and close it.
 */
export type CpuPoolRead = { state: "pending" } | { state: "failed" } | { state: "read"; occupancy: CpuOccupancy; throttled: boolean };

export function useCpuPoolRead(tenant: string, enabled: boolean): CpuPoolRead | undefined {
  const q = useQuery(TenantCapabilityService.method.cPUPoolUsage, enabled && tenant ? { tenant } : skipToken, {
    refetchInterval: poolRefetchMs,
    retry: false,
  });
  const [tick, setTick] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setTick(Date.now()), poolRefetchMs / 2);
    return () => clearInterval(id);
  }, []);
  const now = Math.max(tick, q.dataUpdatedAt, q.errorUpdatedAt);
  const occupancy = q.data ? cpuOccupancy(q.data, now) : undefined;
  // The latest end of a slowed minute any answer showed, for this tenant,
  // kept as React keeps what a render learnt of the renders before.
  const shown = occupancy?.lastSlowedAt ?? 0;
  const [seen, setSeen] = useState({ tenant, at: shown });
  const lastSlowedAt = seen.tenant === tenant ? Math.max(seen.at, shown) : shown;
  if (seen.tenant !== tenant || seen.at !== lastSlowedAt) setSeen({ tenant, at: lastSlowedAt });
  if (!enabled || !tenant) return undefined;
  if (q.isPending) return { state: "pending" };
  if (!occupancy || (q.isError && now - q.dataUpdatedAt > poolDropMs)) return { state: "failed" };
  const throttled = lastSlowedAt > 0 && now - lastSlowedAt < throttledForMs;
  return { state: "read", occupancy: q.isError ? { ...occupancy, current: false } : occupancy, throttled };
}
