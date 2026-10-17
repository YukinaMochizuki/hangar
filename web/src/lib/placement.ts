// Where a machine runs, as a tenant may see it: the host's hardware, never
// how full the host is (docs/decisions/machine-placement.md). It is the
// container's or VM's status.placement, which the apiserver copies from the
// node once the machine is placed. Everything that reads placement goes
// through hostFor.

import type { Placement as ApiPlacement } from "@hangar/api/hangar/compute/v1alpha1/placement_pb";
import type { MachineView } from "@/lib/machines";

/** The chassis the console has a drawing of (components/chassis-art.tsx). */
export type Chassis = "1u-2u2" | "storage-2u" | "mini" | "unknown";

/** Placement as the console shows it; an empty field is one the node does not know. */
export type Placement = {
  region: string;
  site: string;
  cpuModel: string;
  cpuGeneration: string;
  storage: string;
  /** A DNS-1123 label naming the chassis, such as "generic-1u-2x-u2". */
  chassis: string;
  memory: string;
  network: string;
};

export type HostInfo = Placement & { node: string; art: Chassis };

// The chassis labels the console draws; any other, and none, is drawn as a
// generic server.
const drawings: Record<string, Chassis> = {
  "generic-1u-2x-u2": "1u-2u2",
  "storage-2u": "storage-2u",
  mini: "mini",
};

/** The host a placement describes; undefined when there is none yet. */
export function placementOf(p: ApiPlacement | undefined, node: string): HostInfo | undefined {
  if (!p) return undefined;
  return {
    node,
    region: p.region,
    site: p.site,
    cpuModel: p.cpuModel,
    cpuGeneration: p.cpuGeneration,
    storage: p.storage,
    chassis: p.chassis,
    memory: p.memory,
    network: p.network,
    art: drawings[p.chassis] ?? "unknown",
  };
}

/** The host a machine runs on; undefined until the machine is placed. */
export function hostFor(m: Pick<MachineView, "container" | "vm" | "node">): HostInfo | undefined {
  return placementOf(m.container?.status?.placement ?? m.vm?.status?.placement, m.node);
}
