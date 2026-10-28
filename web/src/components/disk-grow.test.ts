import { create } from "@bufbuild/protobuf";
import { ContainerSchema, type Container } from "@hangar/api/hangar/compute/v1alpha1/container_pb";
import { describe, expect, it } from "vitest";
import { diskGrowOf, diskGrowSpec, growthSteps, noDiskGrow, type DiskGrow } from "@/components/disk-grow";
import { changesOf, editedObject } from "@/lib/machine-edit";
import { wholeGiB } from "@/lib/quantity";

const on = (patch: Partial<DiskGrow>): DiskGrow => ({ on: true, maxGi: "20", threshold: "", step: "", ...patch });

describe("diskGrowSpec", () => {
  it("is nothing while off", () => {
    expect(diskGrowSpec(noDiskGrow, 2)).toEqual({ problems: [] });
  });

  it("leaves the defaults empty, and writes a GiB step as a quantity", () => {
    expect(diskGrowSpec(on({}), 2)).toEqual({ spec: { max: "20Gi", threshold: "", step: "" }, problems: [] });
    expect(diskGrowSpec(on({ threshold: "90", step: "3" }), 2).spec).toEqual({ max: "20Gi", threshold: "90%", step: "3Gi" });
    expect(diskGrowSpec(on({ step: "3 GiB" }), 2).spec?.step).toBe("3Gi");
    expect(diskGrowSpec(on({ step: "1.5x" }), 2).spec?.step).toBe("1.5x");
  });

  it("takes any quantity the API takes, as written", () => {
    for (const step of ["2048Mi", "1Ti", "4G", "3Gi"]) {
      expect(diskGrowSpec(on({ step }), 2), step).toEqual({ spec: { max: "20Gi", threshold: "", step }, problems: [] });
    }
    expect(diskGrowSpec(on({ step: "512Mi" }), 2).problems).toEqual(["Grow by: at least 1 GiB."]);
    expect(growthSteps(on({ step: "2048Mi", maxGi: "8" }), 2)).toEqual([2, 4, 6, 8]);
  });

  it("refuses what the apiserver refuses", () => {
    for (const [g, field] of [
      [on({ maxGi: "" }), "Grow up to"],
      [on({ maxGi: "1" }), "Grow up to"],
      [on({ threshold: "49" }), "When full"],
      [on({ threshold: "96" }), "When full"],
      [on({ step: "1x" }), "Grow by"],
      [on({ step: "17x" }), "Grow by"],
      [on({ step: "0" }), "Grow by"],
      [on({ step: "lots" }), "Grow by"],
    ] as const) {
      const { problems } = diskGrowSpec(g, 2);
      expect(problems.length, JSON.stringify(g)).toBe(1);
      expect(problems[0]).toMatch(new RegExp(`^${field}:`));
    }
  });
});

describe("growthSteps", () => {
  it("doubles by default and stops at the max", () => {
    expect(growthSteps(on({}), 2)).toEqual([2, 4, 8, 16, 20]);
  });
  it("adds a GiB step", () => {
    expect(growthSteps(on({ step: "3" }), 2)).toEqual([2, 5, 8, 11, 14, 17, 20]);
  });
  it("rounds a factor up to whole GiB, as the apiserver does", () => {
    expect(growthSteps(on({ maxGi: "10", step: "1.5x" }), 2)).toEqual([2, 3, 5, 8, 10]);
  });
  it("is nothing with no room to grow", () => {
    expect(growthSteps(on({ maxGi: "2" }), 2)).toEqual([]);
  });
});

describe("editing a growing container", () => {
  const growing = () =>
    create(ContainerSchema, {
      metadata: { name: "web", project: "demo" },
      spec: { image: "nginx", resources: { cpu: "1", memory: "512Mi", disk: "2Gi", diskGrow: { max: "20Gi", threshold: "", step: "3Gi" } } },
    });

  it("keeps diskGrow when only the size changes", () => {
    const next = editedObject(growing(), { cpu: "2" }) as Container;
    expect(next.spec?.resources?.diskGrow?.max).toBe("20Gi");
  });

  it("turns it off with null, and says so without a restart", () => {
    const base = growing();
    const next = editedObject(base, { diskGrow: null }) as Container;
    expect(next.spec?.resources?.diskGrow).toBeUndefined();
    expect(changesOf(base, next)).toEqual([expect.objectContaining({ field: "diskGrow", effect: "nothing", detail: "up to 20 GiB, at 80%, by 3Gi → off" })]);
  });

  it("reads back into the form", () => {
    expect(diskGrowOf(growing().spec?.resources?.diskGrow, wholeGiB)).toEqual({ on: true, maxGi: "20", threshold: "", step: "3 GiB" });
  });
});
