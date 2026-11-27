// A machine's Usage: its CPU and memory over the last hour, as its node
// samples them (docs/decisions/machine-usage.md). A container's memory is
// what its guest uses; a virtual machine's is what it holds on the host.

import type { ReactNode } from "react";
import { SeriesPlot, SeriesValue, type ChartSeries } from "@/components/series-chart";
import { Card, CardEmpty, CardHeader } from "@/components/ui";
import { formatNumber } from "@/lib/quantity";

/** What the card draws: the samples' series and which memory they are. */
export type Usage = {
  cpu: ChartSeries;
  memory: ChartSeries;
  /** "guest" for a container's guest's own count, "host" for what a VM holds on the host. */
  memoryKind: "guest" | "host";
  /** What its memory pool leaves the guest now, its memory less its balloon; unset when its node cannot tell. */
  sizeNowBytes?: number;
  /**
   * Its tenant's CPU pool, when it is in one: the plan's CPUs it shares,
   * and the share, 0 to 1, of the last hour they held it back, when known.
   */
  cpuPool?: { cpus: number; slowed?: number };
};

/** The line under a pooled machine's CPU plot: what it shares, and how often that slowed it. */
export function cpuPoolLine(pool: { cpus: number; slowed?: number }): string {
  const shares = `Shares your plan's ${formatNumber(pool.cpus)} CPUs with your other machines`;
  const pct = pool.slowed === undefined ? 0 : Math.round(pool.slowed * 100);
  return pct >= 1 ? `${shares} · slowed ${pct}% in the last hour` : `${shares}.`;
}

/** The memory's name, by what it counts. */
export function memoryTitle(kind: Usage["memoryKind"]): string {
  return kind === "guest" ? "Memory used" : "Host memory";
}

function Frame({ children }: { children: ReactNode }) {
  return (
    <Card>
      <CardHeader title="Usage" action={<span className="text-[12px] text-subtle">last hour</span>} />
      <div className="divide-y">{children}</div>
    </Card>
  );
}

function Section({ title, value, children }: { title: string; value?: ReactNode; children: ReactNode }) {
  return (
    <section className="px-4 pb-3 pt-3">
      <div className="flex items-baseline justify-between text-[13px]">
        <span className="text-muted">{title}</span>
        <span className="whitespace-nowrap font-mono text-[12.5px] tabular">{value}</span>
      </div>
      {children}
    </section>
  );
}

/**
 * The card with the hour's CPU and memory, each with its value now. A Shared
 * machine's (`shared`) memory also shows what its pool leaves it now, a
 * dashed line on the plot (docs/decisions/memory-pool.md).
 */
export function UsageCard({ usage, now, shared }: { usage: Usage; now?: Date; shared?: boolean }) {
  const memory = memoryTitle(usage.memoryKind);
  const sizeNow = shared ? usage.sizeNowBytes : undefined;
  const sizeNowGi = sizeNow === undefined ? undefined : formatNumber(sizeNow / 2 ** 30);
  return (
    <Frame>
      <Section title="CPU" value={<SeriesValue series={usage.cpu} />}>
        <SeriesPlot title="CPU" series={usage.cpu} range="1h" now={now} axis="inside" />
        {usage.cpuPool && <div className="mt-1.5 text-[11.5px] text-subtle">{cpuPoolLine(usage.cpuPool)}</div>}
      </Section>
      <Section
        title={memory}
        value={
          <>
            <SeriesValue series={usage.memory} />
            {sizeNowGi !== undefined && <span className="text-subtle"> of {sizeNowGi} GiB</span>}
          </>
        }
      >
        <SeriesPlot title={memory} series={usage.memory} range="1h" now={now} axis="inside" mark={sizeNow} />
        {sizeNowGi !== undefined && (
          <div className="mt-1.5 flex items-center gap-1.5 text-[11.5px] text-subtle">
            <svg viewBox="0 0 12 2" className="h-0.5 w-3" aria-hidden>
              <line x1={0} x2={12} y1={1} y2={1} className="stroke-pending" strokeWidth={2} strokeDasharray="3 2" />
            </svg>
            available now, {sizeNowGi} GiB
          </div>
        )}
      </Section>
    </Frame>
  );
}

/** The card saying, in one line, why it has no samples to show, as for a machine not running. */
export function UsageNote({ children }: { children: ReactNode }) {
  return (
    <Frame>
      <CardEmpty>{children}</CardEmpty>
    </Frame>
  );
}

/** The card while its samples load or when they failed: as tall as the plots, so the column does not move. */
export function UsagePlaceholder({ memoryKind, children }: { memoryKind: Usage["memoryKind"]; children: ReactNode }) {
  return (
    <Frame>
      {["CPU", memoryTitle(memoryKind)].map((t) => (
        <Section key={t} title={t}>
          <div className="mt-2 flex h-[113px] items-center justify-center px-2 text-center text-[12px] text-subtle">{children}</div>
        </Section>
      ))}
    </Frame>
  );
}
