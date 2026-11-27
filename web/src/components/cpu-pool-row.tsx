// The CPU pool's part of the Overviews' pool card: one row with how often
// the pool slowed its machines this hour, opened into a live strip of what
// each machine uses now while the pool holds them back, as
// docs/decisions/console-design.md ("Memory pools") says.

import { Link } from "@tanstack/react-router";
import { ChevronRight } from "lucide-react";
import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { StatusDot, phaseLabel } from "@/components/ui";
import { cn } from "@/lib/cn";
import type { CpuEntry, CpuOccupancy } from "@/lib/cpu-pool";
import { machinePath } from "@/lib/machines";
import { formatNumber } from "@/lib/quantity";

/** A piece of the strip: one machine; the smallest of the caller's together where a narrow strip has no room for each; or the other projects' machines, muted. */
type Segment = { key: string; e?: CpuEntry; group?: CpuEntry[]; cpu: number };

/** The key a machine has in both strips, so hovering it in one lights it in the other. */
export const machineKey = (e: { project: string; kind: string; name: string }) => `${e.project}/${e.kind}/${e.name}`;

/** As the memory strip's: the least width of a piece on a narrow screen, and the gap between two, in px. */
const minSegmentPx = 26;
const segmentGapPx = 2;

/** Below this many idle CPUs the pool reads as full, as memory's 1/8 GiB. */
const fullBelowCPU = 1 / 8;

const cardWidth = 240;

/** "1.6 CPUs", "1 CPU". */
export const cpus = (v: number) => `${formatNumber(+v.toFixed(1))} ${Math.abs(v - 1) < 0.05 ? "CPU" : "CPUs"}`;

/** The pieces, in order: the machines of project, or all the caller reads without one, by what they use now, largest first; then the rest together. A machine with no sample lately has no piece. */
export function cpuSegments(o: CpuOccupancy, project?: string): Segment[] {
  const own = (project ? o.entries.filter((e) => e.project === project) : o.entries).filter((e) => e.nowCPU !== undefined).sort((a, b) => b.nowCPU! - a.nowCPU!);
  const segments: Segment[] = own.map((e) => ({ key: machineKey(e), e, cpu: e.nowCPU! }));
  const rest = project ? o.entries.filter((e) => e.project !== project) : [];
  const cpu = (o.otherCPU ?? 0) + rest.reduce((sum, e) => sum + (e.nowCPU ?? 0), 0);
  if (cpu > 0) segments.push({ key: "other", cpu });
  return segments;
}

/** As the memory strip's fitSegments: past what a narrow track holds, the smallest of the caller's machines become one "+N" piece before the other projects'. */
export function fitCpuSegments(segments: Segment[], trackPx: number): Segment[] {
  if (trackPx <= 0) return segments;
  const fits = Math.max(Math.floor((trackPx - segmentGapPx) / (minSegmentPx + segmentGapPx)), 2);
  if (segments.length <= fits) return segments;
  const other = segments.filter((x) => !x.e);
  const own = segments.filter((x) => x.e);
  const keep = Math.max(fits - 1 - other.length, 0);
  const grouped = own.slice(keep);
  return [...own.slice(0, keep), { key: "more", group: grouped.map((x) => x.e!), cpu: grouped.reduce((sum, x) => sum + x.cpu, 0) }, ...other];
}

/** The hour's slowed share by minute as bars, oldest first: a grey baseline, amber bars for the minutes the pool slowed its machines. */
export function Sparkline({ minutes }: { minutes: (number | undefined)[] }) {
  const max = Math.max(0.25, ...minutes.map((v) => v ?? 0));
  const w = minutes.length * 3;
  return (
    <svg data-testid="cpu-sparkline" viewBox={`0 0 ${w} 14`} preserveAspectRatio="none" className="h-3.5 w-[150px] max-sm:w-[96px]" aria-hidden>
      <line x1={0} x2={w} y1={13.5} y2={13.5} className="stroke-border-strong" strokeWidth={1} vectorEffect="non-scaling-stroke" />
      {minutes.map((v, i) => {
        if (!v) return null;
        const h = Math.max(1.5, (v / max) * 13);
        return <rect key={i} data-minute={i} x={i * 3} y={14 - h} width={2.2} height={h} className="fill-pending" />;
      })}
    </svg>
  );
}

/** The row's head: "CPU", "not current" when the answer is not of now, the sparkline and the hour's share, amber while the pool slows its machines. */
function Head({ o, throttled }: { o: CpuOccupancy; throttled: boolean }) {
  const pct = o.slowed === undefined ? 0 : Math.round(o.slowed * 100);
  return (
    <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1">
      <span className="text-[12px] text-muted">
        CPU
        {!o.current && <span className="text-pending"> · not current</span>}
      </span>
      <span
        className={cn("flex items-center gap-2 text-[11px]", throttled ? "text-pending" : "text-subtle")}
        title={pct >= 1 ? `Your machines used all your plan's CPUs for ${pct}% of the last hour, so they ran slower.` : undefined}
      >
        <Sparkline minutes={o.minutes} />
        {pct >= 1 && <span className="tabular">slowed {pct}%</span>}
      </span>
    </div>
  );
}

/** A piece's card: the machine's name (a link when pinned), kind, project, state and its CPU now; or the group's, or the other projects' total. */
function SegmentCard({ s, pinned }: { s: Segment; pinned: boolean }) {
  const e = s.e;
  if (s.group) {
    return (
      <div className="text-[12.5px]">
        <div>
          <span className="font-medium">{s.group.length} more machines</span> · <span className="font-mono">{cpus(s.cpu)}</span> now
        </div>
        <div className="mt-1 line-clamp-3 text-[11.5px] text-muted">{s.group.map((g) => g.name).join(", ")}</div>
      </div>
    );
  }
  if (!e) {
    return (
      <div className="text-[12.5px]">
        <span className="font-medium text-muted">Other projects</span> · <span className="font-mono">{cpus(s.cpu)}</span> now
      </div>
    );
  }
  return (
    <div className="text-[12.5px]">
      <div className="flex items-baseline justify-between gap-3">
        {pinned ? (
          <Link to={machinePath(e.kind)} params={{ project: e.project, name: e.name }} className="flex min-w-0 items-center gap-0.5 font-medium text-accent">
            <span className="truncate">{e.name}</span>
            <ChevronRight className="size-3.5 shrink-0" />
          </Link>
        ) : (
          <span className="min-w-0 truncate font-medium">{e.name}</span>
        )}
        <span className="shrink-0 text-[11.5px] text-subtle">{e.kind === "Container" ? "Container" : "VM"}</span>
      </div>
      <div className="mt-0.5 flex items-center justify-between gap-3">
        <span className="min-w-0 truncate font-mono text-[11.5px] text-subtle">{e.project}</span>
        <span className="flex shrink-0 items-center gap-1.5 text-[12px] text-muted">
          <StatusDot phase={e.phase} />
          {phaseLabel(e.phase)}
        </span>
      </div>
      <div className="mt-2 border-t pt-2 tabular">
        <span className="font-mono">{cpus(s.cpu)}</span> now <span className="text-subtle">· </span>
        <span className="font-mono text-subtle">{e.vcpus}</span>
        <span className="text-subtle"> vCPU</span>
      </div>
    </div>
  );
}

function segmentLabel(s: Segment): string {
  if (s.group) return `${s.group.length} more machines, ${cpus(s.cpu)} now`;
  if (!s.e) return `Other projects, ${cpus(s.cpu)} now`;
  return `${s.e.name}, ${cpus(s.cpu)} now of ${s.e.vcpus} vCPU`;
}

/** Whether the screen is narrow (under Tailwind's sm), as the memory strip judges it. */
function useNarrow(): boolean {
  const query = "(max-width: 639px)";
  const media = typeof window !== "undefined" && typeof window.matchMedia === "function" ? window.matchMedia(query) : undefined;
  const [narrow, setNarrow] = useState(media?.matches ?? false);
  useEffect(() => {
    if (!media) return;
    const change = () => setNarrow(media.matches);
    media.addEventListener("change", change);
    return () => media.removeEventListener("change", change);
  }, [media]);
  return narrow;
}

/**
 * The live strip: scaled to the plan's CPUs; each running machine as long as
 * what it uses now, largest first, in one mid tone, there being nothing
 * reserved; idle CPUs the bare track. Hover, focus, click and tap as on the
 * memory strip; linked, the machine hovered in the memory strip, lights here
 * too, and onOpen says which this one opens.
 */
function CpuStrip({
  o,
  head,
  planCPU,
  project,
  linked,
  onOpen,
}: {
  o: CpuOccupancy;
  head: React.ReactNode;
  planCPU: number;
  project?: string;
  linked?: string | null;
  onOpen?: (key: string | null) => void;
}) {
  const narrow = useNarrow();
  const track = useRef<HTMLDivElement>(null);
  const [trackPx, setTrackPx] = useState(0);
  useLayoutEffect(() => {
    const el = track.current;
    if (!el) return;
    setTrackPx(el.clientWidth);
    if (typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(() => setTrackPx(el.clientWidth));
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  const all = cpuSegments(o, project);
  const segments = narrow ? fitCpuSegments(all, trackPx) : all;
  const [open, setOpen] = useState<string | null>(null);
  const [pinned, setPinned] = useState(false);
  const [place, setPlace] = useState({ left: 0, top: 0 });
  const pointer = useRef("mouse");
  const root = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!pinned) return;
    const close = (ev: PointerEvent) => {
      if (root.current && ev.target instanceof Node && root.current.contains(ev.target)) return;
      setPinned(false);
      setOpen(null);
    };
    document.addEventListener("pointerdown", close, true);
    return () => document.removeEventListener("pointerdown", close, true);
  }, [pinned]);

  const openSegment = segments.find((s) => s.key === open);
  useLayoutEffect(() => {
    const r = root.current;
    const el = open ? r?.querySelector<HTMLElement>(`[data-cpu-segment="${CSS.escape(open)}"]`) : null;
    if (!r || !el || !track.current) return;
    const middle = el.offsetLeft + el.offsetWidth / 2;
    setPlace({ left: Math.max(0, Math.min(middle - cardWidth / 2, r.clientWidth - cardWidth)), top: track.current.offsetTop + track.current.offsetHeight + 8 });
  }, [open, segments.length, trackPx]);

  const used = segments.reduce((sum, s) => sum + s.cpu, 0);
  const idle = Math.max(0, planCPU - used);
  const full = idle < fullBelowCPU;
  // What lights: the piece open here; else the machine open in the memory strip, where it has a piece here.
  const lit = openSegment ? openSegment.key : linked && segments.some((s) => s.key === linked) ? linked : null;
  // The memory strip is told the machine open here: a machine's alone, as
  // "more" and "other" hold different machines in each strip; none once it
  // is gone, or this strip is, as when the row closes.
  const openMachine = openSegment?.e ? openSegment.key : null;
  useEffect(() => onOpen?.(openMachine), [openMachine, onOpen]);
  useEffect(() => () => onOpen?.(null), [onOpen]);

  return (
    // The head is in the box the card is placed from, so on a narrow screen
    // the card opens wholly above it and the strip, as memory's does.
    <div ref={root} className="relative">
      {head}
      <div className={cn(!o.current && "opacity-50")}>
        <div
          ref={track}
          data-testid="cpu-track"
          data-full={full || undefined}
          className={cn("mt-2 flex h-6 gap-[2px] overflow-hidden rounded-[3px] bg-surface-2 p-[2px]", full ? "ring-2 ring-foreground/80" : "ring-1 ring-border-strong")}
        >
          {segments.map((s) => {
            const className = cn(
              "@container flex min-w-0 cursor-pointer items-center overflow-hidden rounded-[2px] px-1 text-[10.5px] outline-none transition-opacity focus-visible:ring-2 focus-visible:ring-accent max-sm:min-w-[26px]",
              // A mid tone: CPU keeps nothing reserved, so it is neither memory's reserved fill nor its shared one.
              s.e ? "bg-foreground/45 text-surface" : s.group ? "bg-foreground/35 text-surface" : "bg-foreground/15",
              lit !== null && lit !== s.key && "opacity-35",
            );
            const common = {
              "aria-label": segmentLabel(s),
              "data-cpu-segment": s.key,
              className,
              style: { flex: `${s.cpu} 1 0` },
              onPointerEnter: (ev: React.PointerEvent) => ev.pointerType === "mouse" && !pinned && setOpen(s.key),
              onPointerLeave: (ev: React.PointerEvent) => ev.pointerType === "mouse" && !pinned && setOpen(null),
              onPointerDown: (ev: React.PointerEvent) => {
                pointer.current = ev.pointerType;
              },
              onFocus: () => !pinned && setOpen(s.key),
              onBlur: () => !pinned && setOpen(null),
              onClick: (ev: React.MouseEvent) => {
                // A touch pins the card; a mouse, or a key, goes on.
                if (ev.detail !== 0 && pointer.current !== "mouse") {
                  ev.preventDefault();
                  setOpen(s.key);
                  setPinned(true);
                }
              },
            };
            const label = s.e ? (
              <span className="hidden min-w-0 truncate leading-[14px] @[30px]:inline">{s.e.name}</span>
            ) : s.group ? (
              <span className="w-full truncate text-center leading-[14px]">+{s.group.length}</span>
            ) : null;
            return s.e ? (
              <Link key={s.key} to={machinePath(s.e.kind)} params={{ project: s.e.project, name: s.e.name }} {...common}>
                {label}
              </Link>
            ) : (
              <button key={s.key} type="button" {...common}>
                {label}
              </button>
            );
          })}
          {idle > 0 && <span className="min-w-0" style={{ flex: `${idle} 1 0` }} />}
        </div>
        <div className="mt-1 flex justify-between font-mono text-[10.5px] text-subtle tabular">
          {/* What is in use, until it is all of it: then only the pool's CPUs, in ink. */}
          <span>{!full && cpus(used)}</span>
          <span className={cn(full && "font-medium text-foreground")}>{cpus(planCPU)}</span>
        </div>
      </div>
      {openSegment && (
        <div
          role="tooltip"
          className="absolute top-[var(--pool-card-top)] z-20 rounded-lg border bg-surface px-3 py-2.5 shadow-lg max-sm:top-auto max-sm:bottom-[calc(100%+16px)]"
          style={{ width: cardWidth, left: place.left, ["--pool-card-top" as string]: `${place.top}px` }}
          onPointerEnter={(ev) => ev.pointerType === "mouse" && setOpen(openSegment.key)}
          onPointerLeave={(ev) => ev.pointerType === "mouse" && !pinned && setOpen(null)}
        >
          <SegmentCard s={openSegment} pinned={pinned} />
        </div>
      )}
    </div>
  );
}

/**
 * The pool card's CPU row: calm, the head alone; throttled, the head over
 * the live strip. Not current, the row is dimmed and its head says so.
 */
export function CpuPoolRow({
  o,
  throttled,
  planCPU,
  project,
  linked,
  onOpen,
  divided,
}: {
  o: CpuOccupancy;
  throttled: boolean;
  planCPU: number;
  project?: string;
  linked?: string | null;
  onOpen?: (key: string | null) => void;
  /** Under the memory strip: a rule above it. */
  divided?: boolean;
}) {
  // Not current, the head and the strip are dimmed, not a machine's card.
  const head = (
    <div className={cn(!o.current && "opacity-50")} data-current={o.current || undefined}>
      <Head o={o} throttled={throttled} />
    </div>
  );
  return (
    // Alone on its card, the card is labelled "CPU pool" already.
    <section aria-label={divided ? "CPU pool" : undefined} data-throttled={throttled || undefined} className={cn(divided && (throttled ? "mt-4" : "mt-3 border-t pt-2.5"))}>
      {throttled ? <CpuStrip o={o} head={head} planCPU={planCPU} project={project} linked={linked} onOpen={onOpen} /> : head}
    </section>
  );
}

/** The calm row's shape, unseen, holding its place while the first answer is pending. */
export function CpuPoolRowPlaceholder({ divided }: { divided?: boolean }) {
  return (
    <div aria-hidden data-testid="cpu-row-placeholder" className={cn(divided && "mt-3 border-t pt-2.5")}>
      <div className="invisible">
        <Head o={{ entries: [], minutes: [], current: true }} throttled={false} />
      </div>
    </div>
  );
}
