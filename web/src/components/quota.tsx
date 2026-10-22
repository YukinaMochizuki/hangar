// What a project or a tenant holds against its limits, as the Overview of
// each shows it: the four that size machines as tiles, the counted kinds as a
// list. A limit left unset reads "no limit".

import type { Limits, Usage } from "@hangar/api/hangar/tenancy/v1alpha1/plan_pb";
import { Meter } from "@/components/meter";
import { Skeleton } from "@/components/states";
import { Card, CardHeader } from "@/components/ui";
import { formatNumber, quantityCPU, quantityGiB } from "@/lib/quantity";

export type Tile = { key: string; label: string; used: number; limit?: number; unit?: string };

export function computeTiles(limits: Limits | undefined, usage: Usage | undefined): Tile[] {
  const opt = (n: number) => (Number.isFinite(n) ? n : undefined);
  return [
    { key: "machines", label: "Machines", used: usage?.machines ?? 0, limit: limits?.machines },
    { key: "cpu", label: "vCPU", used: quantityCPU(usage?.cpu || "0"), limit: opt(quantityCPU(limits?.cpu)) },
    { key: "memory", label: "Memory", used: quantityGiB(usage?.memory || "0"), limit: opt(quantityGiB(limits?.memory)), unit: "GiB" },
    { key: "disk", label: "Disk", used: quantityGiB(usage?.disk || "0"), limit: opt(quantityGiB(limits?.disk)), unit: "GiB" },
  ];
}

/** The counted kinds; a tenant's list starts with its projects and ends with its custom domains, which are the tenant's alone. */
export function otherTiles(limits: Limits | undefined, usage: Usage | undefined, withProjects = false): Tile[] {
  return [
    ...(withProjects ? [{ key: "projects", label: "Projects", used: usage?.projects ?? 0, limit: limits?.projects }] : []),
    { key: "networks", label: "Networks", used: usage?.networks ?? 0, limit: limits?.networks },
    { key: "addresses", label: "Addresses", used: usage?.addresses ?? 0, limit: limits?.addresses },
    { key: "publicEndpoints", label: "Public endpoints", used: usage?.publicEndpoints ?? 0, limit: limits?.publicEndpoints },
    { key: "serviceTokens", label: "Service tokens", used: usage?.serviceTokens ?? 0, limit: limits?.serviceTokens },
    ...(withProjects ? [{ key: "domains", label: "Domains", used: usage?.domains ?? 0, limit: limits?.domains }] : []),
  ];
}

export function QuotaTiles({ tiles, loading }: { tiles: Tile[]; loading: boolean }) {
  return (
    <div className="grid grid-cols-2 gap-4 lg:grid-cols-4">
      {tiles.map((q) => (
        <Card key={q.key} className="px-4 py-3.5">
          <div className="text-[12px] text-muted">{q.label}</div>
          {loading ? (
            <Skeleton className="mt-2 h-6 w-20" />
          ) : (
            <div className="mt-1 flex items-baseline gap-1.5">
              <span className="font-mono text-[22px] font-medium tabular tracking-tight">{formatNumber(q.used)}</span>
              <span className="font-mono text-[13px] text-subtle tabular">
                {q.limit === undefined ? "no limit" : `/ ${formatNumber(q.limit)}`}
                {q.limit !== undefined && q.unit && ` ${q.unit}`}
              </span>
            </div>
          )}
          <Meter used={q.used} limit={q.limit ?? 0} className="mt-3" />
        </Card>
      ))}
    </div>
  );
}

export function OtherLimits({ tiles, loading, title = "Other limits" }: { tiles: Tile[]; loading: boolean; title?: string }) {
  return (
    <Card>
      <CardHeader title={title} />
      <ul className="space-y-3 px-4 py-3.5">
        {tiles.map((q) => (
          <li key={q.key} className="text-[13px]">
            <div className="flex justify-between">
              <span className="text-muted">{q.label}</span>
              {loading ? (
                <Skeleton className="h-3 w-10" />
              ) : (
                <span className="font-mono text-[12px] tabular">
                  {q.used} / {q.limit ?? "no limit"}
                </span>
              )}
            </div>
            <Meter used={q.used} limit={q.limit ?? 0} className="mt-1.5" />
          </li>
        ))}
      </ul>
    </Card>
  );
}
