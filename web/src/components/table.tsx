// The pieces of a list page's table: its header cells, its rows, and the
// row a filter leaves when nothing matches.

import type { ReactNode } from "react";
import { cn } from "@/lib/cn";

/** A table header cell. */
export function Th({ children, className }: { children?: ReactNode; className?: string }) {
  return <th className={cn("px-4 py-2 font-medium", className)}>{children}</th>;
}

export const theadRow = "border-b bg-surface-2/60 text-left text-[11.5px] text-subtle";
export const tbodyRow = "group border-b last:border-0 hover:bg-surface-2/60";

/** The row a filter leaves when nothing matches: a table row with colSpan, else a list item. */
export function NoMatch({ noun, colSpan }: { noun: string; colSpan?: number }) {
  return colSpan ? (
    <tr>
      <td colSpan={colSpan} className="px-4 py-10 text-center text-muted">
        No {noun} matches.
      </td>
    </tr>
  ) : (
    <li className="px-4 py-8 text-center text-[13px] text-muted">No {noun} matches.</li>
  );
}
