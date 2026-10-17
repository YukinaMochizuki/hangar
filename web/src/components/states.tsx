// The states a page or a card can be in besides full: loading, failed and
// empty. Each says what is the case and nothing more.

import type { ReactNode } from "react";
import { HangarMark } from "@/components/marks";
import { Notice } from "@/components/notice";
import { ApronArt } from "@/components/taxiway";
import { Button } from "@/components/ui";
import { errorMessage } from "@/lib/api";
import { cn } from "@/lib/cn";

/** A grey bar where text will be. */
export function Skeleton({ className }: { className?: string }) {
  return <span className={cn("block animate-pulse rounded bg-surface-2 motion-reduce:animate-none", className)} />;
}

/** Rows of skeletons, for a list or table while it loads. */
export function SkeletonRows({ rows = 4, className }: { rows?: number; className?: string }) {
  return (
    <ul aria-busy="true" aria-label="Loading" className={cn("divide-y", className)}>
      {Array.from({ length: rows }, (_, i) => (
        <li key={i} className="flex items-center gap-3 px-4 py-3">
          <Skeleton className="size-2 rounded-full" />
          <Skeleton className="h-3 w-32" />
          <Skeleton className="ml-auto h-3 w-20" />
        </li>
      ))}
    </ul>
  );
}

/** An API error, inline where the data would be. */
export function ErrorNotice({ error, title, onRetry, className }: { error: unknown; title: string; onRetry?: () => void; className?: string }) {
  return (
    <div className={cn("p-4", className)}>
      <Notice tone="error" title={title}>
        <div className="flex flex-wrap items-center justify-between gap-2">
          <span className="font-mono text-[12px]">{errorMessage(error)}</span>
          {onRetry && (
            <Button size="sm" onClick={onRetry}>
              Retry
            </Button>
          )}
        </div>
      </Notice>
    </div>
  );
}

/** An empty list: the taxi line runs into the hangar. */
export function EmptyState({ title, children, action }: { title: string; children?: ReactNode; action?: ReactNode }) {
  return (
    <div className="flex flex-col items-center px-6 pb-10 pt-6 text-center">
      <ApronArt className="h-36 w-full max-w-md" />
      <div className="mt-2 text-[14px] font-medium">{title}</div>
      {children && <div className="mt-1 max-w-sm text-[13px] text-muted">{children}</div>}
      {action && <div className="mt-4">{action}</div>}
    </div>
  );
}

/** A page of its own, outside the shell: sign-in, waiting, signup. */
export function StandalonePage({ title, children, footer }: { title: ReactNode; children: ReactNode; footer?: ReactNode }) {
  return (
    <div className="flex min-h-screen flex-col items-center justify-center bg-background px-4 py-10">
      <div className="mb-6 flex items-center gap-2">
        <HangarMark className="size-7 text-foreground" />
        <span className="text-[18px] font-semibold tracking-[-0.02em]">Hangar</span>
      </div>
      <div className="w-full max-w-[400px] rounded-lg border bg-surface p-6 shadow-sm">
        <h1 className="text-[16px] font-semibold tracking-tight">{title}</h1>
        <div className="mt-3 text-[13px] leading-relaxed text-muted">{children}</div>
      </div>
      {footer && <div className="mt-4 text-[12px] text-subtle">{footer}</div>}
    </div>
  );
}
