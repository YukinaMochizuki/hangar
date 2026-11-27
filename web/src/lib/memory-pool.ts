// A tenant's memory pool as the console shows it (docs/decisions/memory-pool.md):
// whether its machines share its memory, what a machine keeps reserved of its
// memory, and what its node says of a pooled machine's memory.

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { skipToken, useQuery } from "@connectrpc/connect-query";
import type { MachinePhase } from "@hangar/api/hangar/compute/v1alpha1/machine_pb";
import { ProjectService } from "@hangar/api/hangar/tenancy/v1alpha1/project_pb";
import { TenantCapabilityService, type MemoryPoolUsageResponse } from "@hangar/api/hangar/tenancy/v1alpha1/tenant_capability_pb";
import { useBillingAccount } from "@/lib/billing";
import { BillingType } from "@hangar/api/hangar/billing/v1alpha1/billing_account_pb";
import { ConditionStatus, type Condition } from "@hangar/api/hangar/meta/v1/meta_pb";
import type { Limits, Usage } from "@hangar/api/hangar/tenancy/v1alpha1/plan_pb";
import { memoryStepMiB } from "@/lib/machine-sizes";
import type { MachineKind, MachineView } from "@/lib/machines";
import { parseQuantity, quantityGiB } from "@/lib/quantity";

/** How a tenant's machines hold their memory: as a pool of `percent`, or each alone. */
export type PoolTerms = { pooled: boolean; percent: number };

/**
 * The pool terms of limits, a tenant's or a project's status.limits, which
 * carry the plan's percent: a pool needs a percent above 100 and a memory
 * limit, and a BillingAccount not PAY_AS_YOU_GO. The account is read only
 * when the caller may; unknown, it is taken as not, and the API refuses a
 * reservation below memory should it be.
 */
export function poolTerms(limits: Limits | undefined, billing?: BillingType): PoolTerms {
  const percent = limits?.memoryLimitPercent ?? 100;
  const pooled = percent > 100 && !!limits?.memory && billing !== BillingType.PAY_AS_YOU_GO;
  return { pooled, percent };
}

/**
 * The pool terms of a project's tenant, from the project's status.limits and
 * the tenant's BillingAccount where the caller reads it; known once the
 * project has loaded.
 */
export function useProjectPool(project: string): PoolTerms & { known: boolean; tenant: string } {
  const q = useQuery(ProjectService.method.getProject, { name: project });
  const p = q.data?.project;
  const tenant = p?.spec?.tenant ?? "";
  const billing = useBillingAccount(tenant, !!tenant && (p?.status?.limits?.memoryLimitPercent ?? 100) > 100);
  return { ...poolTerms(p?.status?.limits, billing.account?.spec?.type), known: !!p, tenant };
}

/**
 * The reservation a Shared machine of memMiB starts at: its memory divided
 * by the plan's percent, in 256 MiB steps, at least one step. 4 GiB at 200%
 * is 2 GiB.
 */
export function defaultReservedMiB(memMiB: number, percent: number): number {
  const steps = Math.round((memMiB * 100) / percent / memoryStepMiB);
  return Math.min(memMiB, Math.max(1, steps) * memoryStepMiB);
}

/** Whether a reservation typed in MiB is one the API takes for memMiB: whole 256 MiB steps, at least one, at most memory. */
export function reservedProblem(reservedMiB: number, memMiB: number): string | undefined {
  if (!Number.isFinite(reservedMiB) || reservedMiB < memoryStepMiB || reservedMiB % memoryStepMiB !== 0) return "Reserved memory is a whole number of 256 MiB steps.";
  if (reservedMiB > memMiB) return "Reserved memory is at most the machine's memory.";
  return undefined;
}

/** A machine's memory as a pool shares it: its reservation, and whether that leaves it Shared. */
export type MachineMemory = { memoryGi: number; reservedGi: number; shared: boolean };

/** A memory quantity in GiB, unrounded, so 256 MiB steps such as 1.75 GiB stay exact. */
export const exactGiB = (q: string | undefined) => parseQuantity(q) / 1024 ** 3;

/** The machine's reservation, from its object's spec; empty is its memory. */
export function machineMemory(m: Pick<MachineView, "memoryGi" | "container" | "vm">): MachineMemory {
  const res = m.container?.spec?.resources ?? m.vm?.spec?.resources;
  const memoryGi = res?.memory ? exactGiB(res.memory) : m.memoryGi;
  const reservedGi = res?.memoryReserved ? exactGiB(res.memoryReserved) : memoryGi;
  return { memoryGi, reservedGi, shared: reservedGi < memoryGi };
}

/** The machine's Machine condition of type, when it holds status. */
function machineCondition(m: Pick<MachineView, "machine">, type: string, status: ConditionStatus): Condition | undefined {
  return m.machine?.status?.conditions.find((c) => c.type === type && c.status === status);
}

/** When the host last killed the machine's VM whole for its pool's memory, from the node's OOMKilled condition; undefined when it never did. */
export function lastOOMKill(m: Pick<MachineView, "machine">): Date | undefined {
  const c = machineCondition(m, "OOMKilled", ConditionStatus.TRUE);
  if (!c) return undefined;
  // The message says when, as RFC 3339; the transition time is when it first did.
  const at = c.message.match(/\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?Z/)?.[0];
  const parsed = at ? new Date(at) : undefined;
  if (parsed && !Number.isNaN(parsed.getTime())) return parsed;
  return c.lastTransitionTime ? timestampDate(c.lastTransitionTime) : new Date(0);
}

/** How long a kill for memory is news: within it, the machine's page says so in a banner and its row in the list. */
export const recentKillMs = 24 * 3600_000;

/** Whether the machine was killed for memory within recentKillMs of now. */
export function killedRecently(m: Pick<MachineView, "machine">, now = Date.now()): Date | undefined {
  const at = lastOOMKill(m);
  return at && now - at.getTime() < recentKillMs ? at : undefined;
}

/** The node's BalloonResponsive=False, while the machine's balloon does not follow what its node asks. */
export function balloonStuck(m: Pick<MachineView, "machine">): Condition | undefined {
  return machineCondition(m, "BalloonResponsive", ConditionStatus.FALSE);
}

/** A running machine whose Machine is not in its tenant's pool yet: a reservation it is given takes effect when it starts again. */
export function outsidePool(m: Pick<MachineView, "machine" | "stopped">): boolean {
  return !m.stopped && !!m.machine && !m.machine.spec?.memoryPool;
}

/**
 * The memory tile's figures for a pooled tenant or project: the memory of
 * its machines asked to run (usage.memory_ceiling) against the plan's memory,
 * and the real cap, the plan's memory times its percent.
 */
export function poolFigures(limits: Limits | undefined, usage: Usage | undefined, percent: number) {
  const planGi = quantityGiB(limits?.memory);
  const ceilingGi = exactGiB(usage?.memoryCeiling || "0");
  return { planGi, ceilingGi, capGi: (planGi * percent) / 100 };
}

/** A running machine in its tenant's pool, as its node last read it, in GiB. */
export type PoolEntry = {
  project: string;
  kind: MachineKind;
  name: string;
  phase: MachinePhase;
  /** Its limit, and what it keeps reserved: all of it when Dedicated. */
  memoryGi: number;
  reservedGi: number;
  /** What its guest has now; undefined when its node could not tell. */
  nowGi?: number;
  /** What its guest uses of that; undefined when it reports nothing, or not lately. */
  inUseGi?: number;
  /** When its node killed it for the pool's memory, within recentKillMs. */
  killedAt?: Date;
  /** Since when its balloon has not followed what its node asks. */
  stuckSince?: Date;
};

/**
 * Machines in the pool summed, in GiB: those of projects the caller does not
 * read. inUseGi counts one whose guest does not say at what it has, or its
 * reservation.
 */
export type PoolTotal = { memoryGi: number; reservedGi: number; nowGi: number; inUseGi: number };

/** Who holds a tenant's pool now: its size, the machines the caller reads, and the rest summed. */
export type PoolOccupancy = { sizeGi: number; entries: PoolEntry[]; other?: PoolTotal };

const bytesGi = (b: bigint) => Number(b) / 1024 ** 3;

/** The pool's occupancy from MemoryPoolUsage's answer at now; undefined for a tenant with no pool. */
export function poolOccupancy(r: MemoryPoolUsageResponse, now = Date.now()): PoolOccupancy | undefined {
  if (r.sizeBytes === undefined) return undefined;
  const entries: PoolEntry[] = [];
  for (const m of r.machines) {
    if (m.kind !== "Container" && m.kind !== "VirtualMachine") continue;
    const killedAt = m.lastOomKillTime ? timestampDate(m.lastOomKillTime) : undefined;
    entries.push({
      project: m.project,
      kind: m.kind,
      name: m.name,
      phase: m.phase,
      memoryGi: bytesGi(m.memoryBytes),
      reservedGi: bytesGi(m.reservedBytes),
      nowGi: m.currentBytes === undefined ? undefined : bytesGi(m.currentBytes),
      inUseGi: m.inUseBytes === undefined ? undefined : bytesGi(m.inUseBytes),
      killedAt: killedAt && now - killedAt.getTime() < recentKillMs ? killedAt : undefined,
      stuckSince: m.balloonUnresponsiveSince ? timestampDate(m.balloonUnresponsiveSince) : undefined,
    });
  }
  const o = r.other;
  return {
    sizeGi: bytesGi(r.sizeBytes),
    entries,
    other: o ? { memoryGi: bytesGi(o.memoryBytes), reservedGi: bytesGi(o.reservedBytes), nowGi: bytesGi(o.currentBytes), inUseGi: bytesGi(o.inUseBytes) } : undefined,
  };
}

/** How often the pool card asks again: the nodes read their pools every few seconds. */
export const poolRefetchMs = 10_000;

/**
 * Who holds the tenant's pool now, asked of its nodes every poolRefetchMs
 * while enabled; undefined while the first answer is pending, when it
 * failed, and for a tenant with no pool.
 */
export function usePoolOccupancy(tenant: string, enabled: boolean): PoolOccupancy | undefined {
  const q = useQuery(TenantCapabilityService.method.memoryPoolUsage, enabled && tenant ? { tenant } : skipToken, {
    refetchInterval: poolRefetchMs,
    retry: false,
  });
  return q.data ? poolOccupancy(q.data) : undefined;
}

/**
 * The memory tile's figures for a tenant's or a project's status, undefined
 * without a pool, where the tile is as for any limit. The tenant's
 * BillingAccount is read where the caller may and the plan's percent is above 100.
 */
export function usePoolTile(tenant: string, limits: Limits | undefined, usage: Usage | undefined) {
  const billing = useBillingAccount(tenant, !!tenant && (limits?.memoryLimitPercent ?? 100) > 100);
  const t = poolTerms(limits, billing.account?.spec?.type);
  return t.pooled ? poolFigures(limits, usage, t.percent) : undefined;
}
