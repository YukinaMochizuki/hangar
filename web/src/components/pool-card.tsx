// The Overview's memory pool card (docs/decisions/console-design.md, "Memory
// pools"): who holds the tenant's pool now. Its scale is the pool, the plan's
// memory; each running machine is a segment as long as what its guest has
// now, its reservation solid and what it has above it a lighter fill; what no
// one holds is the bare track. On a project's Overview the other projects'
// machines are one muted, unnamed segment. A machine stopped for memory, or
// whose balloon does not follow, is tinted and marked. Hover or focus opens a
// machine's card, a click leads to its page; a tap pins the card with its name
// as the link.

import { Link } from "@tanstack/react-router";
import { Ban, ChevronRight, TriangleAlert } from "lucide-react";
import { useEffect, useRef, useState, type CSSProperties, type ReactNode } from "react";
import { FittingText } from "@/components/fitting-text";
import { KilledIcon, StuckIcon } from "@/components/machine-memory";
import { Card, StatusDot, phaseLabel } from "@/components/ui";
import { cn } from "@/lib/cn";
import { ago, machinePath } from "@/lib/machines";
import { usePoolOccupancy, type PoolEntry, type PoolOccupancy } from "@/lib/memory-pool";
import { formatNumber } from "@/lib/quantity";

/** A segment: one machine, or other projects' machines together, muted and unnamed; its sizes in GiB. */
type Segment = { key: string; e?: PoolEntry; reservedGi: number; nowGi: number };

/** Below this much free, in GiB, the pool reads as full. */
const fullBelowGi = 1 / 8;

/** The width of a machine's card, in px. */
const cardWidth = 236;

const gib = (v: number) => `${formatNumber(+v.toFixed(2))} GiB`;

/** What a machine's segment is as long as: what its guest has now, its reservation when its node could not tell. */
const lengthOf = (e: PoolEntry) => e.nowGi ?? e.reservedGi;

const dedicated = (e: PoolEntry) => e.reservedGi >= e.memoryGi;

/** Dedicated machines first, then Shared ones by what they have now, largest first. */
function byPoolOrder(a: PoolEntry, b: PoolEntry): number {
  return Number(!dedicated(a)) - Number(!dedicated(b)) || lengthOf(b) - lengthOf(a);
}

/**
 * The card's segments, in order: the machines of project, or all the caller
 * reads without one, then the rest together, with what the API summed.
 */
export function poolSegments(o: PoolOccupancy, project?: string): Segment[] {
  const own = (project ? o.entries.filter((e) => e.project === project) : o.entries).slice().sort(byPoolOrder);
  const segments: Segment[] = own.map((e) => ({ key: `${e.project}/${e.kind}/${e.name}`, e, reservedGi: Math.min(e.reservedGi, lengthOf(e)), nowGi: lengthOf(e) }));
  const rest = project ? o.entries.filter((e) => e.project !== project) : [];
  let reservedGi = o.other ? Math.min(o.other.reservedGi, o.other.nowGi) : 0;
  let nowGi = o.other?.nowGi ?? 0;
  for (const e of rest) {
    reservedGi += Math.min(e.reservedGi, lengthOf(e));
    nowGi += lengthOf(e);
  }
  if (nowGi > 0) segments.push({ key: "other", reservedGi, nowGi });
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

/** What a machine's card says of its memory, below its name. */
function MemoryLines({ e }: { e: PoolEntry }) {
  if (dedicated(e)) {
    return (
      <>
        {e.nowGi !== undefined && <div>{figures(`${gib(e.nowGi)} now`)}</div>}
        <div className="mt-0.5 text-[11.5px] text-muted">{figures(`${gib(e.memoryGi)} reserved`)}</div>
      </>
    );
  }
  const above = e.nowGi !== undefined ? e.nowGi - e.reservedGi : 0;
  const split =
    above > 0
      ? [`${gib(e.reservedGi)} reserved + ${gib(above)} shared`, `${formatNumber(+e.reservedGi.toFixed(2))} + ${gib(above)} shared`]
      : [`${gib(e.reservedGi)} reserved`];
  return (
    <>
      {e.nowGi !== undefined && <div>{figures(`${gib(e.nowGi)} now`)}</div>}
      <div className="mt-0.5 flex text-[11.5px] text-muted">
        <FittingText texts={split} render={figures} className="min-w-0 truncate whitespace-nowrap" />
      </div>
      <div className="text-[11.5px] text-subtle">{figures(`up to ${gib(e.memoryGi)}`)}</div>
    </>
  );
}

/** A segment's card: the machine's name (a link when pinned), kind, project, state, memory and problem; or the other projects' total. */
function SegmentCard({ s, pinned }: { s: Segment; pinned: boolean }) {
  const e = s.e;
  if (!e) {
    return (
      <div className="text-[12.5px]">
        <span className="font-medium text-muted">Other projects</span> · {figures(`${gib(s.nowGi)} now`)}
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
        <MemoryLines e={e} />
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
  if (!s.e) return `Other projects, ${gib(s.nowGi)} now`;
  const state = s.e.killedAt ? ", stopped for memory" : s.e.stuckSince ? ", not sharing memory" : "";
  return `${s.e.name}${state}${s.e.nowGi !== undefined ? `, ${gib(s.e.nowGi)} now` : ""}`;
}

/** The fills of a segment's reserved and shared parts: a marked one's take a little of its state's colour. */
function fills(s: Segment): { reserved?: CSSProperties; shared?: CSSProperties; reservedClass: string; sharedClass: string } {
  const mark = markOf(s.e);
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

/** Where a segment's middle sits, as a share of the scale, for the card under it. */
function middleOf(segments: Segment[], s: Segment, scaleGi: number): number {
  let at = 0;
  for (const x of segments) {
    if (x === s) return Math.min((at + x.nowGi / 2) / scaleGi, 1);
    at += x.nowGi;
  }
  return 0;
}

/** The strip and its card, for a pool's occupancy; project set, that project's machines named and the rest muted. */
export function PoolStrip({ occupancy, project }: { occupancy: PoolOccupancy; project?: string }) {
  const segments = poolSegments(occupancy, project);
  const [open, setOpen] = useState<string | null>(null);
  const [pinned, setPinned] = useState(false);
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

  const held = segments.reduce((sum, s) => sum + s.nowGi, 0);
  const freeGi = Math.max(0, occupancy.sizeGi - held);
  const full = freeGi < fullBelowGi;
  const scaleGi = Math.max(occupancy.sizeGi, held) || 1;
  const openSegment = segments.find((s) => s.key === open);

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
      <div
        data-testid="pool-track"
        data-full={full || undefined}
        className={cn("flex h-6 gap-[2px] overflow-hidden rounded-[3px] bg-surface-2 p-[2px]", full ? "ring-2 ring-foreground/80" : "ring-1 ring-border-strong")}
      >
        {segments.map((s) => {
          const lit = open === null || open === s.key;
          const reservedPart = s.nowGi > 0 ? s.reservedGi / s.nowGi : 1;
          const f = fills(s);
          const label = (onDark: boolean) => (s.e ? <SegmentLabel name={s.e.name} mark={markOf(s.e)} onDark={onDark} /> : null);
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
          const common = { "aria-label": segmentLabel(s), "data-segment": s.key, className, style: { flex: `${s.nowGi} 1 0` }, ...handlers(s) };
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
        {/* What is held, until it is all of it: then only the pool's size, in ink. */}
        <span>{!full && gib(held)}</span>
        <span className={cn(full && "font-medium text-foreground")}>{gib(occupancy.sizeGi)}</span>
      </div>
      {openSegment && (
        <div
          role="tooltip"
          // Under the strip; on a narrow screen above it, so the strip stays in sight.
          className="absolute top-8 z-20 rounded-lg border bg-surface px-3 py-2.5 shadow-lg max-sm:bottom-8 max-sm:top-auto"
          style={{
            width: cardWidth,
            left: `clamp(0px, calc(${middleOf(segments, openSegment, scaleGi) * 100}% - ${cardWidth / 2}px), calc(100% - ${cardWidth}px))`,
          }}
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
 * the tiles: only for a tenant with a memory pool, asked of its nodes every
 * 10 s, and nothing at all while the first answer is pending or failed, so
 * nothing moves once it comes.
 */
export function PoolCard({ tenant, project, enabled = true }: { tenant: string; project?: string; enabled?: boolean }) {
  const occupancy = usePoolOccupancy(tenant, enabled);
  if (!occupancy) return null;
  return (
    <Card className="px-4 pb-3 pt-3" aria-label="Memory pool">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-x-4 gap-y-1">
        <span className="text-[12px] text-muted">Memory</span>
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
      <PoolStrip occupancy={occupancy} project={project} />
    </Card>
  );
}
