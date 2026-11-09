// A machine's usage as its node sampled it over the last hour
// (docs/decisions/machine-usage.md), shaped for the Usage card.

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { skipToken, useQuery } from "@connectrpc/connect-query";
import { MachineCapabilityService, MachineMemoryKind, type MachineUsagePoint, type UsageResponse } from "@hangar/api/hangar/compute/v1alpha1/machine_capability_pb";
import type { Usage } from "@/components/machine-usage";
import type { Point } from "@/lib/monitoring";
import type { MachineKind } from "@/lib/machines";

/** How often the card asks again: the node samples every 10 s. */
export const usageRefetchMs = 10_000;

// The latest sample lies in the minute after the last point's, which starts
// 60 s after it, and is at most 30 s old: it joins the line up to 150 s on.
const latestGapSeconds = 150;

function point(p: MachineUsagePoint, value: number | undefined): Point[] {
  return p.time && value !== undefined ? [{ time: timestampDate(p.time), value }] : [];
}

/**
 * The card's series: the minute points, a missing minute breaking the line,
 * then the latest sample, so each line reaches the present and its head
 * shows the value now.
 */
export function toUsage(r: UsageResponse, kind: MachineKind): Usage {
  const mem = (p: MachineUsagePoint) => (p.memoryBytes === undefined ? undefined : Number(p.memoryBytes));
  const series = (value: (p: MachineUsagePoint) => number | undefined) => {
    const latest = r.latest ? point(r.latest, value(r.latest)) : [];
    return {
      stepSeconds: 60,
      ...(latest.length > 0 ? { lastGapSeconds: latestGapSeconds } : {}),
      points: [...r.points.flatMap((p) => point(p, value(p))), ...latest],
    };
  };
  return {
    memoryKind:
      r.memoryKind === MachineMemoryKind.GUEST_USED ? "guest" : r.memoryKind === MachineMemoryKind.HOST_HELD ? "host" : kind === "Container" ? "guest" : "host",
    cpu: { unit: "ratio", capacity: 1, ...series((p) => p.cpu) },
    memory: { unit: "bytes", ...(r.memoryLimitBytes > 0n ? { capacity: Number(r.memoryLimitBytes) } : {}), ...series(mem) },
  };
}

/** Whether the API said the machine is not running (it stopped, or has not started, between two asks). */
export function isNotRunning(err: unknown): boolean {
  return err instanceof ConnectError && err.code === Code.FailedPrecondition;
}

/**
 * The machine's usage, asked every 10 s while `ask` holds (the machine
 * runs). A failed ask is not retried: the next interval asks again.
 */
export function useMachineUsage(project: string, kind: MachineKind, name: string, ask: boolean) {
  const q = useQuery(MachineCapabilityService.method.usage, ask ? { project, kind, name } : skipToken, {
    refetchInterval: usageRefetchMs,
    retry: false,
  });
  return { data: q.data && toUsage(q.data, kind), loading: ask && q.isPending, error: q.error, refetch: () => void q.refetch() };
}
