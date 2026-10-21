// A container's or VM's edit page: the new-machine form opened on the
// object as it was when the page opened, applied at that resource version
// so that a newer object is never overwritten (docs/decisions/console-design.md).

import { useTransport } from "@connectrpc/connect-query";
import { useState } from "react";
import { DetailFrame } from "@/components/page";
import { useProjectName } from "@/components/shell";
import { EmptyState } from "@/components/states";
import { applyMachineAt, useReadMachine } from "@/lib/actions";
import type { MachineObject } from "@/lib/machine-edit";
import { useMachines, type MachineKind, type MachineView } from "@/lib/machines";
import { useMayApplyMachine } from "@/lib/permissions";
import { wholeGiB } from "@/lib/quantity";
import { MachineForm } from "@/pages/create-machine";

/** The form on the object as first found; the list refreshing does not move it, a reload does. */
function EditForm({ m }: { m: MachineView }) {
  const transport = useTransport();
  const [base, setBase] = useState<MachineObject>(() => (m.container ?? m.vm)!);
  const read = useReadMachine(m.kind, m.project, m.name);
  const placedDisk = m.machine?.spec?.node ? m.machine.spec.resources?.disk : undefined;
  return (
    <MachineForm
      // Opened anew on the object a reload read.
      key={String(base.metadata?.resourceVersion ?? 0n)}
      kind={m.kind === "Container" ? "container" : "vm"}
      edit={{
        base,
        placedDiskGi: placedDisk ? wholeGiB(placedDisk) : undefined,
        write: (o) => applyMachineAt(transport, o),
        reload: { pending: read.isPending, error: read.error, run: () => read.mutate(undefined, { onSuccess: setBase }) },
      }}
    />
  );
}

/** EditMachine is /projects/<project>/containers/<name>/edit and /vms/<name>/edit. */
export function EditMachine({ kind, name }: { kind: MachineKind; name: string }) {
  const project = useProjectName();
  const machines = useMachines(project, kind);
  const { may, refused } = useMayApplyMachine(project, kind);
  const m = machines.items.find((x) => x.name === name);
  const noun = kind === "Container" ? "container" : "virtual machine";
  return (
    <DetailFrame crumbs={[kind === "Container" ? "Containers" : "Virtual machines", name, "Edit"]} state={machines} found={!!m} noun={noun} name={name}>
      {m?.deleting ? (
        <EmptyState title={`${name} is being deleted`}>It goes once its machine and disks are gone, and is not edited meanwhile.</EmptyState>
      ) : m && may ? (
        // Keyed by the object's identity, so one made again under the name starts a new edit.
        <EditForm key={m.uid} m={m} />
      ) : refused ? (
        <EmptyState title="No editing">This account's roles do not include changing {noun}s in this project.</EmptyState>
      ) : null}
    </DetailFrame>
  );
}
