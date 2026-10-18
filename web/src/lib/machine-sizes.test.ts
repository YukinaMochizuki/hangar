import { create } from "@bufbuild/protobuf";
import { MachineSizeSchema } from "@hangar/api/hangar/compute/v1alpha1/machine_size_pb";
import { describe, expect, it } from "vitest";
import { sizeOf, sortSizes } from "@/lib/machine-sizes";

/** A made-up size; a platform's real sizes are its administrator's. */
const size = (name: string, displayName: string, cpu: string, memory: string, disk: string, order: number) =>
  create(MachineSizeSchema, { metadata: { name }, spec: { displayName, cpu, memory, disk, order } });

describe("sortSizes", () => {
  it("lists by order, then by name where orders tie", () => {
    const got = sortSizes([
      size("big", "Big", "8", "16Gi", "80Gi", 30),
      size("b", "B", "2", "4Gi", "20Gi", 10),
      size("a", "A", "1", "2Gi", "10Gi", 10),
      size("tiny", "Tiny", "1", "512Mi", "5Gi", 0),
    ]);
    expect(got.map((s) => s.name)).toEqual(["tiny", "a", "b", "big"]);
  });

  it("reads the quantities, and keeps them as written to send", () => {
    const [s] = sortSizes([size("m", "M", "2000m", "4096Mi", "20Gi", 1)]);
    expect(s).toMatchObject({ label: "M", cpu: 2, memoryGi: 4, diskGi: 20, resources: { cpu: "2000m", memory: "4096Mi", disk: "20Gi" } });
  });

  it("labels a size with no display name by its name", () => {
    expect(sortSizes([size("m", "", "2", "4Gi", "20Gi", 1)])[0].label).toBe("m");
  });
});

describe("sizeOf", () => {
  const sizes = sortSizes([size("s", "S", "1", "2Gi", "10Gi", 1), size("m", "M", "2", "4Gi", "20Gi", 2)]);

  it("matches a machine by cpu and memory, whatever its disk", () => {
    expect(sizeOf({ cpu: 2, memoryGi: 4 }, sizes)?.label).toBe("M");
  });

  it("matches nothing when only one of them is a size's", () => {
    expect(sizeOf({ cpu: 2, memoryGi: 2 }, sizes)).toBeUndefined();
  });

  it("matches nothing when the platform lists no size", () => {
    expect(sizeOf({ cpu: 1, memoryGi: 2 }, [])).toBeUndefined();
  });
});
