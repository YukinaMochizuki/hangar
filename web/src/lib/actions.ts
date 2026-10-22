// What a member does to a machine: start, stop and delete. Each is the API's
// own call, so the API's rules (roles, quota, admission) decide, and its
// error is what the console shows.

import { Code, ConnectError, createClient, type Transport } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { create } from "@bufbuild/protobuf";
import {
  ContainerRunState,
  ContainerSchema,
  ContainerService,
  ContainerSpecSchema,
  type Container,
} from "@hangar/api/hangar/compute/v1alpha1/container_pb";
import {
  VirtualMachineRunState,
  VirtualMachineService,
  VirtualMachineSpecSchema,
  type VirtualMachine,
} from "@hangar/api/hangar/compute/v1alpha1/virtual_machine_pb";
import type { MachineObject } from "@/lib/machine-edit";
import type { MachineKind } from "@/lib/machines";

export type RunState = "running" | "stopped";

// How many times a write is tried when the object keeps changing under it.
const attempts = 4;

/**
 * Reads an object, changes it and writes it back at the version read, and
 * reads again when the write meets a newer version (Aborted), as the CLI's
 * start and stop do. Any other error ends it.
 */
export async function readModifyWrite<T>(read: () => Promise<T>, change: (o: T) => void, write: (o: T) => Promise<unknown>): Promise<void> {
  for (let attempt = 1; ; attempt++) {
    const o = await read();
    change(o);
    try {
      await write(o);
      return;
    } catch (err) {
      if (err instanceof ConnectError && err.code === Code.Aborted && attempt < attempts) continue;
      throw err;
    }
  }
}

/**
 * Sets a machine's run state as `hangar start` and `hangar stop` do: read the
 * object, apply it back with only the run state changed at the resource
 * version read, and read again if it changed in between.
 */
export function useSetRunState(kind: MachineKind, project: string, name: string) {
  const transport = useTransport();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (want: RunState) => {
      if (kind === "Container") {
        const c = createClient(ContainerService, transport);
        await readModifyWrite(
          async () => {
            const { container } = await c.getContainer({ project, name });
            if (!container) throw new Error(`${name} is gone.`);
            return container;
          },
          (o) => {
            o.spec ??= create(ContainerSpecSchema);
            o.spec.runState = want === "running" ? ContainerRunState.RUNNING : ContainerRunState.STOPPED;
          },
          (o) => c.applyContainer({ container: o }),
        );
      } else {
        const c = createClient(VirtualMachineService, transport);
        await readModifyWrite(
          async () => {
            const { virtualMachine } = await c.getVirtualMachine({ project, name });
            if (!virtualMachine) throw new Error(`${name} is gone.`);
            return virtualMachine;
          },
          (o) => {
            o.spec ??= create(VirtualMachineSpecSchema);
            o.spec.runState = want === "running" ? VirtualMachineRunState.RUNNING : VirtualMachineRunState.STOPPED;
          },
          (o) => c.applyVirtualMachine({ virtualMachine: o }),
        );
      }
    },
    onSettled: () => queryClient.invalidateQueries(),
  });
}

/**
 * Applies a container or VM as it is, at the resource version it carries:
 * a change made on what the person saw. A newer object makes the API answer
 * Aborted, and nothing is read again or written over it.
 */
export async function applyMachineAt(transport: Transport, o: MachineObject): Promise<void> {
  // Without a version Apply is create-or-update, which would write over whatever is there.
  if (!o.metadata?.resourceVersion) throw new Error(`${o.metadata?.name ?? "The object"} carries no resource version to apply at.`);
  if (o.$typeName === ContainerSchema.typeName) await createClient(ContainerService, transport).applyContainer({ container: o as Container });
  else await createClient(VirtualMachineService, transport).applyVirtualMachine({ virtualMachine: o as VirtualMachine });
}

/** Reads a container or VM as it is now, for a form to start again from. */
export function useReadMachine(kind: MachineKind, project: string, name: string) {
  const transport = useTransport();
  return useMutation({
    mutationFn: async (): Promise<MachineObject> => {
      if (kind === "Container") {
        const { container } = await createClient(ContainerService, transport).getContainer({ project, name });
        if (!container) throw new Error(`${name} is gone.`);
        return container;
      }
      const { virtualMachine } = await createClient(VirtualMachineService, transport).getVirtualMachine({ project, name });
      if (!virtualMachine) throw new Error(`${name} is gone.`);
      return virtualMachine;
    },
  });
}

export function useDeleteMachine(kind: MachineKind, project: string, name: string) {
  const transport = useTransport();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      if (kind === "Container") await createClient(ContainerService, transport).deleteContainer({ project, name });
      else await createClient(VirtualMachineService, transport).deleteVirtualMachine({ project, name });
    },
    // Not awaited: the page that deleted the object goes on (to the list)
    // before the refetch finds it gone and says so.
    onSettled: () => void queryClient.invalidateQueries(),
  });
}
