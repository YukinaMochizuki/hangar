// Pieces the network pages share: a list page's head and name filter, the
// frames that hold a list's or an object's loading, failed and empty
// states, an object's conditions, its equivalent CLI and YAML, and delete
// by typed name.

import type { Condition } from "@hangar/api/hangar/meta/v1/meta_pb";
import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import { ChevronRight, Search, Trash2 } from "lucide-react";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { Dialog } from "@/components/dialog";
import { Notice } from "@/components/notice";
import { Topbar, useProjectName } from "@/components/shell";
import { EmptyState, ErrorNotice, Skeleton, SkeletonRows } from "@/components/states";
import { Button, Card, CopyButton, Kbd } from "@/components/ui";
import { errorMessage } from "@/lib/api";
import { cn } from "@/lib/cn";
import { ago, date } from "@/lib/machines";
import { count, type Light } from "@/lib/network";
import { nouns, useDelete, type NetworkKind } from "@/lib/network-actions";

export const inputClass =
  "h-9 w-full rounded-md border border-border-strong bg-background px-2.5 font-mono text-[12.5px] text-foreground outline-none focus:border-accent placeholder:text-subtle";

const lights: Record<Light, string> = { ok: "bg-ok", pending: "bg-pending", fail: "bg-fail", off: "bg-off" };

/** A state's LED. */
export function Led({ light, className }: { light: Light; className?: string }) {
  return <span className={cn("inline-block size-2 shrink-0 rounded-full", lights[light], className)} />;
}

/** What the project's machines say about interfaces: loading, failed, or loaded. */
export type MachinesState = { loading: boolean; error: unknown; refetch: () => void };

/** A count read from the project's machines: a skeleton while they load, unknown if they failed. */
export function MemberCount({ state, n, word }: { state: MachinesState; n: number; word?: string }) {
  if (state.loading) return <Skeleton className="inline-block h-3 w-8 align-middle" />;
  if (state.error) return <span className="text-subtle">unknown</span>;
  return <span className="tabular">{word ? count(n, word) : n}</span>;
}

/** The project's machines failing to load, above what they would have filled in. */
export function MachinesNotice({ state, className }: { state: MachinesState; className?: string }) {
  if (!state.error) return null;
  return (
    <ErrorNotice
      error={state.error}
      title="The project's machines did not load, so which interfaces are on each network is not known"
      onRetry={state.refetch}
      className={cn("px-0 pb-3 pt-0", className)}
    />
  );
}

/**
 * Why a delete the API refuses while machines use the object cannot be
 * judged yet, or the fact that it will be refused; undefined when it can go.
 */
export function usageBlock(state: MachinesState, inUse: string | undefined): string | undefined {
  if (state.loading) return "The project's machines are still loading.";
  if (state.error) return "The project's machines did not load, so whether any uses it is not known.";
  return inUse;
}

/** A list page's title, what the kind is, and its create button. */
export function PageHead({ title, description, action }: { title: string; description: ReactNode; action?: ReactNode }) {
  return (
    <div className="flex flex-wrap items-end justify-between gap-4 px-4 pb-4 pt-6 md:px-8">
      <div>
        <h1 className="text-[22px] font-semibold tracking-tight">{title}</h1>
        <p className="mt-1 max-w-2xl text-[13px] text-muted">{description}</p>
      </div>
      {action}
    </div>
  );
}

/** The name filter; "/" puts the cursor in it. */
export function NameFilter({ value, onChange }: { value: string; onChange: (v: string) => void }) {
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
    <label className="mb-3 flex h-8 w-full items-center gap-2 rounded-md border bg-surface px-2.5 text-[13px] focus-within:border-border-strong sm:w-64">
      <Search className="size-3.5 text-subtle" />
      <input
        ref={ref}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder="Name"
        aria-label="Filter by name"
        className="min-w-0 flex-1 bg-transparent outline-none placeholder:text-subtle"
      />
      <Kbd>/</Kbd>
    </label>
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
  empty: { title: string; text: ReactNode; action?: ReactNode };
  children: ReactNode;
}) {
  if (state.loading) {
    return (
      <Card aria-busy="true" aria-label="Loading" className="divide-y">
        {Array.from({ length: 3 }, (_, i) => (
          <div key={i} className="flex items-center gap-6 px-4 py-3.5">
            <Skeleton className="h-3 w-28" />
            <Skeleton className="h-3 w-24" />
            <Skeleton className="hidden h-3 w-32 md:block" />
            <Skeleton className="hidden h-3 w-16 md:block" />
          </div>
        ))}
      </Card>
    );
  }
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

/** A table header cell. */
export function Th({ children, className }: { children?: ReactNode; className?: string }) {
  return <th className={cn("px-4 py-2 font-medium", className)}>{children}</th>;
}

export const theadRow = "border-b bg-surface-2/60 text-left text-[11.5px] text-subtle";
export const tbodyRow = "group border-b last:border-0 hover:bg-surface-2/60";

/** The row a filter leaves when nothing matches. */
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

/** An object's page: loading, failed, not found, or the page. */
export function DetailFrame({
  crumbs,
  state,
  found,
  noun,
  name,
  children,
}: {
  crumbs: ReactNode[];
  state: { loading: boolean; error: unknown; refetch: () => void };
  found: boolean;
  noun: string;
  name: string;
  children: ReactNode;
}) {
  return (
    <>
      <Topbar crumbs={crumbs} />
      {state.loading ? (
        <div aria-busy="true" aria-label="Loading">
          <div className="space-y-3 border-b px-4 pb-5 pt-6 md:px-8">
            <Skeleton className="h-7 w-48" />
            <Skeleton className="h-3 w-72" />
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
      ) : state.error ? (
        <div className="px-4 py-6 md:px-8">
          <Card>
            <ErrorNotice error={state.error} title={`The ${noun} did not load`} onRetry={state.refetch} />
          </Card>
        </div>
      ) : !found ? (
        <EmptyState title={`No such ${noun}`}>
          This project has no {noun} named <span className="font-mono">{name}</span>.
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

export function Labels({ labels }: { labels: Record<string, string> }) {
  const entries = Object.entries(labels);
  return (
    <span className="flex flex-wrap gap-1.5">
      {entries.length === 0 && <span className="text-subtle">None</span>}
      {entries.map(([k, v]) => (
        <span key={k} className="rounded bg-surface-2 px-1.5 py-0.5 font-mono text-[11.5px]">
          {k}=<span className="text-muted">{v}</span>
        </span>
      ))}
    </span>
  );
}

const statusWord: Record<number, string> = {
  [ConditionStatus.TRUE]: "True",
  [ConditionStatus.FALSE]: "False",
  [ConditionStatus.UNKNOWN]: "Unknown",
};

/** The raw conditions, folded away. */
export function ConditionsCard({ conditions }: { conditions: Condition[] }) {
  return (
    <Card className="overflow-hidden">
      <details className="group">
        <summary className="flex h-11 cursor-pointer list-none items-center gap-2 px-4 text-[13px] font-medium [&::-webkit-details-marker]:hidden">
          <ChevronRight className="size-3.5 text-subtle transition-transform group-open:rotate-90 motion-reduce:transition-none" />
          Conditions
          <span className="font-mono text-[11.5px] font-normal text-subtle">{conditions.length}</span>
        </summary>
        <ul className="divide-y border-t">
          {conditions.length === 0 && <li className="px-4 py-2.5 text-[13px] text-muted">None reported yet.</li>}
          {conditions.map((c) => (
            <li key={c.type} className="flex items-center gap-3 px-4 py-2.5 text-[13px]">
              <Led light={c.status === ConditionStatus.TRUE ? "ok" : c.status === ConditionStatus.UNKNOWN ? "pending" : "off"} className="size-1.5" />
              <span className="w-20 shrink-0 font-medium">{c.type}</span>
              <span className="w-16 shrink-0 font-mono text-[12px] text-muted">{statusWord[c.status] ?? "?"}</span>
              <span className="shrink-0 font-mono text-[12px] text-muted">{c.reason}</span>
              <span className="hidden min-w-0 flex-1 truncate text-muted md:block">{c.message}</span>
              <span className="flex-1 md:hidden" />
              <span className="shrink-0 text-[12px] text-subtle">{ago(date(c.lastTransitionTime))} ago</span>
            </li>
          ))}
        </ul>
      </details>
    </Card>
  );
}

/** The same object through the CLI, or as YAML for `hangar apply -f`. */
export function Equivalent({ cli, yaml }: { cli: string[]; yaml: string }) {
  const [tab, setTab] = useState<"cli" | "yaml">("cli");
  const text = tab === "cli" ? cli.join("\n") : yaml;
  return (
    <Card className="overflow-hidden">
      <div className="flex h-11 items-center justify-between border-b px-4">
        <div role="tablist" className="flex gap-1 text-[12px]">
          {(["cli", "yaml"] as const).map((t) => (
            <button
              key={t}
              role="tab"
              aria-selected={tab === t}
              onClick={() => setTab(t)}
              className={cn("rounded px-2 py-1 font-medium", tab === t ? "bg-surface-2 text-foreground" : "text-muted hover:text-foreground")}
            >
              {t === "cli" ? "CLI" : "YAML"}
            </button>
          ))}
        </div>
        <CopyButton text={text} />
      </div>
      <CodeBlock cli={tab === "cli" ? cli : undefined} yaml={tab === "yaml" ? yaml : undefined} />
    </Card>
  );
}

/** Commands with a prompt, or YAML, in the monospace face. */
export function CodeBlock({ cli, yaml, className }: { cli?: string[]; yaml?: string; className?: string }) {
  return (
    <pre className={cn("max-h-96 overflow-auto whitespace-pre-wrap px-4 py-3 font-mono text-[12px] leading-5", className)}>
      {cli
        ? cli.map((c) => (
            <div key={c} className="-indent-3.5 py-0.5 pl-3.5">
              <span className="select-none text-accent">$ </span>
              {c}
            </div>
          ))
        : yaml}
    </pre>
  );
}

/**
 * A form's equivalent, under its fields, copyable: the manifest it applies
 * and the command that applies it, or the command alone.
 */
export function FormEquivalent({ cli, yaml }: { cli: string[]; yaml?: string }) {
  return (
    <div className="mt-4 overflow-hidden rounded-md border bg-surface-2/60">
      <div className="flex h-8 items-center justify-between border-b px-3 text-[11.5px] text-subtle">
        {yaml ? "Equivalent manifest" : "CLI"}
        <CopyButton text={yaml ?? cli.join("\n")} />
      </div>
      {yaml && <pre className="max-h-48 overflow-auto px-3 py-2 font-mono text-[11.5px] leading-5 text-muted">{yaml}</pre>}
      <CodeBlock cli={cli} className={cn("px-3 py-2 text-[11.5px]", yaml && "border-t")} />
    </div>
  );
}

/** A labelled form field. */
export function FormField({ label, hint, children }: { label: string; hint?: ReactNode; children: ReactNode }) {
  return (
    <label className="mt-3 block">
      <span className="text-[12.5px] font-medium text-foreground">{label}</span>
      <div className="mt-1.5">{children}</div>
      {hint && <span className="mt-1 block text-[11.5px] text-subtle">{hint}</span>}
    </label>
  );
}

/** The CLI's name for each kind. */
export const cliKinds: Record<NetworkKind, string> = { network: "network", securityGroup: "sg", address: "addr", publicEndpoint: "ep" };

/**
 * Delete, confirmed by typing the name, as DeleteX; its error is shown here.
 * When the console already knows the API will refuse, the fact stands above
 * the button and the button is off.
 */
export function DeleteDialog({
  kind,
  name,
  consequence,
  open,
  onClose,
  after,
  blocked,
}: {
  kind: NetworkKind;
  name: string;
  consequence: ReactNode;
  open: boolean;
  onClose: () => void;
  /** What follows a delete the API took, such as going to the list. */
  after?: () => void;
  blocked?: ReactNode;
}) {
  const project = useProjectName();
  const del = useDelete(kind, project, name);
  const [typed, setTyped] = useState("");
  const close = () => {
    setTyped("");
    del.reset();
    onClose();
  };
  return (
    <Dialog open={open} onClose={close} title={`Delete ${nouns[kind]} ${name}`}>
      <p>{consequence}</p>
      <label className="mt-4 block">
        <span className="text-[12.5px] text-foreground">
          Name, to confirm: <span className="font-mono">{name}</span>
        </span>
        <input
          value={typed}
          onChange={(e) => setTyped(e.target.value)}
          autoFocus
          spellCheck={false}
          aria-label="Name, to confirm"
          className="mt-1.5 h-9 w-full rounded-md border border-border-strong bg-background px-2.5 font-mono text-[13px] text-foreground outline-none focus:border-fail"
        />
      </label>
      <FormEquivalent cli={[`hangar delete ${cliKinds[kind]} ${name} -p ${project}`]} />
      {blocked && <Notice tone="warning" title={blocked} className="mt-3" />}
      {del.error && (
        <Notice tone="error" title="Deleting failed" className="mt-3">
          {errorMessage(del.error)}
        </Notice>
      )}
      <div className="mt-5 flex justify-end gap-2">
        <Button onClick={close}>Cancel</Button>
        <Button
          variant="danger"
          disabled={typed !== name || del.isPending || !!blocked}
          onClick={() =>
            del.mutate(undefined, {
              onSuccess: () => {
                close();
                after?.();
              },
            })
          }
        >
          <Trash2 /> {del.isPending ? "Deleting…" : "Delete"}
        </Button>
      </div>
    </Dialog>
  );
}

/**
 * A change the API refused because the object changed after the dialog
 * opened on it: the fact, and a button that starts the dialog again from the
 * object as it is now.
 */
export function StaleNotice({ title, reloading, error, onReload }: { title: string; reloading: boolean; error: unknown; onReload: () => void }) {
  return (
    <Notice tone="warning" title={title} className="mt-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <span>Nothing was applied.</span>
        <Button size="sm" disabled={reloading} onClick={onReload}>
          {reloading ? "Reloading…" : "Reload"}
        </Button>
      </div>
      {error != null && <div className="mt-1 font-mono text-[11.5px] text-fail">{errorMessage(error)}</div>}
    </Notice>
  );
}

/**
 * A form dialog's end: why it cannot be sent yet, the API's error, and the
 * buttons. The submit button is named for what it does.
 */
export function FormFooter({
  problems,
  blocked,
  error,
  failed,
  pending,
  pendingLabel,
  label,
  onSubmit,
  onCancel,
}: {
  problems: string[];
  /** Off for a reason stated elsewhere in the dialog. */
  blocked?: boolean;
  error: unknown;
  failed: string;
  pending: boolean;
  pendingLabel: string;
  label: ReactNode;
  onSubmit: () => void;
  onCancel: () => void;
}) {
  return (
    <>
      {error != null && (
        <Notice tone="error" title={failed} className="mt-3">
          {errorMessage(error)}
        </Notice>
      )}
      {problems.length > 0 && <div className="mt-3 text-[12px] text-subtle">{problems.join(" ")}</div>}
      <div className="mt-5 flex justify-end gap-2">
        <Button onClick={onCancel}>Cancel</Button>
        <Button variant="primary" disabled={problems.length > 0 || !!blocked || pending} onClick={onSubmit}>
          {pending ? pendingLabel : label}
        </Button>
      </div>
    </>
  );
}
