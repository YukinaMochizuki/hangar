// A form's pieces: its text inputs and selects, a labelled field, the
// manifest and command a form amounts to, and a command line beside an
// action.

import { cva, type VariantProps } from "class-variance-authority";
import type { ComponentProps, ReactNode } from "react";
import { CodeBlock } from "@/components/object";
import { CopyButton } from "@/components/ui";
import { cn } from "@/lib/cn";

// Names and values are identifiers, so a field is in the monospace face; a
// field of prose sets font-sans.
const fieldVariants = cva(
  "block h-9 w-full rounded-md border border-border-strong bg-background px-2.5 font-mono text-[13px] text-foreground outline-none placeholder:text-subtle",
  {
    variants: {
      // danger is the field that confirms a delete.
      tone: { default: "focus:border-accent", danger: "focus:border-fail" },
    },
    defaultVariants: { tone: "default" },
  },
);

type FieldProps = VariantProps<typeof fieldVariants>;

/**
 * Input is a text field. These fields hold names, addresses and logins for
 * other systems, never this site's, so it asks browsers and password
 * managers not to fill them; a field that is for the person's own account
 * passes its autoComplete and data-1p-ignore={undefined}.
 */
export function Input({ className, tone, ...props }: ComponentProps<"input"> & FieldProps) {
  return <input autoComplete="off" data-1p-ignore {...props} className={cn(fieldVariants({ tone }), className)} />;
}

/** Textarea is a text field of several lines, filled as Input is. */
export function Textarea({ className, tone, ...props }: ComponentProps<"textarea"> & FieldProps) {
  return <textarea autoComplete="off" data-1p-ignore {...props} className={cn(fieldVariants({ tone }), "h-auto py-2", className)} />;
}

/** Select is a choice among options, drawn as a text field. */
export function Select({ className, tone, ...props }: ComponentProps<"select"> & FieldProps) {
  return <select {...props} className={cn(fieldVariants({ tone }), className)} />;
}

/** A labelled form field, with a hint under it. */
export function FormField({ label, hint, className, children }: { label: ReactNode; hint?: ReactNode; className?: string; children: ReactNode }) {
  return (
    <div className={cn("mt-4 first:mt-0", className)}>
      <label className="block">
        <span className="text-[12.5px] text-foreground">{label}</span>
        <span className="mt-1.5 block">{children}</span>
      </label>
      {hint && <span className="mt-1 block text-[11.5px] text-subtle">{hint}</span>}
    </div>
  );
}

/**
 * A form's equivalent, under its fields: the manifest it applies and the
 * command that applies it, each copyable, or either alone.
 */
export function FormEquivalent({ yaml, cli }: { yaml?: string; cli?: string[] }) {
  return (
    <div className="mt-4 overflow-hidden rounded-md border bg-surface-2/60">
      <div className="flex h-8 items-center justify-between border-b px-3 text-[11.5px] text-subtle">
        {yaml !== undefined ? "Equivalent manifest" : "CLI"}
        <CopyButton text={yaml ?? cli?.join("\n") ?? ""} />
      </div>
      {yaml !== undefined && <pre className="max-h-48 overflow-auto px-3 py-2 font-mono text-[11.5px] leading-5 text-muted">{yaml}</pre>}
      {cli &&
        (yaml === undefined ? (
          <CodeBlock cli={cli} className="px-3 py-2 text-[11.5px]" />
        ) : (
          <div className="flex items-start gap-2 border-t pr-1.5">
            <CodeBlock cli={cli} className="min-w-0 flex-1 px-3 py-2 text-[11.5px]" />
            <CopyButton text={cli.join("\n")} className="mt-1 shrink-0" />
          </div>
        ))}
    </div>
  );
}

/** A command line with its copy button, beside the action it does the same as. */
export function CommandLine({ command, className }: { command: string; className?: string }) {
  return (
    <div className={cn("mt-4 flex items-center gap-2 rounded-md border bg-surface-2 py-1.5 pl-2.5 pr-1.5", className)}>
      <code className="min-w-0 flex-1 break-all font-mono text-[11.5px] text-muted">
        <span className="select-none text-accent">$ </span>
        {command}
      </code>
      <CopyButton text={command} className="shrink-0" />
    </div>
  );
}
