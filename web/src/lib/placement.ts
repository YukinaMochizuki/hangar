// Where a machine runs, as a tenant may see it: the host's hardware, never
// how full the host is. The shape is the API's Placement (region, site, CPU
// model and generation, storage, chassis, memory, network). A machine's node
// name is read from Machine.spec.node; the placement behind it comes from a
// table in this file, and everything that reads placement goes through
// hostFor, so reading it from the API is a change to this file alone.

/** The chassis the console has a drawing of (components/chassis-art.tsx). */
export type Chassis = "1u-2u2" | "storage-2u" | "mini" | "unknown";

/** Placement as the API describes it, every field free text but chassis. */
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

// The chassis labels the console draws; any other is drawn as a generic
// server.
const drawings: Record<string, Chassis> = {
  "generic-1u-2x-u2": "1u-2u2",
  "storage-2u": "storage-2u",
  mini: "mini",
};

const standIn: Record<string, Placement> = {
  "sgp02-01": {
    region: "ap-southeast",
    site: "SGP02",
    cpuModel: "AMD EPYC 7313",
    cpuGeneration: "Zen 3 (Milan)",
    storage: "2 × NVMe SSD",
    chassis: "generic-1u-2x-u2",
    memory: "DDR4-3200 ECC",
    network: "2 × 10 GbE",
  },
};

/** The host a node name stands for; undefined until the machine is placed. */
export function hostFor(node: string): HostInfo | undefined {
  if (!node) return undefined;
  const p = standIn[node] ?? { region: "", site: "", cpuModel: "", cpuGeneration: "", storage: "", chassis: "", memory: "", network: "" };
  return { ...p, node, art: drawings[p.chassis] ?? "unknown" };
}
