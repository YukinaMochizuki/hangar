// A line drawing of the host a machine runs on, seen from below as if
// looking up at it in its rack. Each chassis is built in 3D millimetres and
// projected, so every model shares one camera and the details on its front
// panel stay in perspective. Only the LEDs take colour: the accent while the
// machine runs, the status colours otherwise.

import { useId } from "react";
import { cn } from "@/lib/cn";
import { MachinePhase, type Chassis } from "@/mock/data";

export type ArtStatus = "running" | "pending" | "failed" | "stopped";

/** What the host's LEDs show for a machine in this phase. */
export function artStatus(phase: MachinePhase): ArtStatus {
  if (phase === MachinePhase.RUNNING) return "running";
  if (phase === MachinePhase.FAILED) return "failed";
  if (phase === MachinePhase.STOPPED || phase === MachinePhase.SUSPENDED) return "stopped";
  return "pending";
}

type V3 = [number, number, number];
type P2 = [number, number];

type Box = { w: number; h: number; d: number };

const YAW = 0.34;
const PITCH = -0.27;

function camera(box: Box) {
  const cx = box.w / 2;
  const cy = box.h / 2;
  const cz = -box.d / 2;
  const dist = Math.max(box.w, box.d) * 1.55;
  const cosY = Math.cos(YAW), sinY = Math.sin(YAW);
  const cosP = Math.cos(PITCH), sinP = Math.sin(PITCH);
  return ([x, y, z]: V3): P2 => {
    const px = x - cx, py = y - cy, pz = z - cz;
    const x1 = px * cosY + pz * sinY;
    const z1 = -px * sinY + pz * cosY;
    const y2 = py * cosP - z1 * sinP;
    const z2 = py * sinP + z1 * cosP;
    const s = dist / (dist - z2);
    return [x1 * s, -y2 * s];
  };
}

type Stroke = "line" | "faint" | "detail";

type Shape = {
  pts: P2[];
  closed: boolean;
  stroke: Stroke;
  fill?: "face" | "under" | "side" | "slot";
};

type Led = { at: P2; r: number; role: "status" | "activity" | "power" };

class Builder {
  shapes: Shape[] = [];
  leds: Led[] = [];
  constructor(private proj: (p: V3) => P2) {}

  poly(pts: V3[], stroke: Stroke, fill?: Shape["fill"]) {
    this.shapes.push({ pts: pts.map(this.proj), closed: true, stroke, fill });
  }
  line(a: V3, b: V3, stroke: Stroke = "detail") {
    this.shapes.push({ pts: [this.proj(a), this.proj(b)], closed: false, stroke });
  }
  /** A rectangle on the front panel (z = 0), in panel millimetres. */
  rect(x: number, y: number, w: number, h: number, stroke: Stroke = "detail", fill?: Shape["fill"], z = 0) {
    this.poly([[x, y, z], [x + w, y, z], [x + w, y + h, z], [x, y + h, z]], stroke, fill);
  }
  circle(x: number, y: number, r: number, stroke: Stroke = "detail", z = 0) {
    const pts: V3[] = [];
    for (let i = 0; i < 20; i++) {
      const a = (i / 20) * Math.PI * 2;
      pts.push([x + r * Math.cos(a), y + r * Math.sin(a), z]);
    }
    this.poly(pts, stroke);
  }
  led(x: number, y: number, r: number, role: Led["role"], z = 0) {
    const c = this.proj([x, y, z]);
    const e = this.proj([x + r, y, z]);
    this.leds.push({ at: c, r: Math.hypot(e[0] - c[0], e[1] - c[1]), role });
  }
  /** The three faces seen from below and to the left: underside, left side, front. */
  body(b: Box, oy: number, stroke: Stroke) {
    const { w, h, d } = b;
    const y0 = oy, y1 = oy + h;
    this.poly([[0, y0, 0], [w, y0, 0], [w, y0, -d], [0, y0, -d]], stroke, stroke === "faint" ? undefined : "under");
    this.poly([[0, y0, 0], [0, y0, -d], [0, y1, -d], [0, y1, 0]], stroke, stroke === "faint" ? undefined : "side");
    this.poly([[0, y0, 0], [w, y0, 0], [w, y1, 0], [0, y1, 0]], stroke, stroke === "faint" ? undefined : "face");
  }
}

const RACK_W = 440;

// Rack posts are see-through, like the neighbouring units, so only the
// machine stands out. They are drawn after the chassis body all the same:
// drawn before it, the body's filled faces would cut them short.
const POST_W = 18;
const POST_D = 24;

function rackUnit(b: Builder, h: number, d: number, ghostsAbove: number, ghostsBelow: number) {
  const box = { w: RACK_W, h, d };
  for (let i = ghostsBelow; i >= 1; i--) b.body({ w: RACK_W, h: 44, d }, -i * 46, "faint");
  for (let i = 1; i <= ghostsAbove; i++) b.body({ w: RACK_W, h: 44, d }, h + 2 + (i - 1) * 46, "faint");
  b.body(box, 0, "line");
  // Rails on the underside.
  b.line([30, 0, -20], [30, 0, -d + 20], "faint");
  b.line([RACK_W - 30, 0, -20], [RACK_W - 30, 0, -d + 20], "faint");

  // Rack posts either side, spanning the ghosts: the outer face the camera
  // sees from the left, then the front face with its square holes.
  const top = h + 2 + ghostsAbove * 46;
  const bottom = -ghostsBelow * 46;
  for (const x of [-POST_W - 2, RACK_W + 2]) {
    b.poly([[x, bottom, 0], [x, bottom, -POST_D], [x, top, -POST_D], [x, top, 0]], "faint");
    b.poly([[x, bottom, 0], [x + POST_W, bottom, 0], [x + POST_W, top, 0], [x, top, 0]], "faint");
    for (let y = bottom + 6; y + 4 < top; y += 15.6) {
      if (y > -2 && y < h + 2) continue;
      b.rect(x + POST_W / 2 - 2, y, 4, 4, "faint", undefined, 0);
    }
  }
  // Rack ears, exactly over the posts.
  b.rect(-POST_W - 2, 0, POST_W + 2, h, "line", "face", 1);
  b.rect(RACK_W, 0, POST_W + 2, h, "line", "face", 1);
  for (const x of [-POST_W / 2 - 2, RACK_W + POST_W / 2 + 2]) {
    b.circle(x, h / 2 - (h > 50 ? h / 4 : 0), 3, "detail", 1);
    if (h > 50) b.circle(x, h / 2 + h / 4, 3, "detail", 1);
  }
  return box;
}

function vents(b: Builder, x0: number, x1: number, y0: number, y1: number, rows: number) {
  const step = (y1 - y0) / rows;
  for (let r = 0; r < rows; r++) {
    const y = y0 + step * (r + 0.5);
    const off = r % 2 ? 6 : 0;
    for (let x = x0 + off; x + 8 <= x1; x += 12) b.line([x, y, 0], [x + 7, y, 0], "detail");
  }
}

function build(chassis: Chassis, proj: (box: Box) => (p: V3) => P2) {
  if (chassis === "1u-2u2") {
    // A generic 1U with two 2.5" U.2 bays: the layout of a 1U barebone's
    // front (control column, a column of bays, perforated face), without
    // claiming the exact look of a custom chassis.
    const box = { w: RACK_W, h: 44, d: 650 };
    const b = new Builder(proj(box));
    rackUnit(b, 44, 650, 2, 2);
    // Perforated strip along the top edge.
    for (let x = 6; x + 3 <= RACK_W - 6; x += 7) b.rect(x, 40, 2.4, 1.6, "faint");
    // Control column: power button, status and LAN LEDs, a USB port.
    b.circle(14, 30, 5, "detail");
    b.led(14, 30, 1.8, "power");
    b.led(10, 19, 1.8, "status");
    b.led(18, 19, 1.8, "activity");
    b.rect(8, 6, 12, 5, "detail", "slot");
    b.line([28, 4, 0], [28, 38, 0], "detail");
    // Two U.2 trays, stacked, each with its latch and activity LED.
    for (const y of [4, 21.5]) {
      b.rect(34, y, 92, 16, "detail", "slot");
      b.rect(110, y + 3, 12, 10, "line");
      for (let x = 42; x < 102; x += 6) b.line([x, y + 4, 0], [x, y + 12, 0], "faint");
      b.led(39, y + 8, 1.6, "activity");
    }
    // The rest of the face is perforated.
    for (let row = 0; row < 5; row++) {
      const y = 7 + row * 6.2;
      for (let x = 140 + (row % 2) * 3.5; x + 3 <= RACK_W - 10; x += 7) b.rect(x, y, 3, 3, "faint");
    }
    return b;
  }
  if (chassis === "storage-2u") {
    const box = { w: RACK_W, h: 88, d: 680 };
    const b = new Builder(proj(box));
    rackUnit(b, 88, 680, 1, 1);
    // Twelve 3.5" bays, four across and three high.
    for (let c = 0; c < 4; c++) {
      for (let r = 0; r < 3; r++) {
        const x = 14 + c * 104;
        const y = 4 + r * 27.5;
        b.rect(x, y, 100, 25, "detail", "slot");
        b.line([x + 8, y + 12.5, 0], [x + 70, y + 12.5, 0]);
        b.led(x + 90, y + 12.5, 1.8, "activity");
      }
    }
    b.led(-11, 44, 2.6, "status", 2);
    b.led(RACK_W + 11, 44, 2.6, "power", 2);
    return b;
  }
  if (chassis === "mini") {
    const box = { w: 180, h: 54, d: 180 };
    const b = new Builder(proj(box));
    b.body(box, 0, "line");
    // Rubber feet on the underside.
    for (const [x, z] of [[22, -22], [158, -22], [158, -158], [22, -158]] as const) {
      const pts: V3[] = [];
      for (let i = 0; i < 16; i++) {
        const a = (i / 16) * Math.PI * 2;
        pts.push([x + 9 * Math.cos(a), 0, z + 9 * Math.sin(a)]);
      }
      b.poly(pts, "detail");
    }
    // Side vent.
    for (let z = -30; z > -150; z -= 10) b.line([0, 12, z], [0, 42, z], "detail");
    b.line([14, 46, 0], [166, 46, 0], "detail");
    b.circle(30, 24, 7, "detail");
    b.led(30, 24, 2, "power");
    b.rect(60, 18, 14, 6, "detail", "slot");
    b.rect(82, 18, 14, 6, "detail", "slot");
    b.rect(108, 17, 10, 8, "detail", "slot");
    b.led(150, 24, 2.2, "status");
    return b;
  }
  const box = { w: RACK_W, h: 44, d: 600 };
  const b = new Builder(proj(box));
  rackUnit(b, 44, 600, 2, 2);
  vents(b, 20, 330, 8, 36, 4);
  b.led(380, 22, 2.4, "status");
  b.circle(410, 22, 7, "detail");
  b.led(410, 22, 2, "power");
  return b;
}

const ledColour: Record<ArtStatus, string> = {
  running: "var(--accent)",
  pending: "var(--pending)",
  failed: "var(--fail)",
  stopped: "transparent",
};

export function ChassisArt({
  chassis,
  status,
  className,
}: {
  chassis: Chassis;
  status: ArtStatus;
  className?: string;
}) {
  const id = useId().replace(/:/g, "");
  const b = build(chassis, camera);

  const all = b.shapes.flatMap((s) => s.pts);
  const xs = all.map((p) => p[0]);
  const ys = all.map((p) => p[1]);
  const minX = Math.min(...xs), maxX = Math.max(...xs);
  const minY = Math.min(...ys), maxY = Math.max(...ys);
  const pad = (maxX - minX) * 0.06;
  const vb = [minX - pad, minY - pad, maxX - minX + pad * 2, maxY - minY + pad * 2];

  const d = (s: Shape) =>
    s.pts.map((p, i) => `${i ? "L" : "M"}${p[0].toFixed(2)} ${p[1].toFixed(2)}`).join("") + (s.closed ? "Z" : "");

  const fills: Record<NonNullable<Shape["fill"]>, string> = {
    face: "var(--surface)",
    under: "var(--surface-2)",
    side: "var(--surface-2)",
    slot: "var(--background)",
  };

  return (
    <svg
      viewBox={vb.join(" ")}
      className={cn("chassis-art block h-auto w-full", className)}
      role="img"
      aria-label="Host illustration"
    >
      <defs>
        <filter id={`glow-${id}`} x="-200%" y="-200%" width="500%" height="500%">
          <feGaussianBlur stdDeviation="3" result="b" />
          <feMerge>
            <feMergeNode in="b" />
            <feMergeNode in="SourceGraphic" />
          </feMerge>
        </filter>
      </defs>
      {b.shapes.map((s, i) => (
        <path
          key={i}
          d={d(s)}
          fill={s.fill ? fills[s.fill] : "none"}
          stroke={s.stroke === "faint" ? "var(--art-faint)" : "var(--art-line)"}
          strokeOpacity={s.stroke === "detail" ? 0.7 : 1}
          strokeWidth={s.stroke === "line" ? 1.25 : 1}
          strokeLinejoin="round"
          strokeLinecap="round"
          vectorEffect="non-scaling-stroke"
        />
      ))}
      {b.leds.map((l, i) => {
        const lit =
          l.role === "power" ? status !== "stopped" : l.role === "status" ? status !== "stopped" : status === "running";
        const colour = l.role === "power" ? (status === "stopped" ? "transparent" : "var(--ok)") : ledColour[status];
        return (
          <circle
            key={i}
            cx={l.at[0]}
            cy={l.at[1]}
            r={Math.max(l.r * 1.3, 2.4)}
            fill={lit ? colour : "none"}
            stroke={lit ? "none" : "var(--art-line)"}
            strokeWidth={0.8}
            vectorEffect="non-scaling-stroke"
            filter={lit ? `url(#glow-${id})` : undefined}
            className={cn(
              lit && l.role === "activity" && "led-activity",
              lit && l.role === "status" && status === "pending" && "led-blink",
              lit && l.role === "status" && status === "failed" && "led-blink-slow",
            )}
            style={l.role === "activity" ? { animationDelay: `${(i * 0.37) % 1.9}s` } : undefined}
          />
        );
      })}
    </svg>
  );
}
