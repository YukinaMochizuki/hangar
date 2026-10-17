import { create } from "@bufbuild/protobuf";
import { ContainerRunState, ContainerSchema } from "@hangar/api/hangar/compute/v1alpha1/container_pb";
import { MachineSchema } from "@hangar/api/hangar/compute/v1alpha1/machine_pb";
import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import { describe, expect, it } from "vitest";
import { containerView } from "@/lib/machines";
import { runsOnState } from "@/lib/runs-on";

const placement = { region: "ap-southeast", site: "SGP02", cpuModel: "AMD EPYC 7313", chassis: "generic-1u-2x-u2" };
const noRoom = "no ready node has 8192Mi of memory free";

/** A container as the console reads it, with the Machine it runs as. */
function view(o: { stopped?: boolean; placed?: boolean; node?: string; scheduled?: "true" | "false"; machine?: boolean }) {
  const container = create(ContainerSchema, {
    metadata: { name: "web", project: "demo" },
    spec: { image: "nginx:1", runState: o.stopped ? ContainerRunState.STOPPED : ContainerRunState.RUNNING },
    status: {
      placement: o.placed ? placement : undefined,
      conditions:
        o.scheduled === "false" ? [{ type: "Ready", status: ConditionStatus.FALSE, reason: "Unschedulable", message: noRoom }] : [],
    },
  });
  const machine =
    o.machine === false
      ? undefined
      : create(MachineSchema, {
          metadata: { name: "ct-web", project: "demo" },
          spec: { node: o.node ?? "" },
          status: {
            conditions:
              o.scheduled === "false"
                ? [{ type: "Scheduled", status: ConditionStatus.FALSE, reason: "NoNodeAvailable", message: noRoom }]
                : o.scheduled === "true"
                  ? [{ type: "Scheduled", status: ConditionStatus.TRUE, reason: "Placed", message: "running on node sgp02-01" }]
                  : [],
          },
        });
  return containerView(container, machine);
}

describe("runsOnState", () => {
  it("is scheduling while a running machine has no placement and the scheduler has not refused it", () => {
    expect(runsOnState(view({}))).toEqual({ kind: "scheduling" });
    // Placed on a node whose placement has not been copied yet: still on its way.
    expect(runsOnState(view({ node: "sgp02-01", scheduled: "true" }))).toEqual({ kind: "scheduling" });
  });

  it("is not schedulable, with the scheduler's words, when its Machine is not Scheduled", () => {
    expect(runsOnState(view({ scheduled: "false" }))).toEqual({ kind: "unschedulable", message: noRoom });
  });

  it("takes the container's Ready condition when the Machine is not known", () => {
    expect(runsOnState(view({ scheduled: "false", machine: false }))).toEqual({ kind: "unschedulable", message: noRoom });
  });

  it("is stopped when asked to stay stopped and never placed", () => {
    expect(runsOnState(view({ stopped: true }))).toEqual({ kind: "stopped" });
    expect(runsOnState(view({ stopped: true, scheduled: "false" }))).toEqual({ kind: "stopped" });
  });

  it("is placed, with its host, once it has a placement, stopped or not", () => {
    const s = runsOnState(view({ placed: true, node: "sgp02-01", scheduled: "true" }));
    expect(s.kind).toBe("placed");
    expect(s.kind === "placed" && s.host).toMatchObject({ node: "sgp02-01", cpuModel: "AMD EPYC 7313", art: "1u-2u2" });
    expect(runsOnState(view({ placed: true, node: "sgp02-01", stopped: true })).kind).toBe("placed");
  });

  it("goes from not schedulable to placed when a node is found later", () => {
    expect(runsOnState(view({ scheduled: "false" })).kind).toBe("unschedulable");
    // Room was made; the scheduler places it, then the node's placement is copied.
    expect(runsOnState(view({ node: "sgp02-01", scheduled: "true" })).kind).toBe("scheduling");
    expect(runsOnState(view({ placed: true, node: "sgp02-01", scheduled: "true" })).kind).toBe("placed");
  });
});
