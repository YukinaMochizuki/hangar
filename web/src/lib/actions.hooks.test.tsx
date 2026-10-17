// @vitest-environment jsdom

import { Code, ConnectError } from "@connectrpc/connect";
import {
  ContainerRunState,
  ContainerService,
  type ApplyContainerRequest,
} from "@hangar/api/hangar/compute/v1alpha1/container_pb";
import {
  VirtualMachineRunState,
  VirtualMachineService,
  type ApplyVirtualMachineRequest,
} from "@hangar/api/hangar/compute/v1alpha1/virtual_machine_pb";
import { cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { useSetRunState } from "@/lib/actions";
import { fakeApi, withApi } from "@/test/harness";

afterEach(cleanup);

/** An API holding one container and one VM, recording what is applied. */
function api(opts: { conflicts?: number } = {}) {
  const containers: ApplyContainerRequest[] = [];
  const vms: ApplyVirtualMachineRequest[] = [];
  let version = 7n;
  let conflicts = opts.conflicts ?? 0;
  const a = fakeApi((r) => {
    r.service(ContainerService, {
      getContainer: (req) => ({
        container: { metadata: { name: req.name, project: req.project, resourceVersion: version }, spec: { image: "nginx:1" } },
      }),
      applyContainer: (req) => {
        if (conflicts > 0) {
          conflicts--;
          version++;
          throw new ConnectError("resource version moved", Code.Aborted);
        }
        containers.push(req);
        return { container: req.container };
      },
    });
    r.service(VirtualMachineService, {
      getVirtualMachine: (req) => ({
        virtualMachine: { metadata: { name: req.name, project: req.project, resourceVersion: version }, spec: {} },
      }),
      applyVirtualMachine: (req) => {
        vms.push(req);
        return { virtualMachine: req.virtualMachine };
      },
    });
  });
  return { a, containers, vms, version: () => version };
}

describe("useSetRunState", () => {
  it("stops a container: its own object, run state STOPPED, at the version read", async () => {
    const { a, containers } = api();
    const { result } = renderHook(() => useSetRunState("Container", "demo", "web"), { wrapper: withApi(a) });
    result.current.mutate("stopped");
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(a.calls).toEqual(["GetContainer", "ApplyContainer"]);
    expect(containers).toHaveLength(1);
    expect(containers[0].container?.metadata?.name).toBe("web");
    expect(containers[0].container?.metadata?.resourceVersion).toBe(7n);
    expect(containers[0].container?.spec?.runState).toBe(ContainerRunState.STOPPED);
    expect(containers[0].container?.spec?.image).toBe("nginx:1");
  });

  it("starts a container with run state RUNNING", async () => {
    const { a, containers } = api();
    const { result } = renderHook(() => useSetRunState("Container", "demo", "web"), { wrapper: withApi(a) });
    result.current.mutate("running");
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(containers[0].container?.spec?.runState).toBe(ContainerRunState.RUNNING);
  });

  it("starts and stops a VM through its own service", async () => {
    const { a, vms } = api();
    const { result } = renderHook(() => useSetRunState("VirtualMachine", "demo", "box"), { wrapper: withApi(a) });
    result.current.mutate("running");
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    result.current.mutate("stopped");
    await waitFor(() => expect(vms).toHaveLength(2));
    expect(a.calls).not.toContain("ApplyContainer");
    expect(vms.map((v) => v.virtualMachine?.spec?.runState)).toEqual([VirtualMachineRunState.RUNNING, VirtualMachineRunState.STOPPED]);
  });

  it("reads again after a conflict and writes at the newer version", async () => {
    const { a, containers } = api({ conflicts: 1 });
    const { result } = renderHook(() => useSetRunState("Container", "demo", "web"), { wrapper: withApi(a) });
    result.current.mutate("stopped");
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(a.calls).toEqual(["GetContainer", "ApplyContainer", "GetContainer", "ApplyContainer"]);
    expect(containers[0].container?.metadata?.resourceVersion).toBe(8n);
  });
});
