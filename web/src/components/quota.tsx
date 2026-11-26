// What a project or a tenant holds against its limits, as the Overview of
// each shows it: the four that size machines as tiles, the counted kinds as a
// list. A limit left unset reads "no limit".

import type { Limits, Usage } from "@hangar/api/hangar/tenancy/v1alpha1/plan_pb";
import { Meter } from "@/components/meter";
import { Skeleton } from "@/components/states";
import { Card, CardHeader } from "@/components/ui";
import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { cn } from "@/lib/cn";
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
    { key: "portForwards", label: "Port forwards", used: usage?.portForwards ?? 0, limit: limits?.portForwards },
    { key: "serviceTokens", label: "Service tokens", used: usage?.serviceTokens ?? 0, limit: limits?.serviceTokens },
    { key: "fileShares", label: "File shares", used: usage?.fileShares ?? 0, limit: limits?.fileShares },
    { key: "customImages", label: "Custom images", used: usage?.customImages ?? 0, limit: limits?.customImages },
    { key: "networkEndpoints", label: "Network endpoints", used: usage?.networkEndpoints ?? 0, limit: limits?.networkEndpoints },
    ...(withProjects ? [{ key: "domains", label: "Domains", used: usage?.domains ?? 0, limit: limits?.domains }] : []),
  ];
}

/** The memory tile's figures for a tenant with a pool (lib/memory-pool.ts poolFigures). */
export type PoolTile = { planGi: number; ceilingGi: number; capGi: number };

/**
 * The first of texts that fits the width it is given, measured: the longest
 * is drawn first, and the next while it overflows; a change of width starts
 * over. The last is cut short when even it does not fit.
 */
function FittingText({ texts, className }: { texts: string[]; className?: string }) {
  const ref = useRef<HTMLSpanElement>(null);
  // Which text is drawn, and how often its width changed, so a new width measures again.
  const [fit, setFit] = useState({ i: 0, key: texts.join("\n"), resizes: 0 });
  const key = texts.join("\n");
  if (fit.key !== key) setFit({ i: 0, key, resizes: fit.resizes });
  useLayoutEffect(() => {
    const el = ref.current;
    if (el && el.scrollWidth > el.clientWidth && fit.i < texts.length - 1) setFit((f) => ({ ...f, i: f.i + 1 }));
  }, [fit, texts.length]);
  useEffect(() => {
    const parent = ref.current?.parentElement;
    if (!parent || typeof ResizeObserver === "undefined") return;
    let width = parent.clientWidth;
    const ro = new ResizeObserver(() => {
      if (parent.clientWidth === width) return;
      width = parent.clientWidth;
      setFit((f) => ({ ...f, i: 0, resizes: f.resizes + 1 }));
    });
    ro.observe(parent);
    return () => ro.disconnect();
  }, []);
  return (
    <span ref={ref} className={className}>
      {texts[Math.min(fit.i, texts.length - 1)]}
    </span>
  );
}

/**
 * The memory tile of a tenant whose machines share its memory as a pool
 * (docs/decisions/memory-pool.md): the memory of its machines asked to run
 * against the plan's memory, as the other tiles count, which it may pass.
 * Past it, the bar is full and a note beside the number says by how much,
 * in the shortest words that fit; the bar's colour follows the real cap, the
 * plan's memory times its percent, which no figure here names.
 */
function MemoryPoolTile({ pool }: { pool: PoolTile }) {
  const over = pool.ceilingGi - pool.planGi;
  const ratio = pool.capGi > 0 ? pool.ceilingGi / pool.capGi : 0;
  const tone = ratio >= 1 ? "bg-fail" : ratio >= 0.8 ? "bg-pending" : "bg-foreground/70";
  const o = formatNumber(over);
  const notes = [`${o} GiB over plan · shared`, `+${o} GiB shared`, `+${o} GiB`];
  return (
    <Card className="px-4 py-3.5">
      <div className="text-[12px] text-muted">Memory</div>
      <div className="mt-1 flex items-baseline gap-1.5">
        <span className="font-mono text-[22px] font-medium tabular tracking-tight">{formatNumber(pool.ceilingGi)}</span>
        <span className="whitespace-nowrap font-mono text-[13px] text-subtle tabular">/ {formatNumber(pool.planGi)} GiB</span>
        {over > 0 && (
          <FittingText texts={notes} className="ml-1 min-w-0 truncate whitespace-nowrap text-[11.5px] text-pending" />
        )}
      </div>
      <div className="mt-3 h-1 overflow-hidden rounded-full bg-surface-2">
        <div className={cn("h-full rounded-full", tone)} style={{ width: `${pool.planGi > 0 ? Math.min(pool.ceilingGi / pool.planGi, 1) * 100 : 0}%` }} />
      </div>
    </Card>
  );
}

export function QuotaTiles({ tiles, loading, pool }: { tiles: Tile[]; loading: boolean; pool?: PoolTile }) {
  return (
    <div className="grid grid-cols-2 gap-4 lg:grid-cols-4">
      {tiles.map((q) =>
        q.key === "memory" && pool && !loading ? (
          <MemoryPoolTile key={q.key} pool={pool} />
        ) : (
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
        ),
      )}
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
