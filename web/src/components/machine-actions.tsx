// Start, stop and delete, on a machine's page and in a list's row menu.

import { useNavigate } from "@tanstack/react-router";
import { VirtualMachineDeletePolicy } from "@hangar/api/hangar/compute/v1alpha1/virtual_machine_pb";
import { Play, Square, Trash2, X } from "lucide-react";
import { useState } from "react";
import { ConfirmDeleteDialog } from "@/components/delete-dialog";
import { AnchoredLayer, RowMenu } from "@/components/menu";
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
  const noun = m.kind === "Container" ? "container" : "virtual machine";
  return (
    <ConfirmDeleteDialog
      open={open}
      onClose={onClose}
      title={`Delete ${noun} ${m.name}`}
      name={m.name}
      consequence={deleteConsequence(m)}
      del={del}
      after={after}
    />
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
  const [deleting, setDeleting] = useState(false);
  return (
    <RowMenu
      label={`Actions for ${m.name}`}
      actions={[
        m.stopped
          ? { label: "Start", icon: Play, disabled: m.deleting, onSelect: () => run.mutate("running") }
          : { label: "Stop", icon: Square, disabled: m.deleting, onSelect: () => run.mutate("stopped") },
        { label: "Delete", icon: Trash2, danger: true, disabled: m.deleting, onSelect: () => setDeleting(true) },
      ]}
    >
      {(anchor) => (
        <>
          {run.error && (
            <AnchoredLayer anchor={anchor} className="w-72">
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
            </AnchoredLayer>
          )}
          <DeleteDialog m={m} open={deleting} onClose={() => setDeleting(false)} />
        </>
      )}
    </RowMenu>
  );
}
