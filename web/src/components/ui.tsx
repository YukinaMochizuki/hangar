// Small building blocks in the shadcn/ui style: cva variants over the
// design tokens in styles.css, kept in the repo rather than generated.

import { cva, type VariantProps } from "class-variance-authority";
import { Check, Copy } from "lucide-react";
import { useState, type ButtonHTMLAttributes, type HTMLAttributes, type ReactNode } from "react";
import { cn } from "@/lib/cn";
import { MachinePhase } from "@hangar/api/hangar/compute/v1alpha1/machine_pb";

export const buttonVariants = cva(
  "inline-flex items-center justify-center gap-1.5 whitespace-nowrap rounded-md text-[13px] font-medium transition-colors focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring disabled:opacity-50 [&_svg]:size-3.5 [&_svg]:shrink-0",
  {
    variants: {
      variant: {
        primary: "bg-accent text-accent-fg hover:opacity-90",
        secondary: "border border-border-strong bg-surface text-foreground hover:bg-surface-2",
        ghost: "text-muted hover:bg-surface-2 hover:text-foreground",
        danger: "border border-border-strong bg-surface text-fail hover:bg-surface-2",
      },
      size: { sm: "h-7 px-2.5", md: "h-8 px-3", icon: "size-8" },
    },
    defaultVariants: { variant: "secondary", size: "md" },
  },
);

export function Button({
  className,
  variant,
  size,
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & VariantProps<typeof buttonVariants>) {
  return <button className={cn(buttonVariants({ variant, size }), className)} {...props} />;
}

export function Card({ className, ...props }: HTMLAttributes<HTMLDivElement>) {
  return <div className={cn("rounded-lg border bg-surface", className)} {...props} />;
}

export function CardHeader({ title, action, className }: { title: ReactNode; action?: ReactNode; className?: string }) {
  return (
    <div className={cn("flex h-11 items-center justify-between border-b px-4", className)}>
      <h3 className="text-[13px] font-medium">{title}</h3>
      {action}
    </div>
  );
}

export function Badge({ className, ...props }: HTMLAttributes<HTMLSpanElement>) {
  return (
    <span
      className={cn(
        "inline-flex h-5 items-center rounded border border-border-strong px-1.5 text-[11px] font-medium text-muted",
        className,
      )}
      {...props}
    />
  );
}

export function SizeBadge({ size }: { size: string }) {
  return (
    <span className="inline-flex h-5 min-w-6 items-center justify-center rounded border border-border-strong bg-surface-2 px-1.5 font-mono text-[11px] font-semibold text-foreground">
      {size}
    </span>
  );
}

/** What a state LED says: fine, on its way, failed, or nothing going on. */
export type Tone = "ok" | "pending" | "fail" | "off";

const tones: Record<Tone, string> = { ok: "bg-ok", pending: "bg-pending", fail: "bg-fail", off: "bg-off" };

/** Led is a state's light; a pulse marks a state on its way, and stays still when reduced motion is asked for. */
export function Led({ tone, pulse, className }: { tone: Tone; pulse?: boolean; className?: string }) {
  return (
    <span className={cn("relative inline-flex size-2 shrink-0", className)}>
      {pulse && <span className={cn("absolute inset-0 animate-ping rounded-full opacity-60 motion-reduce:hidden", tones[tone])} />}
      <span className={cn("relative size-full rounded-full", tones[tone])} />
    </span>
  );
}

const phases: Record<number, { label: string; tone: Tone; pulse?: boolean }> = {
  [MachinePhase.RUNNING]: { label: "Running", tone: "ok" },
  [MachinePhase.PENDING]: { label: "Pending", tone: "pending", pulse: true },
  [MachinePhase.CREATING]: { label: "Starting", tone: "pending", pulse: true },
  [MachinePhase.STOPPED]: { label: "Stopped", tone: "off" },
  [MachinePhase.FAILED]: { label: "Failed", tone: "fail" },
  [MachinePhase.SUSPENDED]: { label: "Suspended", tone: "off" },
};

export function phaseLabel(phase: MachinePhase) {
  return phases[phase]?.label ?? "Unknown";
}

/** StatusDot is a machine phase's LED. */
export function StatusDot({ phase, className }: { phase: MachinePhase; className?: string }) {
  const p: { tone: Tone; pulse?: boolean } = phases[phase] ?? { tone: "off" };
  return <Led tone={p.tone} pulse={p.pulse} className={className} />;
}

export function Status({ phase }: { phase: MachinePhase }) {
  return (
    <span className="inline-flex items-center gap-2 text-[13px]">
      <StatusDot phase={phase} />
      {phaseLabel(phase)}
    </span>
  );
}

/**
 * CopyButton copies text to the clipboard. The check shows only once the
 * clipboard took it; when it did not, the button says so beside it.
 */
export function CopyButton({ text, className }: { text: string; className?: string }) {
  const [state, setState] = useState<"idle" | "done" | "failed">("idle");
  return (
    <span className={cn("inline-flex items-center gap-1.5", className)}>
      {state === "failed" && (
        <span role="status" className="whitespace-nowrap text-[11.5px] text-fail">
          Not copied: the clipboard is unavailable.
        </span>
      )}
      <button
        type="button"
        aria-label="Copy"
        className="rounded p-1 text-subtle hover:bg-surface-2 hover:text-foreground"
        onClick={async () => {
          try {
            if (!navigator.clipboard) throw new Error("no clipboard");
            await navigator.clipboard.writeText(text);
            setState("done");
            setTimeout(() => setState("idle"), 1200);
          } catch {
            setState("failed");
          }
        }}
      >
        {state === "done" ? <Check className="size-3.5" aria-label="Copied" /> : <Copy className="size-3.5" />}
      </button>
    </span>
  );
}

/** A key and its value, the value in monospace when it is an identifier. */
export function Field({ label, children, mono }: { label: string; children: ReactNode; mono?: boolean }) {
  return (
    <div className="grid grid-cols-[96px_1fr] items-baseline gap-3 py-2 text-[13px] sm:grid-cols-[132px_1fr]">
      <dt className="text-muted">{label}</dt>
      <dd className={cn("min-w-0 break-all", mono && "font-mono text-[12.5px]")}>{children}</dd>
    </div>
  );
}

export function Kbd({ children }: { children: ReactNode }) {
  return (
    <kbd className="inline-flex h-5 min-w-5 items-center justify-center rounded border border-border-strong bg-surface-2 px-1 font-mono text-[10.5px] text-muted">
      {children}
    </kbd>
  );
}
