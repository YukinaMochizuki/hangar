// Start, stop, edit, resize and delete, on a machine's page and in a list's
// row menu.

import { Link, useNavigate } from "@tanstack/react-router";
import { VirtualMachineDeletePolicy } from "@hangar/api/hangar/compute/v1alpha1/virtual_machine_pb";
import { Maximize2, Pencil, Play, Square, Trash2, X } from "lucide-react";
import { useState } from "react";
import { ConfirmDeleteDialog } from "@/components/delete-dialog";
import { Dialog } from "@/components/dialog";
import { FormEquivalent } from "@/components/form";
import { Choice } from "@/components/form-page";
import { AnchoredLayer, RowMenu } from "@/components/menu";
import { Notice } from "@/components/notice";
import { Skeleton, ErrorNotice } from "@/components/states";
import { Button, buttonVariants } from "@/components/ui";
import { StaleNotice } from "@/components/resource";
import { ContainerSchema } from "@hangar/api/hangar/compute/v1alpha1/container_pb";
import { useApplyMachineAt, useDeleteMachine, useReadMachine, useSetRunState, type RunState } from "@/lib/actions";
import { errorMessage } from "@/lib/api";
import { cn } from "@/lib/cn";
import { containerDefaults, editedObject, vmDefaults, type MachineObject } from "@/lib/machine-edit";
import { isStale } from "@/lib/network-actions";
import { sizeOf, useMachineSizes, type Size } from "@/lib/machine-sizes";
import type { MachineView } from "@/lib/machines";
import { manifestOf, toYaml } from "@/lib/manifest";
import { useOverQuota } from "@/lib/over-quota";
import { useMayApplyMachine } from "@/lib/permissions";
import { formatNumber, quantityCPU, quantityGiB } from "@/lib/quantity";

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

const editPath = (m: MachineView) => (m.kind === "Container" ? "/projects/$project/containers/$name/edit" : "/projects/$project/vms/$name/edit");

/**
 * The size a machine's spec asks for: the listed size its cpu and memory
 * match, or Custom, never a listed size it does not match.
 */
function specSize(o: MachineObject, sizes: Size[]): Size {
  const container = o.$typeName === ContainerSchema.typeName;
  const res = o.spec?.resources;
  const d = container ? containerDefaults : vmDefaults;
  const cpu = quantityCPU(res?.cpu || d.cpu);
  const memoryGi = quantityGiB(res?.memory || d.memory);
  const match = sizeOf({ cpu, memoryGi }, sizes);
  return match ?? { name: "", label: "Custom", order: -1, cpu, memoryGi, diskGi: 0, resources: { cpu: res?.cpu ?? "", memory: res?.memory ?? "", disk: "" } };
}

/**
 * Resize: the platform's sizes, the machine's own marked, and the size
 * chosen applied with only the resources' cpu and memory changed, at the
 * version of the object the dialog opened on. A newer object is never
 * written over: the dialog says it changed and can start again from it.
 * Mounted when it opens, so it opens on the object as it is then.
 */
export function ResizeDialog({ m, onClose }: { m: MachineView; onClose: () => void }) {
  const machineSizes = useMachineSizes();
  const overQuota = useOverQuota(m.project);
  const [base, setBase] = useState<MachineObject>(() => (m.container ?? m.vm)!);
  const [chosen, setChosen] = useState<string | undefined>(undefined);
  const apply = useApplyMachineAt();
  const read = useReadMachine(m.kind, m.project, m.name);
  const stale = isStale(apply.error);
  const current = specSize(base, machineSizes.sizes);
  const to = machineSizes.sizes.find((z) => z.name === chosen && z.name !== current.name);
  const noun = m.kind === "Container" ? "container" : "VM";
  const over = (z: Size) => overQuota({ machines: 0, cpu: z.cpu - current.cpu, memoryGi: z.memoryGi - current.memoryGi, diskGi: 0 });
  const toOver = to ? over(to) : [];
  const obj = to ? editedObject(base, { cpu: to.resources.cpu, memory: to.resources.memory }) : base;
  const cards = [...(current.name ? [] : [current]), ...machineSizes.sizes];
  const startAgain = () =>
    read.mutate(undefined, {
      onSuccess: (fresh) => {
        setBase(fresh);
        apply.reset();
      },
    });
  return (
    <Dialog open onClose={onClose} title={`Resize ${m.name}`}>
      <p>
        {m.stopped
          ? `${m.name} is stopped; the new size counts once it starts.`
          : `Resizing restarts the ${noun}.`}{" "}
        Its disk stays as it is.
      </p>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (to && toOver.length === 0 && !stale) apply.mutate(obj, { onSuccess: onClose });
        }}
      >
        <div className="mt-4 text-foreground">
          {machineSizes.loading ? (
            <div className="grid grid-cols-2 gap-2">
              {[0, 1, 2, 3].map((i) => (
                <Skeleton key={i} className="h-[66px] rounded-lg" />
              ))}
            </div>
          ) : machineSizes.error ? (
            <ErrorNotice error={machineSizes.error} title="The machine sizes did not load" onRetry={machineSizes.refetch} className="p-0" />
          ) : (
            <div role="radiogroup" aria-label="Size" className="grid grid-cols-2 gap-2">
              {cards.map((z) => {
                const own = z === current;
                const selected = to ? to.name === z.name : own;
                const tooBig = over(z).length > 0;
                return (
                  <Choice
                    key={own ? "(current)" : z.name}
                    label={`Size ${z.label}`}
                    selected={selected}
                    onClick={() => setChosen(own ? undefined : z.name)}
                    className={cn("px-3 py-2.5", tooBig && (selected ? "border-fail ring-fail [&>span:first-child]:bg-fail" : "opacity-60"))}
                  >
                    <div className="flex items-baseline gap-2 pr-5">
                      <span className="font-mono text-[15px] font-semibold tracking-tight">{z.label}</span>
                      {own && <span className="text-[11px] font-medium text-muted">Current</span>}
                      {tooBig && <span className="text-[11px] font-medium text-fail">Over quota</span>}
                    </div>
                    <div className="mt-1 font-mono text-[12px] tabular text-muted">
                      {formatNumber(z.cpu)} vCPU · {formatNumber(z.memoryGi)} GiB
                    </div>
                  </Choice>
                );
              })}
            </div>
          )}
        </div>
        {obj && (
          <FormEquivalent
            yaml={toYaml(manifestOf(obj, m.kind))}
            cli={[`hangar apply -p ${m.project} -f ${m.name}.yaml`]}
          />
        )}
        {toOver.length > 0 && (
          <Notice tone="error" title={`Over the project's ${toOver.map((o) => o.label).join(" and ")} limit`} className="mt-3">
            {toOver.map((o) => (
              <div key={o.key}>
                {to!.label} adds {formatNumber(o.need)}
                {o.unit}; {formatNumber(o.left)}
                {o.unit} of {formatNumber(o.limit)}
                {o.unit} is left.
              </div>
            ))}
          </Notice>
        )}
        {stale ? (
          <StaleNotice title={`${m.name} changed since this dialog opened.`} reloading={read.isPending} error={read.error} onReload={startAgain} />
        ) : (
          apply.error != null && (
            <Notice tone="error" title="Resizing failed" className="mt-3">
              {errorMessage(apply.error)}
            </Notice>
          )
        )}
        <div className="mt-5 flex justify-end gap-2">
          <Button type="button" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabled={!to || toOver.length > 0 || stale || apply.isPending}>
            <Maximize2 /> {apply.isPending ? "Resizing…" : to ? `Resize to ${to.label}` : "Resize"}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

/** The run-state button, edit, resize and delete, as on a machine's page. */
export function MachineActions({ m }: { m: MachineView }) {
  const run = useSetRunState(m.kind, m.project, m.name);
  const [deleting, setDeleting] = useState(false);
  const [resizing, setResizing] = useState(false);
  const navigate = useNavigate();
  const { may: mayApply } = useMayApplyMachine(m.project, m.kind);
  const list = m.kind === "Container" ? "/projects/$project/containers" : "/projects/$project/vms";
  return (
    <div className="flex flex-col items-stretch gap-2 md:items-end">
      <div className="flex flex-wrap items-center gap-2">
        {mayApply && !m.deleting && (
          <>
            <Link to={editPath(m)} params={{ project: m.project, name: m.name }} className={buttonVariants()}>
              <Pencil /> Edit
            </Link>
            <Button onClick={() => setResizing(true)}>
              <Maximize2 /> Resize
            </Button>
          </>
        )}
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
      {mayApply && resizing && <ResizeDialog m={m} onClose={() => setResizing(false)} />}
    </div>
  );
}

/** The same actions in a row's menu, Edit among them where the caller may apply the kind. */
export function MachineRowMenu({ m, mayEdit }: { m: MachineView; mayEdit?: boolean }) {
  const run = useSetRunState(m.kind, m.project, m.name);
  const [deleting, setDeleting] = useState(false);
  const navigate = useNavigate();
  return (
    <RowMenu
      label={`Actions for ${m.name}`}
      actions={[
        m.stopped
          ? { label: "Start", icon: Play, disabled: m.deleting, onSelect: () => run.mutate("running") }
          : { label: "Stop", icon: Square, disabled: m.deleting, onSelect: () => run.mutate("stopped") },
        ...(mayEdit && !m.deleting
          ? [{ label: "Edit", icon: Pencil, onSelect: () => void navigate({ to: editPath(m), params: { project: m.project, name: m.name } }) }]
          : []),
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
