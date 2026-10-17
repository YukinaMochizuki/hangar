import { create } from "@bufbuild/protobuf";
import { ContainerSchema } from "@hangar/api/hangar/compute/v1alpha1/container_pb";
import { PlacementSchema } from "@hangar/api/hangar/compute/v1alpha1/placement_pb";
import { VirtualMachineSchema } from "@hangar/api/hangar/compute/v1alpha1/virtual_machine_pb";
import { describe, expect, it } from "vitest";
import { hostFor, placementOf } from "@/lib/placement";

const full = create(PlacementSchema, {
  region: "ap-southeast",
  site: "SGP02",
  cpuModel: "AMD EPYC 7313",
  cpuGeneration: "Zen 3 (Milan)",
  storage: "2 × NVMe SSD",
  chassis: "generic-1u-2x-u2",
  memory: "DDR4-3200 ECC",
  network: "2 × 10 GbE",
});

describe("placementOf", () => {
  it("takes every field from the API's placement, and draws a chassis it knows", () => {
    expect(placementOf(full, "sgp02-01")).toEqual({
      node: "sgp02-01",
      region: "ap-southeast",
      site: "SGP02",
      cpuModel: "AMD EPYC 7313",
      cpuGeneration: "Zen 3 (Milan)",
      storage: "2 × NVMe SSD",
      chassis: "generic-1u-2x-u2",
      memory: "DDR4-3200 ECC",
      network: "2 × 10 GbE",
      art: "1u-2u2",
    });
  });

  // The node leaves empty what it does not know; the panel shows nothing
  // for it, and draws a generic server.
  it("keeps empty fields empty", () => {
    const h = placementOf(create(PlacementSchema, { cpuModel: "AMD EPYC 7313" }), "n1");
    expect(h).toMatchObject({ cpuModel: "AMD EPYC 7313", region: "", site: "", memory: "", network: "", storage: "", cpuGeneration: "" });
    expect(h?.art).toBe("unknown");
  });

  it("draws a chassis it has no drawing of as a generic server", () => {
    const h = placementOf(create(PlacementSchema, { ...full, chassis: "custom-4u-tower" }), "n1");
    expect(h?.chassis).toBe("custom-4u-tower");
    expect(h?.art).toBe("unknown");
  });

  it("is nothing while there is no placement", () => {
    expect(placementOf(undefined, "n1")).toBeUndefined();
  });
});

describe("hostFor", () => {
  it("reads a container's placement", () => {
    const container = create(ContainerSchema, { status: { placement: full } });
    expect(hostFor({ container, node: "sgp02-01" })?.cpuModel).toBe("AMD EPYC 7313");
  });

  it("reads a VM's placement", () => {
    const vm = create(VirtualMachineSchema, { status: { placement: { ...full, chassis: "mini" } } });
    expect(hostFor({ vm, node: "sgp02-01" })?.art).toBe("mini");
  });

  it("is nothing for a machine not placed yet, though its node is named", () => {
    const container = create(ContainerSchema, { status: {} });
    expect(hostFor({ container, node: "sgp02-01" })).toBeUndefined();
    expect(hostFor({ node: "" })).toBeUndefined();
  });
});
