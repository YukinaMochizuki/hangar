import { create } from "@bufbuild/protobuf";
import { MachineSizeSchema } from "@hangar/api/hangar/compute/v1alpha1/machine_size_pb";
import { describe, expect, it } from "vitest";
import { clampCPU, cpuRange, memoryLabel, memoryQuantity, memoryStops, shapeProblem, sizeOf, sliderLimits, sortSizes } from "@/lib/machine-sizes";

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

// As the apiserver's TestCPURange (internal/apiserver).
describe("cpuRange", () => {
  it("allows 512 MiB to 8 GiB of memory per vCPU, and one vCPU always", () => {
    const cases: [number, number, number][] = [
      [256, 1, 1],
      [512, 1, 1],
      [768, 1, 1],
      [1024, 1, 2],
      [4096, 1, 8],
      [8192, 1, 16],
      [8448, 2, 16],
      [12288, 2, 24],
      [32768, 4, 64],
    ];
    for (const [mib, lo, hi] of cases) expect(cpuRange(mib), `${mib} MiB`).toEqual({ lo, hi });
  });
});

describe("shapeProblem", () => {
  it("takes a shape within the rules", () => {
    for (const [cpu, mib] of [
      [1, 256],
      [2, 1024],
      [8, 4096],
      [2, 16384],
    ]) {
      expect(shapeProblem(cpu!, mib!), `${cpu} / ${mib}`).toBeUndefined();
    }
  });

  it("says why a shape breaks them", () => {
    expect(shapeProblem(1, 300)).toBe("Memory is a whole number of 256 MiB steps, at least one.");
    expect(shapeProblem(1, 0)).toBe("Memory is a whole number of 256 MiB steps, at least one.");
    expect(shapeProblem(1, NaN)).toBe("Memory is a whole number of 256 MiB steps, at least one.");
    expect(shapeProblem(1.5, 1024)).toBe("The vCPUs are a whole number, at least 1.");
    expect(shapeProblem(8, 256)).toBe("0.25 GiB of memory goes with 1 vCPU, not 8.");
    expect(shapeProblem(1, 32768)).toBe("32 GiB of memory goes with 4 to 64 vCPUs, not 1.");
  });
});

describe("memory as shown and sent", () => {
  it("shows GiB, and sends whole Gi as Gi and the rest as Mi", () => {
    expect(memoryLabel(1536)).toBe("1.5 GiB");
    expect(memoryQuantity(1536)).toBe("1536Mi");
    expect(memoryQuantity(2048)).toBe("2Gi");
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

describe("the Custom size's rulers", () => {
  const sizes = sortSizes([size("s", "S", "1", "2Gi", "10Gi", 1), size("xl", "XL", "8", "16Gi", "80Gi", 4)]);

  it("reach twice the largest size, and always a shape asked to fit", () => {
    expect(sliderLimits(sizes)).toEqual({ memMiB: 32768, cpu: 16 });
    expect(sliderLimits(sizes, [{ memMiB: 65536, cpu: 24 }])).toEqual({ memMiB: 65536, cpu: 24 });
    // With no size listed, a reach of their own.
    expect(sliderLimits([]).memMiB).toBe(16384);
  });

  it("never reach fewer vCPUs than their memory's end needs", () => {
    // 64 GiB needs at least 8 vCPUs, more than twice the largest size's 2.
    const small = sortSizes([size("s", "S", "1", "32Gi", "10Gi", 1)]);
    expect(sliderLimits(small).cpu).toBe(8);
  });

  it("stop at the common shapes up to the end, and at a machine's own memory off them", () => {
    expect(memoryStops(4096)).toEqual([256, 512, 1024, 1536, 2048, 3072, 4096]);
    expect(memoryStops(4096, [2816])).toEqual([256, 512, 1024, 1536, 2048, 2816, 3072, 4096]);
    // A memory the rules refuse is no stop.
    expect(memoryStops(4096, [1000, 0])).toEqual([256, 512, 1024, 1536, 2048, 3072, 4096]);
  });

  it("move the vCPUs into the range the memory allows", () => {
    expect(clampCPU(8, 2048)).toBe(4);
    expect(clampCPU(1, 16384)).toBe(2);
    expect(clampCPU(3, 2048)).toBe(3);
    expect(clampCPU(NaN, 16384)).toBe(2);
  });
});
