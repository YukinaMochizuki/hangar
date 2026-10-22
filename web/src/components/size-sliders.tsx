// A Custom machine shape as two rulers, under the size cards. Memory stops
// at the common shapes; the vCPU ruler hatches the vCPUs that memory does
// not allow, says why, and keeps the pointer out of them, so a shape the
// rules refuse cannot be set (lib/machine-sizes.ts). A field beside each
// ruler takes the value typed, memory off the stops included.

import { useState, type ReactNode } from "react";
import { Input } from "@/components/form";
import { cn } from "@/lib/cn";
import { clampCPU, cpuRange, memoryStepMiB } from "@/lib/machine-sizes";

/** Memory in GiB as typed and shown: 2, 2.75. */
export const gibText = (mib: number) => String(+(mib / 1024).toFixed(2));

/** The share of the ruler a value sits at, as the input's thumb centre does. */
const at = (i: number, n: number) => (n <= 1 ? 0 : (i / (n - 1)) * 100);

/** A memory stop as a ruler label: 512M, 2G. */
const stopLabel = (mib: number) => (mib < 1024 ? `${mib}M` : `${+(mib / 1024).toFixed(2)}G`);

// The memory stops labelled: whole powers of two GiB, and the last.
const labelled = (mib: number, i: number, stops: number[]) => i === stops.length - 1 || (mib >= 1024 && Number.isInteger(Math.log2(mib)));

export function SizeSliders({
  memMiB,
  cpu,
  stops,
  cpuMax,
  over,
  onChange,
}: {
  memMiB: number;
  cpu: number;
  /** The memory ruler's stops, MiB, smallest first; memMiB is one of them. */
  stops: number[];
  cpuMax: number;
  /** The shape is over the project's quota. */
  over?: boolean;
  onChange: (shape: { memMiB: number; cpu: number }) => void;
}) {
  const mi = Math.max(0, stops.indexOf(memMiB));
  const { lo, hi: rawHi } = cpuRange(memMiB);
  const hi = Math.min(rawHi, cpuMax);
  const pos = (v: number) => at(v - 1, cpuMax);
  // Memory typed in GiB: any whole number of 256 MiB steps, on a stop or not.
  const typeMemory = (text: string) => {
    const mib = Math.round(Number(text) * 1024);
    if (!text.trim() || !Number.isFinite(mib) || mib < memoryStepMiB || mib % memoryStepMiB !== 0) return false;
    onChange({ memMiB: mib, cpu: clampCPU(cpu, mib) });
    return true;
  };
  // vCPUs typed: only what the memory allows.
  const typeCPU = (text: string) => {
    const n = Number(text);
    if (!/^\d+$/.test(text.trim()) || n < lo || n > rawHi) return false;
    onChange({ memMiB, cpu: n });
    return true;
  };
  return (
    <div className={cn("space-y-3 rounded-lg border bg-surface px-4 pb-3 pt-3.5", over && "border-fail")}>
      <Row label="Memory" field={<NumberField label="Memory, GiB" unit="GiB" value={gibText(memMiB)} onType={typeMemory} />}>
        <input
          type="range"
          className="size-range"
          aria-label="Memory"
          aria-valuetext={`${gibText(memMiB)} GiB`}
          min={0}
          max={stops.length - 1}
          step={1}
          value={mi}
          onChange={(e) => {
            const m = stops[Number(e.target.value)];
            onChange({ memMiB: m, cpu: clampCPU(cpu, m) });
          }}
        />
        <Ruler n={stops.length} value={mi} major={(i) => labelled(stops[i], i, stops)} />
        <Labels>
          {stops.map((m, i) =>
            labelled(m, i, stops) ? (
              <Label key={m} left={at(i, stops.length)} on={m === memMiB}>
                {stopLabel(m)}
              </Label>
            ) : null,
          )}
        </Labels>
      </Row>
      <Row label="vCPUs" field={<NumberField label="vCPUs, number" unit="vCPU" value={String(cpu)} onType={typeCPU} />}>
        <input
          type="range"
          className="size-range"
          aria-label="vCPUs"
          min={1}
          max={cpuMax}
          step={1}
          value={cpu}
          onChange={(e) => onChange({ memMiB, cpu: clampCPU(Number(e.target.value), memMiB) })}
        />
        <Ruler n={cpuMax} value={cpu - 1} major={(i) => i + 1 === lo || i + 1 === hi} allowed={(i) => i + 1 >= lo && i + 1 <= hi} />
        <Labels>
          {/* The allowed range's ends, and the ruler's when they differ. */}
          {[...new Set([1, lo, hi, cpuMax])].map((v) => (
            <Label key={v} left={pos(v)} on={v === cpu}>
              {v}
            </Label>
          ))}
        </Labels>
        {(hi < cpuMax || lo > 1) && (
          <Hints>
            {lo > 1 && (
              <Hint from={0} to={pos(lo - 1)}>
                {lo === 2 ? "1 needs" : `1–${lo - 1} need`} less memory
              </Hint>
            )}
            {hi < cpuMax && (
              <Hint from={pos(hi + 1)} to={100}>
                {hi + 1 === cpuMax ? `${cpuMax} needs` : `${hi + 1}+ need`} more memory
              </Hint>
            )}
          </Hints>
        )}
      </Row>
    </div>
  );
}

function Row({ label, field, children }: { label: string; field: ReactNode; children: ReactNode }) {
  return (
    // On a phone the label and field sit above the ruler, which takes the row.
    <div className="grid grid-cols-[1fr_auto] items-start gap-x-4 gap-y-1 sm:grid-cols-[56px_minmax(0,1fr)_auto]">
      <span className="pt-1.5 text-[12.5px] text-muted">{label}</span>
      <div className="relative order-last col-span-2 min-h-8 sm:order-none sm:col-span-1">{children}</div>
      {field}
    </div>
  );
}

/**
 * A value typed by hand beside its ruler. What is typed is applied once it
 * is a value the rules allow; until then the field is marked and the shape
 * stays as it was. Leaving the field shows the shape's value again.
 */
function NumberField({ label, unit, value, onType }: { label: string; unit: string; value: string; onType: (text: string) => boolean }) {
  // What is typed, until the field is left, and whether the rules refused it.
  const [draft, setDraft] = useState<string | null>(null);
  const [bad, setBad] = useState(false);
  return (
    <span className="flex items-center gap-1.5">
      <Input
        // What is typed stays while it is refused or is the value; once a ruler moves the value, the value shows.
        value={draft !== null && (bad || String(+draft) === value) ? draft : value}
        onChange={(e) => {
          setDraft(e.target.value);
          setBad(!onType(e.target.value));
        }}
        onBlur={() => {
          setDraft(null);
          setBad(false);
        }}
        inputMode="decimal"
        aria-label={label}
        aria-invalid={bad}
        className={cn("h-7 w-16 px-2 text-right text-[12.5px] tabular", bad && "border-fail focus:border-fail")}
      />
      <span className="w-8 text-[11.5px] text-subtle">{unit}</span>
    </span>
  );
}

/**
 * A ruler under the input, one tick a value, inset by half the input's thumb
 * so a tick sits where the thumb's centre does for its value. Ticks up to the
 * value are filled; values the rules do not allow are a hatched stretch with
 * no ticks, and cannot be reached. The input's thumb is invisible; the
 * pointer drawn here shows its focus (styles.css).
 */
function Ruler({ n, value, major, allowed = () => true }: { n: number; value: number; major: (i: number) => boolean; allowed?: (i: number) => boolean }) {
  // The stretches the rules do not allow, as [first, last] indices.
  const off: [number, number][] = [];
  for (let i = 0; i < n; i++) {
    if (allowed(i)) continue;
    const last = off[off.length - 1];
    if (last && last[1] === i - 1) last[1] = i;
    else off.push([i, i]);
  }
  return (
    <div aria-hidden className="size-ruler pointer-events-none absolute inset-x-2 top-0 h-5">
      <span className="absolute inset-x-0 bottom-1.5 h-px bg-border-strong" />
      {off.map(([a, b]) => (
        <span
          key={a}
          className="absolute bottom-1.5 h-1.5 -translate-x-[2px] bg-[repeating-linear-gradient(135deg,var(--border-strong)_0_1px,transparent_1px_4px)] px-[2px]"
          style={{ left: `${at(a, n)}%`, width: `calc(${at(b, n) - at(a, n)}% + 4px)` }}
        />
      ))}
      <span className="absolute bottom-1.5 left-0 h-px bg-accent" style={{ width: `${at(value, n)}%` }} />
      {Array.from({ length: n }, (_, i) =>
        allowed(i) ? (
          <span
            key={i}
            className={cn("absolute bottom-1.5 w-px -translate-x-1/2", major(i) ? "h-2" : "h-[5px]", i <= value ? "bg-accent" : "bg-subtle")}
            style={{ left: `${at(i, n)}%` }}
          />
        ) : null,
      )}
      <span
        className="size-pointer absolute bottom-[3px] h-3 w-[3px] -translate-x-1/2 rounded-[1px] bg-accent shadow-[0_0_0_2px_var(--surface)]"
        style={{ left: `${at(value, n)}%` }}
      />
    </div>
  );
}

function Labels({ children }: { children: ReactNode }) {
  return <div className="pointer-events-none absolute inset-x-2 top-[21px] h-3">{children}</div>;
}

function Label({ left, on, children }: { left: number; on?: boolean; children: ReactNode }) {
  return (
    <span
      className={cn("absolute -translate-x-1/2 font-mono text-[10.5px] leading-none tabular", on ? "font-semibold text-foreground" : "text-subtle")}
      style={{ left: `${left}%` }}
    >
      {children}
    </span>
  );
}

/** The line of notes under the labels, in the flow so the row grows to hold it. */
function Hints({ children }: { children: ReactNode }) {
  return <div className="pointer-events-none relative mx-2 mt-[34px] h-3">{children}</div>;
}

/** A note under a hatched stretch: centred on a wide one, held to the ruler's end on a narrow one. */
function Hint({ from, to, children }: { from: number; to: number; children: ReactNode }) {
  const wide = to - from >= 40;
  const style = wide ? { left: `${(from + to) / 2}%` } : from === 0 ? { left: 0 } : { right: 0 };
  return (
    <span className={cn("absolute whitespace-nowrap text-[10.5px] italic leading-none text-subtle", wide && "-translate-x-1/2")} style={style}>
      {children}
    </span>
  );
}
