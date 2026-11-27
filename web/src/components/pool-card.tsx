// The Overviews' pool card: who uses the tenant's memory pool now, and its
// CPU pool's row (components/cpu-pool-row.tsx), as
// docs/decisions/console-design.md ("Memory pools") says.

import { Link } from "@tanstack/react-router";
import { Ban, ChevronRight, TriangleAlert } from "lucide-react";
import { useEffect, useLayoutEffect, useRef, useState, type CSSProperties, type ReactNode } from "react";
import { CpuPoolRow, CpuPoolRowPlaceholder } from "@/components/cpu-pool-row";
import { FittingText } from "@/components/fitting-text";
import { KilledIcon, StuckIcon } from "@/components/machine-memory";
import { Card, StatusDot, phaseLabel } from "@/components/ui";
import { cn } from "@/lib/cn";
import { useCpuPoolRead } from "@/lib/cpu-pool";
import { ago, machinePath } from "@/lib/machines";
import { usePoolRead, type PoolEntry, type PoolOccupancy } from "@/lib/memory-pool";
import { formatNumber } from "@/lib/quantity";

/**
 * A segment: one machine; the smallest of the caller's machines together
 * where a narrow strip has no room for each (group); or other projects'
 * machines together, muted and unnamed. Its sizes in GiB.
 */
type Segment = { key: string; e?: PoolEntry; group?: PoolEntry[]; reservedGi: number; lengthGi: number };

/** The least width of a segment on a narrow screen, in px, and the gap between two. */
const minSegmentPx = 26;
const segmentGapPx = 2;

/**
 * The segments as a strip of trackPx holds them on a narrow screen, where
 * each is at least minSegmentPx wide: past what fits, the smallest of the
 * caller's machines, at the end of its own, become one "+N" piece before
 * the other projects'. trackPx zero, unknown, groups nothing.
 */
export function fitSegments(segments: Segment[], trackPx: number): Segment[] {
  if (trackPx <= 0) return segments;
  // The track's padding is a gap wide on each side.
  const fits = Math.max(Math.floor((trackPx - segmentGapPx) / (minSegmentPx + segmentGapPx)), 2);
  if (segments.length <= fits) return segments;
  const other = segments.filter((x) => !x.e);
  const own = segments.filter((x) => x.e);
  const keep = Math.max(fits - 1 - other.length, 0);
  const grouped = own.slice(keep);
  const group: Segment = {
    key: "more",
    group: grouped.map((x) => x.e!),
    reservedGi: grouped.reduce((sum, x) => sum + x.reservedGi, 0),
    lengthGi: grouped.reduce((sum, x) => sum + x.lengthGi, 0),
  };
  return [...own.slice(0, keep), group, ...other];
}

/** Below this much free, in GiB, the pool reads as full. */
const fullBelowGi = 1 / 8;

/** The width of a machine's card, in px. */
const cardWidth = 260;

const gib = (v: number) => `${formatNumber(+v.toFixed(2))} GiB`;

/**
 * What a machine's segment is as long as: what its guest uses; what it has
 * when its guest does not say; its reservation when its node cannot tell
 * either.
 */
const lengthOf = (e: PoolEntry) => e.inUseGi ?? e.nowGi ?? e.reservedGi;

const dedicated = (e: PoolEntry) => e.reservedGi >= e.memoryGi;

/** Dedicated machines first, then Shared ones by what they use, largest first. */
function byPoolOrder(a: PoolEntry, b: PoolEntry): number {
  return Number(!dedicated(a)) - Number(!dedicated(b)) || lengthOf(b) - lengthOf(a);
}

/**
 * The card's segments, in order: the machines of project, or all the caller
 * reads without one, then the rest together, with what the API summed.
 */
export function poolSegments(o: PoolOccupancy, project?: string): Segment[] {
  const own = (project ? o.entries.filter((e) => e.project === project) : o.entries).slice().sort(byPoolOrder);
  const segments: Segment[] = own.map((e) => ({ key: `${e.project}/${e.kind}/${e.name}`, e, reservedGi: Math.min(e.reservedGi, lengthOf(e)), lengthGi: lengthOf(e) }));
  const rest = project ? o.entries.filter((e) => e.project !== project) : [];
  let reservedGi = o.other ? Math.min(o.other.reservedGi, o.other.inUseGi) : 0;
  let lengthGi = o.other?.inUseGi ?? 0;
  for (const e of rest) {
    reservedGi += Math.min(e.reservedGi, lengthOf(e));
    lengthGi += lengthOf(e);
  }
  if (lengthGi > 0) segments.push({ key: "other", reservedGi, lengthGi });
  return segments;
}

type Mark = "killed" | "stuck";

const markOf = (e?: PoolEntry): Mark | undefined => (e?.killedAt ? "killed" : e?.stuckSince ? "stuck" : undefined);

/** The state colour a marked segment's fills take a little of. */
const markColour = (mark: Mark) => (mark === "killed" ? "var(--fail)" : "var(--pending)");

/**
 * A segment's label: its name, after the mark's glyph when it has one, the
 * glyph lightened on the dark reserved fill. Where the segment is too narrow
 * for the name the glyph stands alone in its middle; nothing overflows it.
 */
function SegmentLabel({ name, mark, onDark }: { name: string; mark?: Mark; onDark: boolean }) {
  const ink = mark ? (onDark ? `color-mix(in oklab, ${markColour(mark)} 55%, var(--surface))` : markColour(mark)) : undefined;
  const Glyph = mark === "killed" ? Ban : TriangleAlert;
  return (
    <span className="flex w-full min-w-0 items-center justify-center gap-[3px] leading-none @[40px]:justify-start">
      {mark && <Glyph aria-hidden data-mark={mark} className="size-[11px] shrink-0" strokeWidth={2.5} style={{ color: ink }} />}
      <span className={cn("min-w-0 truncate leading-[14px]", mark ? "hidden @[40px]:inline" : "max-w-full")}>{name}</span>
    </span>
  );
}

function Swatch({ kind }: { kind: "reserved" | "shared" | "free" }) {
  return (
    <span
      aria-hidden
      className={cn("inline-block h-2 w-3 rounded-[2px]", kind === "reserved" ? "bg-foreground/75" : kind === "shared" ? "bg-foreground/30" : "border border-border-strong bg-surface-2")}
    />
  );
}

/** "1 GiB" in the mono figures of the card, the words around it plain. */
function figures(text: string): ReactNode {
  return text.split(/(\d+(?:\.\d+)?(?: GiB)?)/).map((part, i) =>
    i % 2 === 1 ? (
      <span key={i} className="font-mono">
        {part}
      </span>
    ) : (
      part
    ),
  );
}

/**
 * What a machine's card says of its memory, below its name: what its guest
 * uses, what it holds and its range, in the longest words that fit; and,
 * when its segment is drawn from what it holds or from its reservation, so.
 */
function MemoryLine({ e }: { e: PoolEntry }) {
  const range = dedicated(e) ? `${gib(e.memoryGi)} Dedicated` : `${formatNumber(+e.reservedGi.toFixed(2))}–${gib(e.memoryGi)}`;
  // A Dedicated machine holds all of its memory: its range says so.
  const holds = e.nowGi !== undefined && !dedicated(e) ? `holds ${gib(e.nowGi)}` : undefined;
  const n = (v: number) => formatNumber(+v.toFixed(2));
  let texts: string[];
  let from: string | undefined;
  if (e.inUseGi !== undefined) {
    const use = `${gib(e.inUseGi)} in use`;
    // Shorter, the units go but the range's, then the range, then what it holds.
    texts = holds
      ? [`${use} · ${holds} · ${range}`, `${n(e.inUseGi)} in use · holds ${n(e.nowGi ?? 0)} · ${range}`, `${use} · ${holds}`, use]
      : [`${use} · ${range}`, use];
  } else if (e.nowGi !== undefined) {
    texts = holds ? [`${holds} · ${range}`, holds] : [range];
    from = "Size from what it holds";
  } else {
    texts = [range];
    from = "Size from its reservation";
  }
  return (
    <>
      <div className="flex">
        <FittingText texts={texts} render={figures} className="min-w-0 truncate whitespace-nowrap" />
      </div>
      {from && <div className="mt-0.5 text-[11.5px] text-subtle">{from}</div>}
    </>
  );
}

/** A segment's card: the machine's name (a link when pinned), kind, project, state, memory and problem; or the other projects' total. */
function SegmentCard({ s, pinned }: { s: Segment; pinned: boolean }) {
  const e = s.e;
  if (s.group) {
    return (
      <div className="text-[12.5px]">
        <div>
          <span className="font-medium">{s.group.length} more machines</span> · {figures(`${gib(s.lengthGi)} in use`)}
        </div>
        <div className="mt-1 line-clamp-3 text-[11.5px] text-muted">{s.group.map((g) => g.name).join(", ")}</div>
      </div>
    );
  }
  if (!e) {
    return (
      <div className="text-[12.5px]">
        <span className="font-medium text-muted">Other projects</span> · {figures(`${gib(s.lengthGi)} in use`)}
      </div>
    );
  }
  const name = pinned ? (
    <Link to={machinePath(e.kind)} params={{ project: e.project, name: e.name }} className="flex min-w-0 items-center gap-0.5 font-medium text-accent">
      <span className="truncate">{e.name}</span>
      <ChevronRight className="size-3.5 shrink-0" />
    </Link>
  ) : (
    <span className="min-w-0 truncate font-medium">{e.name}</span>
  );
  return (
    <div className="text-[12.5px]">
      <div className="flex items-baseline justify-between gap-3">
        {name}
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
        <MemoryLine e={e} />
      </div>
      {e.killedAt && (
        <div className="mt-1.5 flex items-center gap-1 text-[12px] text-fail">
          <KilledIcon />
          Stopped for memory {ago(e.killedAt)} ago
        </div>
      )}
      {e.stuckSince && (
        <div className="mt-1.5 flex items-center gap-1 text-[12px] text-pending">
          <StuckIcon />
          Not sharing memory
        </div>
      )}
    </div>
  );
}

/** What a segment is called to a screen reader: the machine and its state, or the other projects. */
function segmentLabel(s: Segment): string {
  if (s.group) return `${s.group.length} more machines, ${gib(s.lengthGi)} in use`;
  if (!s.e) return `Other projects, ${gib(s.lengthGi)} in use`;
  const state = s.e.killedAt ? ", stopped for memory" : s.e.stuckSince ? ", not sharing memory" : "";
  return `${s.e.name}${state}${s.e.inUseGi !== undefined ? `, ${gib(s.e.inUseGi)} in use` : ""}`;
}

/** The fills of a segment's reserved and shared parts: a marked one's take a little of its state's colour. */
function fills(s: Segment): { reserved?: CSSProperties; shared?: CSSProperties; reservedClass: string; sharedClass: string } {
  const mark = markOf(s.e);
  if (s.group) return { reservedClass: "bg-foreground/60", sharedClass: "bg-foreground/25" };
  if (!s.e) return { reservedClass: "bg-foreground/25", sharedClass: "bg-foreground/10" };
  if (!mark) return { reservedClass: "bg-foreground/75", sharedClass: "bg-foreground/30" };
  const c = markColour(mark);
  return {
    reserved: { background: `color-mix(in oklab, color-mix(in oklab, var(--foreground) 62%, ${c}) 80%, transparent)` },
    shared: { background: `color-mix(in oklab, color-mix(in oklab, var(--foreground) 45%, ${c}) 32%, transparent)` },
    reservedClass: "",
    sharedClass: "",
  };
}

/** Whether the screen is narrow (under Tailwind's sm), where segments keep a least width and the pinned card opens above. */
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

/** The card's first line: "Memory", "not current" when the answer is not of now, and the legend. */
function Legend({ current }: { current: boolean }) {
  return (
    <div className="mb-2 flex flex-wrap items-center justify-between gap-x-4 gap-y-1">
      <span className="text-[12px] text-muted">
        Memory
        {!current && <span className="text-pending"> · not current</span>}
      </span>
      <span className="flex items-center gap-3 text-[11px] text-subtle">
        <span className="flex items-center gap-1.5">
          <Swatch kind="reserved" />
          reserved
        </span>
        <span className="flex items-center gap-1.5">
          <Swatch kind="shared" />
          shared
        </span>
        <span className="flex items-center gap-1.5">
          <Swatch kind="free" />
          free
        </span>
      </span>
    </div>
  );
}

/**
 * The legend, the strip and its card, for a pool's occupancy; project set,
 * that project's machines named and the rest muted. Not current, the strip
 * is dimmed and the legend says so. linked, the machine open in the CPU
 * strip, lights here too; onOpen says which this one opens.
 */
export function PoolStrip({
  occupancy,
  project,
  linked,
  onOpen,
}: {
  occupancy: PoolOccupancy;
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
  const all = poolSegments(occupancy, project);
  const segments = narrow ? fitSegments(all, trackPx) : all;
  const [open, setOpen] = useState<string | null>(null);
  const [pinned, setPinned] = useState(false);
  // Where the open card goes, from its segment as drawn: left, and below the strip.
  const [place, setPlace] = useState({ left: 0, top: 0 });
  // The pointer of the last press: a mouse's click leads on, a touch's pins the card.
  const pointer = useRef("mouse");
  const root = useRef<HTMLDivElement>(null);

  // A pinned card closes on a press anywhere but the strip and the card,
  // which goes on to whatever it pressed.
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

  // The open segment, gone when a later answer no longer has it.
  const openSegment = segments.find((s) => s.key === open);
  // What lights: the segment open here; else the machine open in the CPU strip, where it has a segment here.
  const litKey = openSegment ? openSegment.key : linked && segments.some((s) => s.key === linked) ? linked : null;
  // The CPU strip is told the machine open here: a machine's alone, as
  // "more" and "other" hold different machines in each strip; none once it
  // is gone, or this strip is.
  const openMachine = openSegment?.e ? openSegment.key : null;
  useEffect(() => onOpen?.(openMachine), [openMachine, onOpen]);
  useEffect(() => () => onOpen?.(null), [onOpen]);
  useLayoutEffect(() => {
    const r = root.current;
    const el = open ? r?.querySelector<HTMLElement>(`[data-segment="${CSS.escape(open)}"]`) : null;
    if (!r || !el || !track.current) return;
    const middle = el.offsetLeft + el.offsetWidth / 2;
    const left = Math.max(0, Math.min(middle - cardWidth / 2, r.clientWidth - cardWidth));
    setPlace({ left, top: track.current.offsetTop + track.current.offsetHeight + 8 });
  }, [open, segments.length, trackPx]);

  const used = segments.reduce((sum, s) => sum + s.lengthGi, 0);
  const freeGi = Math.max(0, occupancy.sizeGi - used);
  const full = freeGi < fullBelowGi;

  const handlers = (s: Segment) => ({
    onPointerEnter: (ev: React.PointerEvent) => ev.pointerType === "mouse" && !pinned && setOpen(s.key),
    onPointerLeave: (ev: React.PointerEvent) => ev.pointerType === "mouse" && !pinned && setOpen(null),
    onPointerDown: (ev: React.PointerEvent) => {
      pointer.current = ev.pointerType;
    },
    onFocus: () => !pinned && setOpen(s.key),
    onBlur: () => !pinned && setOpen(null),
    onClick: (ev: React.MouseEvent) => {
      // A touch pins the card; a mouse, or a key (no pointer at all), goes on.
      if (ev.detail !== 0 && pointer.current !== "mouse") {
        ev.preventDefault();
        setOpen(s.key);
        setPinned(true);
      }
    },
  });

  return (
    <div ref={root} className="relative">
      <Legend current={occupancy.current} />
      <div className={cn(!occupancy.current && "opacity-50")} data-current={occupancy.current || undefined}>
        <div
          ref={track}
          data-testid="pool-track"
          data-full={full || undefined}
          className={cn("flex h-6 gap-[2px] overflow-hidden rounded-[3px] bg-surface-2 p-[2px]", full ? "ring-2 ring-foreground/80" : "ring-1 ring-border-strong")}
        >
          {segments.map((s) => {
            const lit = litKey === null || litKey === s.key;
            const reservedPart = s.lengthGi > 0 ? s.reservedGi / s.lengthGi : 1;
            const f = fills(s);
            const label = (onDark: boolean) =>
              s.e ? (
                <SegmentLabel name={s.e.name} mark={markOf(s.e)} onDark={onDark} />
              ) : s.group ? (
                <span className="w-full truncate text-center leading-[14px]">+{s.group.length}</span>
              ) : null;
            const className = cn(
              "relative flex min-w-0 cursor-pointer overflow-hidden rounded-[2px] outline-none transition-opacity focus-visible:ring-2 focus-visible:ring-accent max-sm:min-w-[26px]",
              !lit && "opacity-35",
            );
            const body = (
              <>
                <span
                  className={cn("@container flex h-full shrink-0 items-center overflow-hidden px-1 text-[10.5px] text-surface", f.reservedClass)}
                  style={{ width: `${reservedPart * 100}%`, ...f.reserved }}
                >
                  {reservedPart > 0.55 && label(true)}
                </span>
                {reservedPart < 1 && (
                  <span className={cn("@container flex h-full flex-1 items-center overflow-hidden px-1 text-[10.5px] text-foreground", f.sharedClass)} style={f.shared}>
                    {reservedPart <= 0.55 && label(false)}
                  </span>
                )}
              </>
            );
            const common = { "aria-label": segmentLabel(s), "data-segment": s.key, className, style: { flex: `${s.lengthGi} 1 0` }, ...handlers(s) };
            return s.e ? (
              <Link key={s.key} to={machinePath(s.e.kind)} params={{ project: s.e.project, name: s.e.name }} {...common}>
                {body}
              </Link>
            ) : (
              <button key={s.key} type="button" {...common}>
                {body}
              </button>
            );
          })}
          {freeGi > 0 && <span className="min-w-0" style={{ flex: `${freeGi} 1 0` }} />}
        </div>
        <div className="mt-1 flex justify-between font-mono text-[10.5px] text-subtle tabular">
          {/* What is in use, until it is all of it: then only the pool's size, in ink. */}
          <span>{!full && gib(used)}</span>
          <span className={cn(full && "font-medium text-foreground")}>{gib(occupancy.sizeGi)}</span>
        </div>
      </div>
      {openSegment && (
        <div
          role="tooltip"
          // Under the strip; on a narrow screen wholly above the card's
          // legend and strip, so both stay in sight under a finger.
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
 * The card on a tenant's Overview, or a project's (project set), right under
 * the tiles, for a tenant the page knows has a memory pool (enabled), a CPU
 * pool (cpu, with the plan's CPUs), or both: the memory strip, and under it
 * the CPU row. Until its first answer each part holds its place, blank and
 * as tall as it is calm; a part goes only when its answer fails, the tenant
 * turns out to have no memory pool, or its asks keep failing (usePoolRead,
 * useCpuPoolRead). The CPU row opening into its strip, while the pool slows
 * its machines, is the one other change in the card's height.
 */
export function PoolCard({ tenant, project, enabled = true, cpu }: { tenant: string; project?: string; enabled?: boolean; cpu?: { planCPU: number } }) {
  const read = usePoolRead(tenant, enabled);
  const cpuRead = useCpuPoolRead(tenant, !!cpu);
  // The machine open in either strip, which the other lights too.
  const [memoryOpen, setMemoryOpen] = useState<string | null>(null);
  const [cpuOpen, setCpuOpen] = useState<string | null>(null);
  const memory = read && read.state !== "failed" && read.state !== "none" ? read : undefined;
  const cpuShown = cpu && cpuRead && cpuRead.state !== "failed" && cpuRead.state !== "none" ? cpuRead : undefined;
  if (!memory && !cpuShown) return null;
  const cpuPart =
    cpuShown?.state === "read" ? (
      <CpuPoolRow
        o={cpuShown.occupancy}
        throttled={cpuShown.throttled}
        planCPU={cpu!.planCPU}
        project={project}
        linked={memoryOpen}
        onOpen={setCpuOpen}
        divided={!!memory}
      />
    ) : cpuShown ? (
      <CpuPoolRowPlaceholder divided={!!memory} />
    ) : null;
  const label = memory ? "Memory pool" : "CPU pool";
  if (memory?.state === "pending") {
    return (
      <Card className="px-4 pb-3 pt-3" aria-label={label} aria-busy data-loading>
        {/* The same lines as the card's, unseen, so it is as tall. */}
        <div aria-hidden className="invisible">
          <Legend current />
        </div>
        <div aria-hidden data-testid="pool-track-placeholder" className="h-6 animate-pulse rounded-[3px] bg-surface-2 ring-1 ring-border-strong" />
        <div aria-hidden className="invisible mt-1 font-mono text-[10.5px] tabular">
          0 GiB
        </div>
        {cpuPart}
      </Card>
    );
  }
  return (
    <Card className="px-4 pb-3 pt-3" aria-label={label} aria-busy={(!memory && cpuShown?.state === "pending") || undefined}>
      {memory && <PoolStrip occupancy={memory.occupancy} project={project} linked={cpuOpen} onOpen={setMemoryOpen} />}
      {cpuPart}
    </Card>
  );
}
