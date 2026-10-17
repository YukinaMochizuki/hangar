// Start, stop and delete, on a machine's page and in a list's row menu.

import { useNavigate } from "@tanstack/react-router";
import { VirtualMachineDeletePolicy } from "@hangar/api/hangar/compute/v1alpha1/virtual_machine_pb";
import { MoreHorizontal, Play, Square, Trash2, X } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { Dialog } from "@/components/dialog";
import { Notice } from "@/components/notice";
import { Button } from "@/components/ui";
import { useDeleteMachine, useSetRunState, type RunState } from "@/lib/actions";
import { errorMessage } from "@/lib/api";
import type { MachineView } from "@/lib/machines";

/** The title of a run-state error: the action asked for, whatever the state now. */
function runErrorTitle(asked: RunState | undefined): string {
  return asked === "running" ? "Starting failed" : "Stopping failed";
}

/** What deleting takes with it, as the API does it. */
function deleteConsequence(m: MachineView): string {
  if (m.kind === "Container") return "Its machine and root disk are deleted with it.";
  const root = m.vm?.spec?.rootDisk?.source;
  if (root?.case === "fromImage") {
    return root.value.deletePolicy === VirtualMachineDeletePolicy.RETAIN
      ? "Its root disk is kept, and another VM can boot from it."
      : "Its machine and the root disk made for it are deleted with it.";
  }
  if (root?.case === "disk") return `Its machine is deleted; the disk ${root.value} is kept.`;
  return "Its machine is deleted with it.";
}

function DeleteDialog({ m, open, onClose, after }: { m: MachineView; open: boolean; onClose: () => void; after?: () => void }) {
  const del = useDeleteMachine(m.kind, m.project, m.name);
  const [typed, setTyped] = useState("");
  const noun = m.kind === "Container" ? "container" : "virtual machine";
  return (
    <Dialog
      open={open}
      onClose={() => {
        del.reset();
        setTyped("");
        onClose();
      }}
      title={`Delete ${noun} ${m.name}`}
    >
      <p>{deleteConsequence(m)}</p>
      <label className="mt-4 block">
        <span className="text-[12.5px] text-foreground">
          Name, to confirm: <span className="font-mono">{m.name}</span>
        </span>
        <input
          value={typed}
          onChange={(e) => setTyped(e.target.value)}
          autoFocus
          spellCheck={false}
          className="mt-1.5 h-9 w-full rounded-md border border-border-strong bg-background px-2.5 font-mono text-[13px] text-foreground outline-none focus:border-fail"
        />
      </label>
      {del.error && (
        <Notice tone="error" title="Deleting failed" className="mt-3">
          {errorMessage(del.error)}
        </Notice>
      )}
      <div className="mt-5 flex justify-end gap-2">
        <Button onClick={onClose}>Cancel</Button>
        <Button
          variant="danger"
          disabled={typed !== m.name || del.isPending}
          onClick={() =>
            del.mutate(undefined, {
              onSuccess: () => {
                setTyped("");
                onClose();
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

/** The run-state button and delete, as on a machine's page. */
export function MachineActions({ m }: { m: MachineView }) {
  const run = useSetRunState(m.kind, m.project, m.name);
  const [deleting, setDeleting] = useState(false);
  const navigate = useNavigate();
  const list = m.kind === "Container" ? "/projects/$project/containers" : "/projects/$project/vms";
  return (
    <div className="flex flex-col items-stretch gap-2 md:items-end">
      <div className="flex flex-wrap items-center gap-2">
        {m.stopped ? (
          <Button variant="primary" disabled={run.isPending || m.deleting} onClick={() => run.mutate("running")}>
            <Play /> {run.isPending ? "Starting…" : "Start"}
          </Button>
        ) : (
          <Button disabled={run.isPending || m.deleting} onClick={() => run.mutate("stopped")}>
            <Square /> {run.isPending ? "Stopping…" : "Stop"}
          </Button>
        )}
        <Button variant="danger" disabled={m.deleting} onClick={() => setDeleting(true)}>
          <Trash2 /> Delete
        </Button>
      </div>
      {run.error && (
        <Notice tone="error" title={runErrorTitle(run.variables)} className="max-w-md">
          {errorMessage(run.error)}
        </Notice>
      )}
      <DeleteDialog
        m={m}
        open={deleting}
        onClose={() => setDeleting(false)}
        after={() => void navigate({ to: list, params: { project: m.project } })}
      />
    </div>
  );
}

/** The same actions in a row's menu. */
export function MachineRowMenu({ m }: { m: MachineView }) {
  const run = useSetRunState(m.kind, m.project, m.name);
  const [open, setOpen] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => ref.current && !ref.current.contains(e.target as Node) && setOpen(false);
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && setOpen(false);
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);
  const item = "flex w-full items-center gap-2 px-3 py-1.5 text-left text-[13px] hover:bg-surface-2 disabled:opacity-50";
  return (
    <div ref={ref} className="relative">
      <Button variant="ghost" size="icon" aria-label={`Actions for ${m.name}`} aria-haspopup="menu" aria-expanded={open} onClick={() => setOpen(!open)}>
        <MoreHorizontal />
      </Button>
      {open && (
        <div role="menu" className="absolute right-0 top-full z-20 mt-1 w-40 rounded-md border bg-surface py-1 shadow-lg">
          {m.stopped ? (
            <button role="menuitem" className={item} disabled={m.deleting} onClick={() => (run.mutate("running"), setOpen(false))}>
              <Play className="size-3.5" /> Start
            </button>
          ) : (
            <button role="menuitem" className={item} disabled={m.deleting} onClick={() => (run.mutate("stopped"), setOpen(false))}>
              <Square className="size-3.5" /> Stop
            </button>
          )}
          <button role="menuitem" className={`${item} text-fail`} disabled={m.deleting} onClick={() => (setDeleting(true), setOpen(false))}>
            <Trash2 className="size-3.5" /> Delete
          </button>
        </div>
      )}
      {run.error && (
        <div className="absolute right-0 top-full z-20 mt-1 w-72">
          <Notice tone="error" title={runErrorTitle(run.variables)} className="pr-8">
            {errorMessage(run.error)}
          </Notice>
          <button
            aria-label="Dismiss"
            onClick={() => run.reset()}
            className="absolute right-1.5 top-1.5 rounded p-1 text-subtle hover:bg-surface-2 hover:text-foreground"
          >
            <X className="size-3.5" />
          </button>
        </div>
      )}
      <DeleteDialog m={m} open={deleting} onClose={() => setDeleting(false)} />
    </div>
  );
}
