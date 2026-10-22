// The frame of a page: a list page's head and name filter, the frames that
// hold a list's or an object's loading, failed and empty states, and an
// object page's head and columns.

import { Search } from "lucide-react";
import { useEffect, useRef, type ReactNode } from "react";
import { Topbar } from "@/components/shell";
import { EmptyState, ErrorNotice, Skeleton, SkeletonRows } from "@/components/states";
import { Card, Kbd } from "@/components/ui";
import { cn } from "@/lib/cn";

/** A list page's title, what the kind is in a few words, and its create button. */
export function PageHead({ title, description, action }: { title: string; description?: ReactNode; action?: ReactNode }) {
  return (
    <div className="flex flex-wrap items-end justify-between gap-4 px-4 pb-4 pt-6 md:px-8">
      <div>
        <h1 className="text-[22px] font-semibold tracking-tight">{title}</h1>
        {description && <p className="mt-1 max-w-2xl text-[13px] text-muted">{description}</p>}
      </div>
      {action}
    </div>
  );
}

/** The name filter; "/" puts the cursor in it. */
export function NameFilter({
  value,
  onChange,
  placeholder = "Name",
  label = "Filter by name",
  className,
}: {
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
  label?: string;
  className?: string;
}) {
  const ref = useRef<HTMLInputElement>(null);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const t = e.target as HTMLElement;
      if (e.key === "/" && !["INPUT", "TEXTAREA", "SELECT"].includes(t.tagName)) {
        e.preventDefault();
        ref.current?.focus();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, []);
  return (
    <label
      className={cn(
        "mb-3 flex h-8 w-full items-center gap-2 rounded-md border bg-surface px-2.5 text-[13px] focus-within:border-border-strong sm:w-64",
        className,
      )}
    >
      <Search className="size-3.5 text-subtle" />
      <input
        ref={ref}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={placeholder}
        aria-label={label}
        className="min-w-0 flex-1 bg-transparent outline-none placeholder:text-subtle"
      />
      <Kbd>/</Kbd>
    </label>
  );
}

/** A list page's table while it loads. */
export function LoadingTable() {
  return (
    <Card aria-busy="true" aria-label="Loading" className="divide-y">
      {Array.from({ length: 4 }, (_, i) => (
        <div key={i} className="flex items-center gap-6 px-4 py-3.5">
          <span className="w-[28%] space-y-1.5">
            <Skeleton className="h-3 w-28" />
            <Skeleton className="h-2.5 w-36" />
          </span>
          <Skeleton className="h-3 w-16" />
          <Skeleton className="hidden h-3 w-20 md:block" />
          <Skeleton className="hidden h-3 w-24 md:block" />
        </div>
      ))}
    </Card>
  );
}

/**
 * How many of a list's objects its table shows, under the table, in line
 * with the text of its first column (the cells' px-4), not with the card's
 * edge.
 */
export function ListCount({ shown, total }: { shown: number; total: number }) {
  return (
    <p className="mt-3 px-4 text-[12px] text-subtle tabular">
      {shown} of {total}
    </p>
  );
}

/** A list's body: loading, failed, empty, or the list itself. */
export function ListFrame({
  state,
  noun,
  empty,
  children,
}: {
  state: { loading: boolean; error: unknown; refetch: () => void; count: number };
  noun: string;
  empty: { title: string; text?: ReactNode; action?: ReactNode };
  children: ReactNode;
}) {
  if (state.loading) return <LoadingTable />;
  if (state.error) {
    return (
      <Card>
        <ErrorNotice error={state.error} title={`The ${noun} did not load`} onRetry={state.refetch} />
      </Card>
    );
  }
  if (state.count === 0) {
    return (
      <Card>
        <EmptyState title={empty.title} action={empty.action}>
          {empty.text}
        </EmptyState>
      </Card>
    );
  }
  return <>{children}</>;
}

/** An object's page while it loads: its head, and its two columns. */
export function DetailSkeleton() {
  return (
    <div aria-busy="true" aria-label="Loading">
      <div className="space-y-3 border-b px-4 pb-5 pt-6 md:px-8">
        <Skeleton className="h-7 w-48" />
        <Skeleton className="h-3 w-64" />
      </div>
      <div className="grid grid-cols-1 gap-5 px-4 py-6 md:px-8 xl:grid-cols-[minmax(0,1fr)_380px]">
        <Card>
          <SkeletonRows rows={5} />
        </Card>
        <Card>
          <SkeletonRows rows={3} />
        </Card>
      </div>
    </div>
  );
}

/** An object's page: loading, failed, not found, or the page. */
export function DetailFrame({
  crumbs,
  state,
  found,
  noun,
  name,
  where = "This project",
  children,
}: {
  crumbs: ReactNode[];
  state: { loading: boolean; error: unknown; refetch: () => void };
  found: boolean;
  noun: string;
  name: string;
  /** What holds the object, as the not-found sentence starts: "This project", "This tenant". */
  where?: string;
  children: ReactNode;
}) {
  return (
    <>
      <Topbar crumbs={crumbs} />
      {state.loading ? (
        <DetailSkeleton />
      ) : state.error ? (
        <div className="px-4 py-6 md:px-8">
          <Card>
            <ErrorNotice error={state.error} title={`The ${noun} did not load`} onRetry={state.refetch} />
          </Card>
        </div>
      ) : !found ? (
        <EmptyState title={`No such ${noun}`}>
          {where} has no {noun} named <span className="font-mono">{name}</span>.
        </EmptyState>
      ) : (
        children
      )}
    </>
  );
}

/** An object page's head: its name, kind, a line of facts, and its actions. */
export function DetailHead({ name, kind, facts, actions }: { name: string; kind: string; facts: ReactNode; actions?: ReactNode }) {
  return (
    <div className="border-b px-4 pb-5 pt-6 md:px-8">
      <div className="flex flex-col gap-4 md:flex-row md:items-start md:justify-between md:gap-6">
        <div className="min-w-0">
          <div className="flex items-center gap-2.5">
            <h1 className="truncate text-[22px] font-semibold tracking-tight">{name}</h1>
            <span className="inline-flex h-5 shrink-0 items-center rounded border border-border-strong px-1.5 text-[11px] font-medium text-muted">
              {kind}
            </span>
          </div>
          <div className="mt-2 flex flex-wrap items-center gap-x-5 gap-y-1 text-[13px] text-muted">{facts}</div>
        </div>
        {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
      </div>
    </div>
  );
}

/** The two columns of an object's page. */
export function DetailBody({ main, side }: { main: ReactNode; side: ReactNode }) {
  return (
    <div className="grid grid-cols-1 gap-5 px-4 py-6 md:px-8 xl:grid-cols-[minmax(0,1fr)_380px]">
      <div className="min-w-0 space-y-5">{main}</div>
      <div className="min-w-0 space-y-5">{side}</div>
    </div>
  );
}
