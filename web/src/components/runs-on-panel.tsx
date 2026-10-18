// A machine's Runs on panel: where the machine stands on its way to a host,
// and the host once it is placed (docs/decisions/console-design.md).

import { create } from "@bufbuild/protobuf";
import { PlacementSchema } from "@hangar/api/hangar/compute/v1alpha1/placement_pb";
import { useEffect, useState, type CSSProperties, type ReactNode } from "react";
import { ChassisArt, type ArtStatus } from "@/components/chassis-art";
import { Card, CardHeader, Field } from "@/components/ui";
import { cn } from "@/lib/cn";
import { useReducedMotion } from "@/lib/motion";
import { placementOf, type HostInfo } from "@/lib/placement";
import type { RunsOnState } from "@/lib/runs-on";

type Phase = "waiting" | "arriving" | "settled";

// How long an arrival plays, its rows included, in milliseconds.
const arrivalMs = 1100;

/**
 * Where the panel is: waiting for a host, playing its arrival, or showing
 * it. An arrival plays when the host comes while the panel is shown, or,
 * for a machine the console has just created (fresh), once when the panel
 * opens on a host already there.
 */
export function useArrival(placed: boolean, animate: boolean, fresh = false): Phase {
  const [phase, setPhase] = useState<Phase>(placed ? (fresh && animate ? "arriving" : "settled") : "waiting");
  const [was, setWas] = useState(placed);
  if (was !== placed) {
    setWas(placed);
    setPhase(!placed ? "waiting" : animate ? "arriving" : "settled");
  }
  useEffect(() => {
    if (phase !== "arriving") return;
    const t = setTimeout(() => setPhase("settled"), arrivalMs);
    return () => clearTimeout(t);
  }, [phase]);
  return phase;
}

/** What the project runs on the host: the panel's last block. */
export type HostShare = { node: string; values: [value: string, label: string][] };

// When the first row starts to fade in, after the host's lines, and the
// step between rows, in seconds.
const rowStart = 0.5;
const rowStep = 0.07;

function ShareBlock({ share }: { share: HostShare }) {
  return (
    <div className="mx-4 mb-4 mt-2 rounded-md border bg-surface-2 px-3 py-2.5">
      <div className="text-[11.5px] font-medium text-subtle">This project on {share.node}</div>
      <div className="mt-1.5 grid grid-cols-4 gap-2 text-center">
        {share.values.map(([v, l]) => (
          <div key={l}>
            <div className="font-mono text-[15px] font-medium tabular">{v}</div>
            <div className="text-[11px] text-muted">{l}</div>
          </div>
        ))}
      </div>
    </div>
  );
}

const artBox = "relative border-b bg-[radial-gradient(ellipse_at_50%_0%,var(--accent-soft),transparent_70%)] px-6 pb-3 pt-6";

/** The host's drawing and rows; arriving, the drawing traces itself and the rows fade in in order. */
function Host({ host, status, arriving, share }: { host: HostInfo; status: ArtStatus; arriving: boolean; share?: HostShare }) {
  let n = 0;
  const row = (): { className?: string; style?: CSSProperties } =>
    arriving ? { className: "arrive-row", style: { animationDelay: `${rowStart + n++ * rowStep}s` } } : {};
  const fields: [string, ReactNode][] = [];
  if (host.region || host.site) {
    fields.push([
      "Region",
      <>
        {host.region}
        {host.site && <span className="text-muted"> · {host.site}</span>}
      </>,
    ]);
  }
  if (host.memory) fields.push(["Memory", host.memory]);
  if (host.storage) fields.push(["Storage", host.storage]);
  if (host.network) fields.push(["Network", host.network]);
  const head = row();
  return (
    <>
      <div className={artBox}>
        <ChassisArt chassis={host.art} status={status} entrance={arriving ? "draw" : undefined} />
      </div>
      {/* Only what the host's placement says: its hardware, never its load. */}
      <div className={cn("px-4 pt-3", head.className)} style={head.style}>
        <div className="text-[15px] font-medium">{host.cpuModel || host.node}</div>
        {host.cpuGeneration && <div className="text-[12.5px] text-muted">{host.cpuGeneration}</div>}
      </div>
      <dl className="px-4 pb-1 pt-2">
        {fields.map(([label, value]) => {
          const r = row();
          return (
            <div key={label} className={r.className} style={r.style}>
              <Field label={label}>{value}</Field>
            </div>
          );
        })}
      </dl>
      {share && (
        <div {...row()}>
          <ShareBlock share={share} />
        </div>
      )}
    </>
  );
}

// A host with every field, drawn unseen under an empty panel so the panel is
// as tall as a placed one.
const anyHost = placementOf(
  create(PlacementSchema, { region: "x", site: "x", cpuModel: "x", cpuGeneration: "x", memory: "x", storage: "x", network: "x" }),
  "x",
)!;
const anyShare: HostShare = { node: "x", values: [["0", "a"], ["0", "b"], ["0", "c"], ["0", "d"]] };

/** The scheduler's words as a sentence: a capital first, a full stop last. */
function sentence(s: string): string {
  const t = s.trim();
  if (!t) return t;
  return t[0].toUpperCase() + t.slice(1) + (/[.!?]$/.test(t) ? "" : ".");
}

/**
 * A machine's Runs on panel. Its header says where the machine stands, on
 * the right where the host's name goes once it is placed: "Scheduling…",
 * "Not schedulable", nothing for a machine stopped before it was placed, or
 * the host. Until it is placed the body is empty, as tall as it will be: a
 * light sweeps across it while the machine is being scheduled, and the
 * scheduler's words stand in it when it cannot be. When the placement comes
 * while the panel is shown, or the panel opens on a machine just created
 * (fresh), the host draws itself in and its rows fade in; with reduced
 * motion asked for, nothing sweeps and the host simply appears. share is the
 * project's use of the host, shown last.
 */
export function RunsOnPanel({ state, status, share, fresh }: { state: RunsOnState; status: ArtStatus; share?: HostShare; fresh?: boolean }) {
  const still = useReducedMotion();
  const phase = useArrival(state.kind === "placed", !still, fresh);
  const header =
    state.kind === "placed" ? (
      <span className="font-mono text-[12px] text-muted">{state.host.node}</span>
    ) : state.kind === "scheduling" ? (
      <span className="text-[12px] text-muted">Scheduling…</span>
    ) : state.kind === "unschedulable" ? (
      <span className="text-[12px] font-medium text-pending">Not schedulable</span>
    ) : undefined;
  return (
    // Clipped to the card's corners, so the sweep reaches its bottom edge.
    <Card className="overflow-hidden">
      <CardHeader title="Runs on" action={header} />
      {state.kind === "placed" ? (
        <Host host={state.host} status={status} arriving={phase === "arriving"} share={share} />
      ) : (
        // flow-root keeps the last block's margin inside the body, so the
        // body, and the sweep over it, end at the card's bottom edge.
        <div className="relative flow-root" data-runs-on={state.kind}>
          <div className="invisible" aria-hidden>
            <Host host={anyHost} status={status} arriving={false} share={anyShare} />
          </div>
          {state.kind === "scheduling" && !still && (
            <div className="pointer-events-none absolute inset-0" aria-hidden>
              <div className="runs-on-sweep absolute inset-0" />
            </div>
          )}
          {state.kind === "unschedulable" && state.message && (
            <p className="absolute inset-x-0 top-0 px-6 pt-10 text-center text-[13px] leading-relaxed text-muted">{sentence(state.message)}</p>
          )}
        </div>
      )}
    </Card>
  );
}
