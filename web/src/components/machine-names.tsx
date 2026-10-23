// A line that names machines names the first two and folds the rest into
// "+N more", which opens a scrollable list of them all, so a line holds
// however many machines there are (docs/decisions/file-shares.md).

import { Link } from "@tanstack/react-router";
import { Box, ChevronDown, Server } from "lucide-react";
import { useState, type ReactNode } from "react";
import { AnchoredLayer, type LayerAlign } from "@/components/menu";
import { useProjectName } from "@/components/shell";
import { cn } from "@/lib/cn";
import { machinePath, type MachineKind } from "@/lib/machines";

/** How many names a line shows before it folds the rest into "+N more". */
export const namesShown = 2;

const KindIcon = ({ kind }: { kind: MachineKind }) =>
  kind === "Container" ? <Box className="size-3.5 shrink-0 text-subtle" strokeWidth={1.75} /> : <Server className="size-3.5 shrink-0 text-subtle" strokeWidth={1.75} />;

/**
 * MachinesPopover is a trigger that opens a scrollable list of machines,
 * each with its kind's icon and address, for a line that names only the
 * first few. The list is an AnchoredLayer, so a card that clips what
 * overflows it does not cut it off. It closes on a click outside or Escape.
 */
export function MachinesPopover({
  machines,
  label,
  title,
  defaultOpen = false,
  align = "left",
  className,
}: {
  machines: { kind: MachineKind; name: string; ipv4?: string }[];
  label: ReactNode;
  title: ReactNode;
  defaultOpen?: boolean;
  align?: LayerAlign;
  className?: string;
}) {
  const project = useProjectName();
  const [open, setOpen] = useState(defaultOpen);
  const [anchor, setAnchor] = useState<HTMLSpanElement | null>(null);
  return (
    <span ref={setAnchor} className={cn("relative inline-flex items-center align-baseline", className)}>
      <button
        type="button"
        aria-expanded={open}
        onClick={(e) => {
          e.stopPropagation();
          setOpen(!open);
        }}
        className="inline-flex h-5 items-center gap-0.5 rounded px-1 font-sans text-[12px] font-medium leading-5 text-accent hover:bg-accent-soft"
      >
        {label}
        <ChevronDown className={cn("size-3 transition-transform", open && "rotate-180")} />
      </button>
      {open && (
        <AnchoredLayer
          anchor={anchor}
          onAway={() => setOpen(false)}
          align={align}
          role="dialog"
          aria-label="Machines"
          className="w-64 overflow-hidden rounded-md border bg-surface text-left font-sans shadow-xl"
        >
          {/* React carries events up its own tree, through the portal: a click here is not the row's. */}
          <span className="block" onClick={(e) => e.stopPropagation()}>
            <span className="block border-b px-3 py-2 text-[11.5px] text-subtle">{title}</span>
            <span className="block max-h-56 overflow-y-auto py-1">
              {machines.map((m) => (
                <Link
                  key={`${m.kind}/${m.name}`}
                  to={machinePath(m.kind)}
                  params={{ project, name: m.name }}
                  className="flex items-center gap-2 px-3 py-1.5 text-[12.5px] hover:bg-surface-2"
                >
                  <KindIcon kind={m.kind} />
                  <span className="min-w-0 flex-1 truncate font-mono text-foreground">{m.name}</span>
                  {m.ipv4 && <span className="font-mono text-[11.5px] text-subtle">{m.ipv4}</span>}
                </Link>
              ))}
            </span>
          </span>
        </AnchoredLayer>
      )}
    </span>
  );
}

/** The first few machines' names, then "+N more" opening the whole list. */
export function CappedNames({
  machines,
  title,
  max = namesShown,
  defaultOpen,
  align,
}: {
  machines: { kind: MachineKind; name: string; ipv4?: string }[];
  title: ReactNode;
  max?: number;
  defaultOpen?: boolean;
  align?: LayerAlign;
}) {
  const shown = machines.slice(0, max);
  const rest = machines.length - shown.length;
  return (
    <>
      <span className="font-mono">{shown.map((m) => m.name).join(", ")}</span>
      {rest > 0 && (
        <>
          {" "}
          <MachinesPopover machines={machines} label={`+${rest} more`} title={title} defaultOpen={defaultOpen} align={align} />
        </>
      )}
    </>
  );
}
