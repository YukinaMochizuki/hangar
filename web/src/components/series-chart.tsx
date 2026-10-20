// A time series drawn as inline SVG: the console loads no chart library,
// and an SVG's attributes pass the Content-Security-Policy's style-src.
// Text stays in HTML around the plot, so stretching the plot to its card's
// width never stretches a label.

import type { ReactNode } from "react";
import { Card, CardHeader } from "@/components/ui";
import { axisTime, formatValue, rangeSeconds, type Series, type SeriesRange } from "@/lib/monitoring";

const W = 300;
const H = 100;

/**
 * The axis's top: the series' capacity when it has one, the whole for a
 * share of the CPU or one whose alert is high, else a round number above
 * its highest point.
 */
export function axisMax(s: Series): number {
  if (s.capacity) return s.capacity;
  if (s.name === "HOST_CPU" || (s.unit === "ratio" && (s.alertAbove ?? 0) >= 0.5)) return 1;
  const top = Math.max(...s.points.map((p) => p.value), 0) * 1.2;
  if (top <= 0) return s.unit === "ratio" ? 0.01 : 1;
  const mag = 10 ** Math.floor(Math.log10(top));
  const step = [1, 2, 2.5, 5, 10].find((m) => m * mag >= top) ?? 10;
  return step * mag;
}

/**
 * The line and the area under it, placed by time over the range ending
 * now. A step with no point breaks the line, as the API leaves it out.
 */
export function plotPaths(s: Series, range: SeriesRange, max: number, now: Date): { line: string; area: string } {
  const span = rangeSeconds[range] * 1000;
  const start = now.getTime() - span;
  const gap = Math.max(s.stepSeconds, 1) * 1000 * 1.5;
  const runs: string[][] = [];
  let prev: number | undefined;
  for (const p of s.points) {
    const t = p.time.getTime();
    const x = Math.min(Math.max(((t - start) / span) * W, 0), W);
    const y = H - Math.min(Math.max(p.value / max, 0), 1) * H;
    if (prev === undefined || t - prev > gap) runs.push([]);
    runs[runs.length - 1].push(`${x.toFixed(2)},${y.toFixed(2)}`);
    prev = t;
  }
  const line = runs.map((r) => `M${r.join("L")}`).join("");
  const area = runs.map((r) => `M${r[0].split(",")[0]},${H}L${r.join("L")}L${r[r.length - 1].split(",")[0]},${H}Z`).join("");
  return { line, area };
}

function Frame({ title, legend, value, children }: { title: string; legend?: ReactNode; value: ReactNode; children: ReactNode }) {
  return (
    <Card>
      <CardHeader
        className="h-9"
        title={
          <span className="flex min-w-0 items-center gap-2 whitespace-nowrap">
            {title}
            {legend}
          </span>
        }
        action={<span className="whitespace-nowrap font-mono text-[12.5px] tabular">{value}</span>}
      />
      <div className="px-3 pb-2 pt-2.5">{children}</div>
    </Card>
  );
}

/** A chart's place while its series loads or when it failed: as tall as the chart, so the grid does not move. */
export function ChartPlaceholder({ title, children }: { title: string; children: ReactNode }) {
  return (
    <Frame title={title} value="">
      <div className="flex h-[100px] items-center justify-center px-2 text-center text-[12px] text-subtle">{children}</div>
    </Frame>
  );
}

export function SeriesChart({ title, series, range, now = new Date() }: { title: string; series: Series; range: SeriesRange; now?: Date }) {
  const max = axisMax(series);
  const { line, area } = plotPaths(series, range, max, now);
  const last = series.points.at(-1);
  const start = new Date(now.getTime() - rangeSeconds[range] * 1000);
  const mid = new Date(now.getTime() - (rangeSeconds[range] * 1000) / 2);
  const alertY = series.alertAbove !== undefined && series.alertAbove <= max ? H - (series.alertAbove / max) * H : undefined;
  const fmt = (v: number) => formatValue(v, series.unit);
  const legend = series.alertAbove !== undefined && (
    <span className="flex min-w-0 items-center gap-1 truncate text-[11px] font-normal text-subtle">
      <svg viewBox="0 0 12 2" className="h-0.5 w-3" aria-hidden>
        <line x1={0} x2={12} y1={1} y2={1} className="stroke-pending" strokeWidth={2} strokeDasharray="3 2" />
      </svg>
      alert at {fmt(series.alertAbove)}
    </span>
  );

  return (
    <Frame
      title={title}
      legend={legend}
      value={last ? <span title={series.capacity !== undefined ? `of ${fmt(series.capacity)}` : undefined}>{fmt(last.value)}</span> : <span className="text-subtle">no data</span>}
    >
      <div className="flex gap-2">
        <div className="flex h-20 w-14 shrink-0 flex-col justify-between text-right font-mono text-[10.5px] leading-none text-subtle tabular">
          {/* An axis with nothing on it is not labelled. */}
          <span>{last && fmt(max)}</span>
          <span>{last && fmt(max / 2)}</span>
          <span>{last && "0"}</span>
        </div>
        <div className="relative h-20 min-w-0 flex-1">
          <svg viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" className="size-full overflow-visible" role="img" aria-label={`${title}, ${range}`}>
            {[0, H / 2, H].map((y) => (
              <line key={y} x1={0} x2={W} y1={y} y2={y} className="stroke-border" vectorEffect="non-scaling-stroke" />
            ))}
            <path d={area} className="fill-foreground/[0.06]" />
            <path d={line} fill="none" className="stroke-foreground/80" strokeWidth={1.5} strokeLinejoin="round" vectorEffect="non-scaling-stroke" />
            {alertY !== undefined && (
              <line x1={0} x2={W} y1={alertY} y2={alertY} className="stroke-pending" strokeDasharray="4 3" vectorEffect="non-scaling-stroke" />
            )}
          </svg>
          {!last && (
            <span className="absolute inset-0 flex items-center justify-center">
              <span className="bg-surface px-1.5 text-[12px] text-subtle">No data in this range</span>
            </span>
          )}
        </div>
      </div>
      <div className="ml-16 mt-1 flex justify-between font-mono text-[10.5px] text-subtle tabular">
        <span>{axisTime(start, range)}</span>
        <span>{axisTime(mid, range)}</span>
        <span>now</span>
      </div>
    </Frame>
  );
}
