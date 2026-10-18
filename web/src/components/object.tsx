// Cards and pieces an object's page shows whatever its kind: its labels, its
// raw conditions, and the same object through the CLI or as YAML.

import { ConditionStatus, type Condition } from "@hangar/api/hangar/meta/v1/meta_pb";
import { ChevronRight } from "lucide-react";
import { useState, type ReactNode } from "react";
import { Card, CopyButton, Led } from "@/components/ui";
import { cn } from "@/lib/cn";
import { ago, date } from "@/lib/machines";

/** An object's labels as key=value chips, or None. */
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

/** One condition as a row of a conditions card; `of` names the object it is of, when a card holds more than one. */
export function ConditionRow({ c, of }: { c: Condition; of?: string }) {
  return (
    <li className="flex items-center gap-3 px-4 py-2.5 text-[13px]">
      <Led tone={c.status === ConditionStatus.TRUE ? "ok" : c.status === ConditionStatus.UNKNOWN ? "pending" : "off"} className="size-1.5" />
      {of !== undefined && <span className="w-20 shrink-0 text-[12px] text-subtle">{of}</span>}
      <span className="w-20 shrink-0 font-medium">{c.type}</span>
      <span className="w-16 shrink-0 font-mono text-[12px] text-muted">{statusWord[c.status] ?? "?"}</span>
      <span className="shrink-0 font-mono text-[12px] text-muted">{c.reason}</span>
      <span className="hidden min-w-0 flex-1 truncate text-muted md:block">{c.message}</span>
      <span className="flex-1 md:hidden" />
      <span className="shrink-0 text-[12px] text-subtle">{ago(date(c.lastTransitionTime))} ago</span>
    </li>
  );
}

/** A card of raw conditions, folded away under its count. */
export function ConditionsFold({ count, children }: { count: number; children: ReactNode }) {
  return (
    <Card className="overflow-hidden">
      <details className="group">
        <summary className="flex h-11 cursor-pointer list-none items-center gap-2 px-4 text-[13px] font-medium [&::-webkit-details-marker]:hidden">
          <ChevronRight className="size-3.5 text-subtle transition-transform group-open:rotate-90 motion-reduce:transition-none" />
          Conditions
          <span className="font-mono text-[11.5px] font-normal text-subtle">{count}</span>
        </summary>
        <ul className="divide-y border-t">{children}</ul>
      </details>
    </Card>
  );
}

/** An object's raw conditions, folded away. */
export function ConditionsCard({ conditions }: { conditions: Condition[] }) {
  return (
    <ConditionsFold count={conditions.length}>
      {conditions.length === 0 && <li className="px-4 py-2.5 text-[13px] text-muted">None reported yet.</li>}
      {conditions.map((c) => (
        <ConditionRow key={c.type} c={c} />
      ))}
    </ConditionsFold>
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

/** The same object through the CLI, and as YAML for `hangar apply -f` when given; each copyable. */
export function EquivalentCard({ cli, yaml }: { cli: string[]; yaml?: string }) {
  const [tab, setTab] = useState<"cli" | "yaml">("cli");
  const showYaml = tab === "yaml" && yaml !== undefined;
  return (
    <Card className="overflow-hidden">
      <div className="flex h-11 items-center justify-between border-b px-4">
        <div role="tablist" className="flex gap-1 text-[12px]">
          {(yaml !== undefined ? (["cli", "yaml"] as const) : (["cli"] as const)).map((t) => (
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
        <CopyButton text={showYaml ? yaml : cli.join("\n")} />
      </div>
      {showYaml ? <CodeBlock yaml={yaml} /> : <CodeBlock cli={cli} />}
    </Card>
  );
}
