// Edit labels, one dialog for every kind: the object's labels as rows of key
// and value, checked as the API checks them, applied back over the object
// as read with its labels alone changed, at the resource version read.

import { Lock, Plus, Tag, X } from "lucide-react";
import { useState } from "react";
import { Dialog } from "@/components/dialog";
import { FormEquivalent, Input } from "@/components/form";
import { Notice } from "@/components/notice";
import { StaleNotice } from "@/components/resource";
import { Button } from "@/components/ui";
import { errorMessage } from "@/lib/api";
import { cn } from "@/lib/cn";
import { labelChanges, labelsOf, rowProblems, rowsOf, systemDomain, type LabelRow } from "@/lib/labels";
import { isStale } from "@/lib/network-actions";
import { labelKinds, labelledYaml, useApplyObjectAt, useReadObject, withLabels, type LabelKind, type LabelObject } from "@/lib/object-kinds";

/** The words of what applying does, by key: "Adds team · Removes app". */
function changeWords(c: ReturnType<typeof labelChanges>): string {
  const part = (verb: string, keys: string[]) => (keys.length ? [`${verb} ${keys.join(", ")}`] : []);
  return [...part("Adds", c.added), ...part("Changes", c.changed), ...part("Removes", c.removed)].join(" · ");
}

/**
 * EditLabelsDialog edits an object's labels. It is mounted when it opens, so
 * it starts from the object as it is then, and keeps that object while the
 * lists refresh: what is applied depends on what was seen. A newer object
 * is never written over: the dialog says it changed, and Reload starts it
 * again from the object as it is now.
 */
export function EditLabelsDialog<K extends LabelKind>({ kind, object, onClose }: { kind: K; object: LabelObject<K>; onClose: () => void }) {
  const info = labelKinds[kind];
  const [base, setBase] = useState(object);
  const stored = base.metadata?.labels ?? {};
  const [rows, setRows] = useState<LabelRow[]>(() => rowsOf(stored));
  const [nextId, setNextId] = useState(rows.length);
  const name = base.metadata?.name ?? "";
  const project = base.metadata?.project ?? "";
  const apply = useApplyObjectAt(kind);
  const reload = useReadObject(kind, project, name);
  const stale = isStale(apply.error);
  const problems = rowProblems(rows, stored);
  const labels = labelsOf(rows);
  const changes = labelChanges(stored, labels);
  const obj = withLabels(kind, base, labels);
  const set = (id: number, field: "key" | "value", v: string) => setRows(rows.map((r) => (r.id === id ? { ...r, [field]: v } : r)));
  const add = () => {
    setRows([...rows, { id: nextId, key: "", value: "" }]);
    setNextId(nextId + 1);
  };
  const remove = (id: number) => setRows(rows.filter((r) => r.id !== id));
  // Reload reads the object as it is now and starts the rows again from it; nothing is applied until Apply is pressed again.
  const startAgain = () =>
    reload.mutate(undefined, {
      onSuccess: (fresh) => {
        setBase(fresh);
        const fresher = rowsOf(fresh.metadata?.labels ?? {});
        setRows(fresher);
        setNextId(fresher.length);
        apply.reset();
      },
    });
  const blocked = problems.size > 0 || changes.none || stale || apply.isPending;
  return (
    <Dialog open onClose={onClose} title={`Edit labels of ${name}`} wide>
      <p>
        The {info.noun}'s own key=value pairs, which <span className="font-mono text-[12px]">hangar get -l</span> selects by. Changing them restarts nothing.
      </p>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (!blocked) apply.mutate(obj, { onSuccess: onClose });
        }}
      >
        <div className="mt-4 grid grid-cols-[minmax(0,1fr)_minmax(0,1fr)_2rem] gap-x-2 gap-y-1.5 text-foreground">
          <span className="text-[12px] text-subtle">Key</span>
          <span className="text-[12px] text-subtle">Value</span>
          <span />
          {rows.length === 0 && <span className="col-span-3 py-1.5 text-[13px] text-muted">No labels.</span>}
          {rows.map((r) => {
            // Fields are numbered among the rows that can be edited.
            const i = rows.filter((x) => !x.system).indexOf(r);
            const p = problems.get(r.id);
            if (r.system) {
              return (
                <div key={r.id} className="col-span-3 grid grid-cols-subgrid items-center">
                  <span className="flex h-9 min-w-0 items-center gap-1.5 rounded-md border border-dashed px-2.5 font-mono text-[12.5px] text-muted">
                    <Lock className="size-3 shrink-0" />
                    <span className="truncate">{r.key}</span>
                  </span>
                  <span className="flex h-9 min-w-0 items-center rounded-md border border-dashed px-2.5 font-mono text-[12.5px] text-muted">
                    <span className="truncate">{r.value}</span>
                  </span>
                  <span />
                </div>
              );
            }
            return (
              <div key={r.id} className="col-span-3 grid grid-cols-subgrid items-start">
                <span>
                  <Input
                    value={r.key}
                    onChange={(e) => set(r.id, "key", e.target.value)}
                    aria-label={`Key ${i + 1}`}
                    aria-invalid={!!p?.key}
                    spellCheck={false}
                    placeholder="app"
                    className={cn(p?.key && "border-fail focus:border-fail")}
                  />
                  {p?.key && <span className="mt-1 block text-[11.5px] text-fail">{p.key}</span>}
                </span>
                <span>
                  <Input
                    value={r.value}
                    onChange={(e) => set(r.id, "value", e.target.value)}
                    aria-label={`Value ${i + 1}`}
                    aria-invalid={!!p?.value}
                    spellCheck={false}
                    placeholder="web"
                    className={cn(p?.value && "border-fail focus:border-fail")}
                  />
                  {p?.value && <span className="mt-1 block text-[11.5px] text-fail">{p.value}</span>}
                </span>
                <Button type="button" variant="ghost" size="icon" aria-label={`Remove ${r.key || `label ${i + 1}`}`} onClick={() => remove(r.id)}>
                  <X />
                </Button>
              </div>
            );
          })}
        </div>
        <div className="mt-2.5 flex flex-wrap items-center justify-between gap-2">
          <Button type="button" size="sm" onClick={add}>
            <Plus /> Add label
          </Button>
          {rows.some((r) => r.system) && (
            <span className="inline-flex items-center gap-1.5 text-[12px] text-subtle">
              <Lock className="size-3" /> Under {systemDomain}/: the system's, kept as they are.
            </span>
          )}
        </div>
        <FormEquivalent yaml={labelledYaml(kind, obj)} cli={[`hangar apply ${info.global ? "" : `-p ${project} `}-f ${name}.yaml`]} />
        {stale ? (
          <StaleNotice title={`${name} changed since this dialog opened.`} reloading={reload.isPending} error={reload.error} onReload={startAgain} />
        ) : (
          apply.error != null && (
            <Notice tone="error" title="Applying the labels failed" className="mt-3">
              {errorMessage(apply.error)}
            </Notice>
          )
        )}
        <div className="mt-3 text-[12px] text-subtle">{changes.none ? "No change." : changeWords(changes)}</div>
        <div className="mt-4 flex justify-end gap-2">
          <Button type="button" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabled={blocked}>
            <Tag /> {apply.isPending ? "Applying…" : "Apply labels"}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
