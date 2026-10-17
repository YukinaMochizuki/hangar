import { cn } from "@/lib/cn";

/** A quota bar: neutral until it nears the limit, then the pending colour, then fail. */
export function Meter({ used, limit, className }: { used: number; limit: number; className?: string }) {
  const ratio = limit > 0 ? Math.min(used / limit, 1) : 0;
  const tone = ratio >= 1 ? "bg-fail" : ratio >= 0.8 ? "bg-pending" : "bg-foreground/70";
  return (
    <div className={cn("h-1 overflow-hidden rounded-full bg-surface-2", className)}>
      <div className={cn("h-full rounded-full", tone)} style={{ width: `${ratio * 100}%` }} />
    </div>
  );
}
