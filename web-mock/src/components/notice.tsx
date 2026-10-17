// Notices say what is the case, in two places only:
//  - inline, next to the action it concerns (above a form's submit button),
//    when that action would fail;
//  - as a page banner under the top bar, for a state of the whole project or
//    platform.
// The copy states facts. It does not tell the reader what to do next.

import { AlertTriangle, Info, OctagonAlert } from "lucide-react";
import type { ReactNode } from "react";
import { cn } from "@/lib/cn";

export type NoticeTone = "info" | "warning" | "error";

const tones: Record<NoticeTone, { icon: typeof Info; box: string; iconClass: string }> = {
  info: { icon: Info, box: "border-border-strong bg-surface-2", iconClass: "text-muted" },
  warning: {
    icon: AlertTriangle,
    box: "border-[color-mix(in_oklch,var(--pending)_45%,transparent)] bg-[color-mix(in_oklch,var(--pending)_9%,transparent)]",
    iconClass: "text-pending",
  },
  error: {
    icon: OctagonAlert,
    box: "border-[color-mix(in_oklch,var(--fail)_45%,transparent)] bg-[color-mix(in_oklch,var(--fail)_8%,transparent)]",
    iconClass: "text-fail",
  },
};

export function Notice({
  tone,
  title,
  children,
  className,
}: {
  tone: NoticeTone;
  title: ReactNode;
  children?: ReactNode;
  className?: string;
}) {
  const t = tones[tone];
  const Icon = t.icon;
  return (
    <div role={tone === "error" ? "alert" : "status"} className={cn("flex gap-2.5 rounded-md border px-3 py-2.5", t.box, className)}>
      <Icon className={cn("mt-px size-4 shrink-0", t.iconClass)} strokeWidth={1.9} />
      <div className="min-w-0 text-[12.5px] leading-snug">
        <div className="font-medium text-foreground">{title}</div>
        {children && <div className="mt-0.5 text-muted">{children}</div>}
      </div>
    </div>
  );
}

/** The page-level slot, directly under the top bar. */
export function PageBanner({ tone, title, children }: { tone: NoticeTone; title: ReactNode; children?: ReactNode }) {
  return (
    <div className="px-4 pt-4 md:px-8">
      <Notice tone={tone} title={title}>
        {children}
      </Notice>
    </div>
  );
}
