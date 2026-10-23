// Edit, start, stop, create image (a VM's), edit labels and delete, on a
// machine's page and in a list's row menu. Resizing is the edit page's.

import { useNavigate } from "@tanstack/react-router";
import { VirtualMachineDeletePolicy } from "@hangar/api/hangar/compute/v1alpha1/virtual_machine_pb";
import { ImagePlus, Pencil, Play, Square, Trash2, X } from "lucide-react";
import { useState } from "react";
import { ConfirmDeleteDialog } from "@/components/delete-dialog";
import { EditLabelsDialog } from "@/components/labels-dialog";
import { AnchoredLayer } from "@/components/menu";
import { Notice } from "@/components/notice";
import { editLabelsAction, HeadActions, ObjectRowMenu, type ActionSet, type Can, type ObjectAction } from "@/components/object-actions";
import { useDeleteMachine, useSetRunState, type RunState } from "@/lib/actions";
import { errorMessage } from "@/lib/api";
import { isLive, type MachineView } from "@/lib/machines";
import { capturePerm, machineDeletePerms, machinePermKey, machinePerms } from "@/lib/permissions";

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
      ? "Its root disk is kept."
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

const editPath = (m: MachineView) => (m.kind === "Container" ? "/projects/$project/containers/$name/edit" : "/projects/$project/vms/$name/edit");

/**
 * A machine's actions, on its page and in its row's menu: Edit (not
 * offered while it is being deleted), Start or Stop, Create image (a VM's,
 * the New image page on its root disk, off until the VM is stopped, as the
 * API captures only then), Edit labels and Delete, by `can`, the answer of
 * useMachineCan its list or page asked: the kind's apply for Edit, Start,
 * Stop and labels, storage.customimages.apply for Create image, and the
 * kind's delete for Delete. `after` follows a delete the API took. The run
 * state's call is given too, for where its error is shown.
 */
export function useMachineActions(m: MachineView, can: Can, after?: () => void): ActionSet & { run: ReturnType<typeof useSetRunState> } {
  const run = useSetRunState(m.kind, m.project, m.name);
  const navigate = useNavigate();
  const [open, setOpen] = useState<"delete" | "labels" | null>(null);
  const mayApply = can(machinePerms[machinePermKey(m.kind)]);
  const mayDelete = can(machineDeletePerms[machinePermKey(m.kind)]);
  const object = m.container ?? m.vm;
  const actions: ObjectAction[] = [];
  if (mayApply && !m.deleting) {
    const link = { to: editPath(m), params: { project: m.project, name: m.name } };
    actions.push({ label: "Edit", icon: Pencil, link, onSelect: () => void navigate(link) });
  }
  if (mayApply) {
    actions.push(
      m.stopped
        ? { label: "Start", icon: Play, primary: true, pending: run.isPending ? "Starting…" : undefined, onSelect: () => run.mutate("running") }
        : { label: "Stop", icon: Square, pending: run.isPending ? "Stopping…" : undefined, onSelect: () => run.mutate("stopped") },
    );
  }
  const rootDisk = m.vm?.status?.rootDisk;
  if (m.kind === "VirtualMachine" && rootDisk && can(capturePerm)) {
    const link = { to: "/projects/$project/new/custom-image", params: { project: m.project }, search: { disk: rootDisk } };
    actions.push({ label: "Create image", icon: ImagePlus, disabled: m.deleting || isLive(m), link, onSelect: () => void navigate(link) });
  }
  if (mayDelete) actions.push({ label: "Delete", icon: Trash2, danger: true, onSelect: () => setOpen("delete") });
  const close = () => setOpen(null);
  return {
    run,
    actions,
    editLabels: editLabelsAction(mayApply && !!object, m.deleting, () => setOpen("labels")),
    deleting: m.deleting,
    dialogs: (
      <>
        <DeleteDialog m={m} open={open === "delete"} onClose={close} after={after} />
        {open === "labels" && m.container && <EditLabelsDialog kind="container" object={m.container} onClose={close} />}
        {open === "labels" && m.vm && <EditLabelsDialog kind="vm" object={m.vm} onClose={close} />}
      </>
    ),
  };
}

/** The machine's actions as its page's head draws them, with the run state's error under them. */
export function MachineActions({ set }: { set: ReturnType<typeof useMachineActions> }) {
  const run = set.run;
  return (
    <div className="flex flex-col items-stretch gap-2 md:items-end">
      <div className="flex flex-wrap items-center gap-2">
        <HeadActions set={set} />
      </div>
      {run.error && (
        <Notice tone="error" title={runErrorTitle(run.variables)} className="max-w-md">
          {errorMessage(run.error)}
        </Notice>
      )}
      {set.dialogs}
    </div>
  );
}

/** The same actions in a row's menu, with the run state's error beside it. */
export function MachineRowMenu({ m, can }: { m: MachineView; can: Can }) {
  const set = useMachineActions(m, can);
  const run = set.run;
  return (
    <ObjectRowMenu name={m.name} set={set}>
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
        </>
      )}
    </ObjectRowMenu>
  );
}
