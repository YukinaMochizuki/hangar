// A container's root disk growing by itself while it runs, as it fills
// (docs/decisions/disk-autogrow.md): off by default, and, on, up to a
// largest size, with how full it is when it grows and by how much. The
// threshold and the step may stay empty for the platform's defaults.
// The rules mirror computeapi.ParseDiskGrow.

import { Input } from "@/components/form";
import { cn } from "@/lib/cn";
import { parseQuantity } from "@/lib/quantity";

export type DiskGrow = { on: boolean; maxGi: string; threshold: string; step: string };

export const noDiskGrow: DiskGrow = { on: false, maxGi: "", threshold: "", step: "" };

/** The platform's defaults, which an empty field takes. */
export const diskGrowDefaults = { threshold: 80, step: "2x" } as const;

const factor = /^(\d+(\.\d+)?)x$/;
const GiB = 2 ** 30;

/** A step that is not a factor, in bytes: GiB as a bare number or "3 GiB", else any quantity the API takes, such as 2048Mi; NaN if none. */
function stepBytes(st: string): number {
  const gib = /^(\d+(\.\d+)?)\s*(GiB)?$/.exec(st);
  return gib ? Number(gib[1]) * GiB : parseQuantity(st);
}

/** The form's fields from a stored DiskGrow, its max in whole GiB. */
export function diskGrowOf(g: { max: string; threshold: string; step: string } | undefined, gib: (q: string) => number): DiskGrow {
  if (!g) return noDiskGrow;
  return {
    on: true,
    maxGi: g.max ? String(gib(g.max)) : "",
    threshold: g.threshold.replace(/%$/, ""),
    step: g.step.replace(/Gi$/, " GiB"),
  };
}

/** What the fields amount to as spec.resources.diskGrow, and what keeps them from being sent. */
export function diskGrowSpec(g: DiskGrow, diskGi: number): { spec?: { max: string; threshold: string; step: string }; problems: string[] } {
  if (!g.on) return { problems: [] };
  const problems: string[] = [];
  const max = Number(g.maxGi.trim());
  if (g.maxGi.trim() === "" || !Number.isInteger(max) || max < 1) problems.push("Grow up to: a whole number of GiB.");
  else if (Number.isFinite(diskGi) && max < diskGi) problems.push(`Grow up to: ${max} GiB is less than the disk's ${diskGi} GiB.`);
  const t = g.threshold.trim();
  const tn = Number(t);
  if (t !== "" && (!Number.isInteger(tn) || tn < 50 || tn > 95)) problems.push("When full: a whole percentage from 50 to 95.");
  const st = g.step.trim();
  let step = "";
  if (st !== "") {
    const f = factor.exec(st);
    const bytes = f ? NaN : stepBytes(st);
    if (f) {
      const n = Number(f[1]);
      if (n <= 1 || n > 16) problems.push("Grow by: a factor over 1x and at most 16x.");
      step = st;
    } else if (Number.isFinite(bytes)) {
      if (bytes < GiB) problems.push("Grow by: at least 1 GiB.");
      // A bare number or "n GiB" is the form's way of writing nGi; anything else goes as typed.
      const gib = /^(\d+(\.\d+)?)\s*(GiB)?$/.exec(st);
      step = gib ? `${gib[1]}Gi` : st;
    } else problems.push("Grow by: a factor such as 2x, or GiB such as 3.");
  }
  return { spec: { max: `${max}Gi`, threshold: t === "" ? "" : `${t}%`, step }, problems };
}

/** The sizes the disk takes as it grows, for the person to see what they chose: 2, 4, 8, 16, 20. */
export function growthSteps(g: DiskGrow, diskGi: number): number[] {
  const max = Number(g.maxGi);
  if (!g.on || !Number.isInteger(max) || !Number.isFinite(diskGi) || diskGi < 1 || max <= diskGi) return [];
  const st = g.step.trim() || diskGrowDefaults.step;
  const f = factor.exec(st);
  const stepGi = f ? NaN : stepBytes(st) / GiB;
  const out = [diskGi];
  let size = diskGi;
  while (size < max && out.length < 12) {
    const next = Math.ceil(f ? size * Number(f[1]) : size + stepGi);
    if (!Number.isFinite(next) || next <= size) break;
    size = Math.min(next, max);
    out.push(size);
  }
  return out;
}

export function DiskGrowFields({
  value: g,
  onChange,
  diskGi,
  disabled,
  apiPath = "spec.resources.diskGrow",
  indent = true,
  note = "Grows while it runs, no restart.",
}: {
  value: DiskGrow;
  onChange: (g: DiskGrow) => void;
  diskGi: number;
  disabled?: boolean;
  /** The field as the manifest names it. */
  apiPath?: string;
  /** Lines the fields up under a labelled row, as the machine form's disk row is. */
  indent?: boolean;
  /** When it grows, the first sentence of the explanation. */
  note?: string;
}) {
  const set = (patch: Partial<DiskGrow>) => onChange({ ...g, ...patch });
  const { problems } = diskGrowSpec(g, diskGi);
  const steps = growthSteps(g, diskGi);
  return (
    <div className={cn("mt-2.5", indent && "sm:pl-[108px]")}>
      <label className="flex items-center gap-2 text-[12.5px]">
        <input
          type="checkbox"
          checked={g.on}
          disabled={disabled}
          // The max starts where a growth shows: 20 GiB, or twice a disk already that large.
          onChange={(e) => set({ on: e.target.checked, maxGi: g.maxGi || (Number.isFinite(diskGi) ? String(Math.max(diskGi * 2, 20)) : "") })}
          className="size-3.5 accent-[var(--accent)]"
        />
        <span className="font-medium">Grow by itself as it fills</span>
        <span className="hidden font-mono text-[11px] text-subtle sm:inline">{apiPath}</span>
      </label>
      {g.on && (
        <div className="mt-2 space-y-2 rounded-md border bg-background px-3 py-2.5">
          <div className="flex flex-wrap items-center gap-x-4 gap-y-2 text-[12.5px]">
            <Field label="up to" suffix="GiB" ariaLabel="Grow up to, GiB" value={g.maxGi} placeholder="20" onChange={(maxGi) => set({ maxGi })} width="w-20" />
            <Field
              label="when"
              suffix="% full"
              ariaLabel="Grow when full, percent"
              value={g.threshold}
              placeholder={String(diskGrowDefaults.threshold)}
              onChange={(threshold) => set({ threshold })}
              width="w-16"
            />
            <Field label="by" ariaLabel="Grow by" value={g.step} placeholder={diskGrowDefaults.step} onChange={(step) => set({ step })} width="w-20" />
          </div>
          <p className="text-[11.5px] text-muted">
            {note} Each step counts toward the disk quota as it is taken; a step past quota waits, and the disk stays as it is. Grow
            by a factor such as <span className="font-mono">2x</span>, or by GiB such as <span className="font-mono">3</span>.
          </p>
          {steps.length > 1 && (
            <p className="font-mono text-[11.5px] tabular text-subtle" aria-label="Growth steps">
              {steps.map((n) => `${n}`).join(" → ")} GiB
            </p>
          )}
          {problems.map((p) => (
            <p key={p} className="text-[12px] text-fail">
              {p}
            </p>
          ))}
        </div>
      )}
    </div>
  );
}

function Field({
  label,
  suffix,
  ariaLabel,
  value,
  placeholder,
  onChange,
  width,
}: {
  label: string;
  suffix?: string;
  ariaLabel: string;
  value: string;
  placeholder: string;
  onChange: (v: string) => void;
  width: string;
}) {
  return (
    <span className="flex items-center gap-1.5">
      <span className="text-muted">{label}</span>
      <Input value={value} placeholder={placeholder} aria-label={ariaLabel} onChange={(e) => onChange(e.target.value)} className={cn("h-7 tabular", width)} />
      {suffix && <span className="text-muted">{suffix}</span>}
    </span>
  );
}
