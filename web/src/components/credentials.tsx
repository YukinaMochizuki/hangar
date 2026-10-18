// Pieces the Secrets and Service tokens pages share: the banner over a list
// that did not load again, the command line beside an action, and form
// labels.

import type { ReactNode } from "react";
import { PageBanner } from "@/components/notice";
import { Button, CopyButton } from "@/components/ui";
import { errorMessage } from "@/lib/api";

/**
 * StaleBanner says a list that loaded before did not load again, while the
 * page goes on showing its last answer, and whatever dialog is open on it.
 */
export function StaleBanner({ what, error, onRetry }: { what: string; error: unknown; onRetry: () => void }) {
  return (
    <PageBanner tone="warning" title={`The ${what} did not load again; this page shows the last answer.`}>
      <span className="flex flex-wrap items-center justify-between gap-2">
        <span className="font-mono text-[12px]">{errorMessage(error)}</span>
        <Button size="sm" onClick={onRetry}>
          Retry
        </Button>
      </span>
    </PageBanner>
  );
}

/** A command line with its copy button, beside the action it does the same as. */
export function CommandLine({ command, className }: { command: string; className?: string }) {
  return (
    <div className={`mt-4 flex items-center gap-2 rounded-md border bg-surface-2 py-1.5 pl-2.5 pr-1.5 ${className ?? ""}`}>
      <code className="min-w-0 flex-1 break-all font-mono text-[11.5px] text-muted">
        <span className="select-none text-accent">$ </span>
        {command}
      </code>
      <CopyButton text={command} className="shrink-0" />
    </div>
  );
}

// Every field of these forms is in the monospace face, as names and values are.
export const inputClass =
  "mt-1.5 block h-9 w-full rounded-md border border-border-strong bg-background px-2.5 font-mono text-[13px] text-foreground outline-none placeholder:text-subtle focus:border-accent";

export function FormLabel({ title, children, hint }: { title: ReactNode; children: ReactNode; hint?: ReactNode }) {
  return (
    <div className="mt-4 first:mt-0">
      <label className="block">
        <span className="text-[12.5px] text-foreground">{title}</span>
        {children}
      </label>
      {hint && <span className="mt-1 block text-[11.5px] text-subtle">{hint}</span>}
    </div>
  );
}
