// The New image page's source disk: one radio list of the project's disks,
// those that can be captured now first, boot disks before data disks, with a
// filter by name or image. Disks that cannot be captured are folded away
// behind a count until asked for, each saying why in its row, so a project
// with many disks still shows the few it can use at a glance.

import type { Disk } from "@hangar/api/hangar/storage/v1alpha1/disk_pb";
import { Check, ChevronDown, HardDrive, Power, Search } from "lucide-react";
import { useState } from "react";
import { Led } from "@/components/ui";
import { cn } from "@/lib/cn";
import { captureRefusal, type CaptureRefusal } from "@/lib/custom-images";
import type { MachineView } from "@/lib/machines";
import { diskCustomImage, diskImage, gib, usedAs } from "@/lib/storage";

type Row = { d: Disk; name: string; refusal?: CaptureRefusal; boot: boolean; meta: string };

function rowOf(d: Disk, machines: MachineView[]): Row {
  const use = usedAs(d);
  const from = diskImage(d) ?? diskCustomImage(d);
  const parts = [gib(d.status?.size || d.spec?.size)];
  if (from) parts.push(from);
  if (use) parts.push(`${use.role === "boot" ? "boot disk" : use.role === "data" ? "data disk" : "volume"} of ${use.name}`);
  return { d, name: d.metadata?.name ?? "", refusal: captureRefusal(d, machines), boot: use?.role === "boot" || !!from, meta: parts.join(" · ") };
}

const order = (a: Row, b: Row) => Number(!!a.refusal) - Number(!!b.refusal) || Number(b.boot) - Number(a.boot) || a.name.localeCompare(b.name);

function DiskRow({ r, selected, onSelect }: { r: Row; selected: boolean; onSelect: () => void }) {
  const Icon = r.boot ? Power : HardDrive;
  return (
    <li>
      <button
        type="button"
        role="radio"
        aria-checked={selected}
        aria-label={r.name}
        disabled={!!r.refusal}
        title={r.refusal?.reason}
        onClick={onSelect}
        className={cn(
          "flex w-full items-center gap-3 px-3 py-2.5 text-left transition-colors disabled:cursor-default",
          selected ? "bg-accent-soft" : "enabled:hover:bg-surface-2",
        )}
      >
        <span
          className={cn(
            "flex size-8 shrink-0 items-center justify-center rounded-md border",
            selected ? "border-accent bg-accent text-accent-fg" : "bg-surface-2 text-muted",
            r.refusal && "opacity-50",
          )}
        >
          {selected ? <Check className="size-4" strokeWidth={2.5} /> : <Icon className="size-4" strokeWidth={1.75} />}
        </span>
        <span className={cn("min-w-0 flex-1", r.refusal && "opacity-60")}>
          <span className="block truncate font-mono text-[12.5px] font-medium">{r.name}</span>
          <span className="block truncate text-[12px] text-muted">{r.meta}</span>
        </span>
        <span className="flex shrink-0 items-center gap-1.5 text-[12px] text-muted">
          <Led tone={r.refusal ? (r.refusal.label === "Never used" ? "off" : "pending") : "ok"} />
          <span className="hidden sm:inline">{r.refusal ? r.refusal.label : "Can capture"}</span>
        </span>
      </button>
    </li>
  );
}

/** SourceDiskPicker chooses the disk a custom image is captured from. */
export function SourceDiskPicker({ disks, machines, value, onChange }: { disks: Disk[]; machines: MachineView[]; value: string; onChange: (name: string) => void }) {
  const [query, setQuery] = useState("");
  const [showAll, setShowAll] = useState(false);
  const rows = disks.map((d) => rowOf(d, machines)).sort(order);
  const q = query.trim().toLowerCase();
  const matching = rows.filter((r) => !q || r.name.toLowerCase().includes(q) || r.meta.toLowerCase().includes(q));
  const usable = matching.filter((r) => !r.refusal);
  const refused = matching.filter((r) => r.refusal);
  // The chosen disk stays in sight even when folded away or filtered out.
  const chosenHidden = !matching.some((r) => r.name === value) || (!showAll && refused.some((r) => r.name === value));
  const chosen = rows.find((r) => r.name === value);

  return (
    <div className="max-w-2xl">
      <div className="mb-2 flex flex-wrap items-center gap-3">
        <label className="flex h-8 min-w-0 flex-1 items-center gap-2 rounded-md border bg-surface px-2.5 text-[13px] focus-within:border-border-strong sm:max-w-72">
          <Search className="size-3.5 text-subtle" />
          <input
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="Disk, image or machine"
            aria-label="Filter disks"
            className="min-w-0 flex-1 bg-transparent outline-none placeholder:text-subtle"
          />
        </label>
        <span className="text-[12px] text-subtle tabular">
          {rows.filter((r) => !r.refusal).length} of {rows.length} can be captured now
        </span>
      </div>
      <div className="overflow-hidden rounded-lg border bg-surface">
        <ul role="radiogroup" aria-label="Source disk" className="max-h-[22rem] divide-y overflow-y-auto">
          {chosen && chosenHidden && <DiskRow r={chosen} selected onSelect={() => onChange(chosen.name)} />}
          {usable.map((r) => (
            <DiskRow key={r.name} r={r} selected={r.name === value} onSelect={() => onChange(r.name)} />
          ))}
          {usable.length === 0 && (
            <li className="px-3 py-4 text-[13px] text-muted">
              {q ? "No disk that can be captured matches." : "No disk can be captured now: stop the machine whose disk you want first."}
            </li>
          )}
          {showAll && refused.map((r) => <DiskRow key={r.name} r={r} selected={r.name === value} onSelect={() => onChange(r.name)} />)}
        </ul>
        {refused.length > 0 && (
          <button
            type="button"
            onClick={() => setShowAll((v) => !v)}
            aria-expanded={showAll}
            className="flex w-full items-center gap-1.5 border-t bg-surface-2 px-3 py-2 text-[12px] text-muted hover:text-foreground"
          >
            <ChevronDown className={cn("size-3.5 transition-transform", showAll && "rotate-180")} />
            {showAll ? "Hide" : "Show"} {refused.length} that cannot be captured now
          </button>
        )}
      </div>
    </div>
  );
}
