// What a member does to a machine: start, stop and delete. Each is the API's
// own call, so the API's rules (roles, quota, admission) decide, and its
// error is what the console shows.

import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { create } from "@bufbuild/protobuf";
import {
  ContainerRunState,
  ContainerService,
  ContainerSpecSchema,
} from "@hangar/api/hangar/compute/v1alpha1/container_pb";
import {
  VirtualMachineRunState,
  VirtualMachineService,
  VirtualMachineSpecSchema,
} from "@hangar/api/hangar/compute/v1alpha1/virtual_machine_pb";
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

export function useDeleteMachine(kind: MachineKind, project: string, name: string) {
  const transport = useTransport();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      if (kind === "Container") await createClient(ContainerService, transport).deleteContainer({ project, name });
      else await createClient(VirtualMachineService, transport).deleteVirtualMachine({ project, name });
    },
    onSettled: () => queryClient.invalidateQueries(),
  });
}
