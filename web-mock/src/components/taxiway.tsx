// Taxi lines: the line an aircraft follows to where it is going. The console
// uses them only to guide and to show progress, never as decoration: a solid
// accent line for the way already covered, a dashed one for the way ahead.

import { HangarMark } from "@/components/marks";
import { cn } from "@/lib/cn";

export type StopState = "done" | "current" | "todo" | "failed" | "idle";

const dashedH = "bg-[repeating-linear-gradient(to_right,var(--accent)_0_6px,transparent_6px_12px)] opacity-50";
export type Stop = { label: string; state: StopState; note?: string };

/**
 * A machine's way from scheduled to running, left to right. With `gather`,
 * every stop but the last slides into the last one, nearest first: the way
 * behind is pulled in once it has been travelled.
 */
export function Taxiway({ stops, gather = false }: { stops: Stop[]; gather?: boolean }) {
  const n = stops.length;
  return (
    // Clipped, so stops pulled into the last one never show past it.
    <ol className="flex items-start overflow-hidden">
      {stops.map((s, i) => {
        const next = stops[i + 1];
        const covered = next && (next.state === "done" || next.state === "current" || next.state === "failed" || next.state === "idle");
        // The stops before the last are equal flex items, so the last stop is
        // (n - 1 - i) of their widths to the right.
        const pull =
          i < n - 1
            ? {
                transform: gather ? `translateX(${(n - 1 - i) * 100}%)` : undefined,
                opacity: gather ? 0 : 1,
                transition: "transform 520ms cubic-bezier(.6,0,.3,1), opacity 420ms ease-in",
                transitionDelay: gather ? `${(n - 2 - i) * 70}ms` : "0ms",
              }
            : undefined;
        return (
          <li
            key={s.label}
            style={pull}
            className={cn("relative flex min-w-0 flex-col", i < n - 1 ? "flex-1" : "z-10 shrink-0 bg-surface")}
          >
            <div className="flex items-center">
              <span
                className={cn(
                  "relative z-10 flex size-3 shrink-0 items-center justify-center rounded-full",
                  s.state === "done" && "bg-accent",
                  s.state === "current" && "bg-pending",
                  s.state === "failed" && "bg-fail",
                  s.state === "idle" && "bg-off",
                  s.state === "todo" && "border-2 border-dashed border-accent/60 bg-background",
                )}
              >
                {(s.state === "current" || s.state === "failed") && (
                  <span className={cn("absolute inset-0 animate-ping rounded-full opacity-60", s.state === "current" ? "bg-pending" : "bg-fail")} />
                )}
              </span>
              {next && (
                <span
                  className={cn(
                    "mx-1.5 h-0.5 flex-1",
                    covered ? (next.state === "idle" ? "bg-off" : "bg-accent") : dashedH,
                  )}
                />
              )}
            </div>
            <div className={cn("mt-2 pr-3 text-[12.5px]", s.state === "todo" ? "text-subtle" : "font-medium")}>{s.label}</div>
            {s.note && (
              <div
                className={cn(
                  "truncate pr-3 font-mono text-[11.5px]",
                  s.state === "failed" ? "text-fail" : s.state === "current" ? "text-pending" : "text-muted",
                )}
              >
                {s.note}
              </div>
            )}
          </li>
        );
      })}
    </ol>
  );
}

/**
 * An apron seen from where a pilot sits: the centre line runs, in
 * perspective, into the hangar's door. For an empty list.
 */
export function ApronArt({ className }: { className?: string }) {
  const W = 560;
  const H = 220;
  const cx = W / 2;
  const door = 92; // y of the hangar's floor, the vanishing line
  const dashes: string[] = [];
  // Depth z = 1 is the bottom edge; the line recedes as 1/z.
  const y = (z: number) => door + (H - door) / z;
  const half = (z: number) => 7 / z;
  for (let z = 1; z < 14; z *= 1.55) {
    const z2 = z * 1.28;
    dashes.push(
      `M${cx - half(z)} ${y(z)}L${cx + half(z)} ${y(z)}L${cx + half(z2)} ${y(z2)}L${cx - half(z2)} ${y(z2)}Z`,
    );
  }
  const edge = (side: -1 | 1) => `M${cx + side * 210} ${H}L${cx + side * 46} ${door}`;
  return (
    <svg viewBox={`0 0 ${W} ${H}`} className={className} aria-hidden>
      <path d={`M0 ${door}H${W}`} stroke="var(--art-faint)" strokeWidth="1" vectorEffect="non-scaling-stroke" />
      <path d={edge(-1)} stroke="var(--art-faint)" strokeWidth="1" vectorEffect="non-scaling-stroke" />
      <path d={edge(1)} stroke="var(--art-faint)" strokeWidth="1" vectorEffect="non-scaling-stroke" />
      {/* The hangar on the horizon is the mark itself, its floor on the line. */}
      <g transform={`translate(${cx - 72} ${door - 120}) scale(6)`} className="text-[var(--art-line)]">
        <HangarMark bare />
      </g>
      {dashes.map((d) => (
        <path key={d} d={d} fill="var(--accent)" />
      ))}
    </svg>
  );
}
