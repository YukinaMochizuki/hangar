// A machine's memory in its tenant's pool, under the size on the machine
// form: Dedicated, all of its memory reserved, or Shared, part of it
// reserved and the rest shared with the tenant's other machines
// (docs/decisions/memory-pool.md). Drawn only for a tenant with a pool.

import { Input } from "@/components/form";
import { gibText } from "@/components/size-sliders";
import { cn } from "@/lib/cn";
import { defaultReservedMiB } from "@/lib/memory-pool";

export type Share = "dedicated" | "shared";

const options: { id: Share; label: string }[] = [
  { id: "dedicated", label: "Dedicated" },
  { id: "shared", label: "Shared" },
];

export function MemoryShare({
  memMiB,
  percent,
  tenant,
  share,
  reservedMiB,
  typed,
  problem,
  onShare,
  onReserved,
}: {
  memMiB: number;
  /** The plan's memory_limit_percent, which the default reservation divides memory by. */
  percent: number;
  /** The tenant's name as people read it. */
  tenant: string;
  share: Share;
  /** What is reserved, as sent. */
  reservedMiB: number;
  /** The reservation as typed, once the person opened the field; null follows the default. */
  typed: string | null;
  /** Why the typed reservation is not sent. */
  problem?: string;
  onShare: (s: Share) => void;
  /** A reservation typed in GiB; null goes back to the default. */
  onReserved: (text: string | null) => void;
}) {
  const mem = gibText(memMiB);
  const r = gibText(reservedMiB);
  return (
    <div className="mt-3 rounded-lg border bg-surface px-4 py-3.5">
      <div className="grid grid-cols-[1fr_auto] items-center gap-x-4 gap-y-2 sm:grid-cols-[56px_auto_minmax(0,1fr)_auto]">
        <span className="text-[12.5px] text-muted">Memory</span>
        <div role="radiogroup" aria-label="Memory" className="order-last col-span-2 flex h-8 w-fit items-center rounded-md border bg-surface p-0.5 sm:order-none sm:col-span-1">
          {options.map((o) => (
            <button
              key={o.id}
              type="button"
              role="radio"
              aria-checked={share === o.id}
              onClick={() => onShare(o.id)}
              className={cn("flex h-full items-center rounded px-3 text-[12.5px]", share === o.id ? "bg-surface-2 font-medium text-foreground" : "text-muted hover:text-foreground")}
            >
              {o.label}
            </button>
          ))}
        </div>
        <span className="hidden sm:block" />
        <span className="text-right font-mono text-[12.5px] tabular">
          {share === "dedicated" ? mem : `${r}–${mem}`} <span className="text-subtle">GiB</span>
        </span>
      </div>
      <p className="mt-2.5 text-[12.5px] text-muted sm:pl-[72px]">
        {share === "dedicated" ? `All ${mem} GiB reserved.` : `${r} GiB reserved, up to ${mem} GiB while ${tenant}'s other machines leave memory free.`}
      </p>
      {share === "shared" && (
        <div className="mt-2 flex flex-wrap items-center gap-x-3 gap-y-2 text-[12.5px] sm:pl-[72px]">
          {typed !== null ? (
            <>
              <span className="text-muted">Reserved</span>
              <span className="flex items-center gap-1.5">
                <Input
                  value={typed}
                  onChange={(e) => onReserved(e.target.value)}
                  inputMode="decimal"
                  aria-label="Reserved memory, GiB"
                  aria-invalid={!!problem}
                  className={cn("h-7 w-16 px-2 text-right text-[12.5px] tabular", problem && "border-fail focus:border-fail")}
                />
                <span className="text-[11.5px] text-subtle">GiB</span>
              </span>
              <span className={cn("text-[12px]", problem ? "text-fail" : "text-subtle")}>
                {problem ?? `In 256 MiB steps, up to ${mem} GiB. ${gibText(defaultReservedMiB(memMiB, percent))} GiB unless changed.`}
              </span>
            </>
          ) : (
            <button type="button" onClick={() => onReserved(r)} className="text-[12px] text-accent hover:underline">
              Change reserved memory
            </button>
          )}
        </div>
      )}
    </div>
  );
}
