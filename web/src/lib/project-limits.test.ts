import { create } from "@bufbuild/protobuf";
import { LimitsSchema } from "@hangar/api/hangar/tenancy/v1alpha1/plan_pb";
import { describe, expect, it } from "vitest";
import { changeText, draftOf, readDraft, setLimitsCommand } from "@/lib/project-limits";

const base = create(LimitsSchema, { cpu: "2", memory: "4096Mi", machines: 3 });
const plan = create(LimitsSchema, { cpu: "8", memory: "16Gi", disk: "100Gi", machines: 10 });

describe("draftOf", () => {
  it("shows each limit in the field's unit, and an empty field for none", () => {
    const d = draftOf(create(LimitsSchema, { cpu: "2500m", memory: "1536Mi", disk: "20Gi", machines: 0 }));
    expect(d.cpu).toBe("2.5");
    expect(d.memory).toBe("1.5");
    expect(d.disk).toBe("20");
    // Zero is a limit, not none.
    expect(d.machines).toBe("0");
    expect(d.networks).toBe("");
  });
});

describe("readDraft", () => {
  it("changes nothing, and keeps the API's text, for fields not edited", () => {
    const r = readDraft(base, draftOf(base), plan);
    expect(r.changes).toEqual([]);
    expect(r.problems).toEqual([]);
    expect(r.limits.memory).toBe("4096Mi");
  });

  it("writes vCPUs and sizes as the API takes them, and an emptied field as no limit", () => {
    const r = readDraft(base, { ...draftOf(base), cpu: "2.5", memory: "", disk: "1.5", networks: "4" }, plan);
    expect(r.problems).toEqual([]);
    expect(r.limits.cpu).toBe("2500m");
    expect(r.limits.memory).toBeUndefined();
    expect(r.limits.disk).toBe("1536Mi");
    expect(r.limits.networks).toBe(4);
    // Machines was not edited.
    expect(r.limits.machines).toBe(3);
    expect(r.changes.map(changeText)).toEqual(["vCPU 2 → 2.5", "Memory 4 GiB → plan's", "Disk plan's → 1.5 GiB", "Networks plan's → 4"]);
    expect(setLimitsCommand("web", r.limits, r.changes)).toBe("hangar project set-limits web --cpu 2500m --disk 1536Mi --networks 4 --unset memory");
  });

  it("says why a field cannot be applied: not a number, not whole, finer than the API takes, or above the plan", () => {
    const r = readDraft(base, { ...draftOf(base), cpu: "16", machines: "2.5", networks: "-1", disk: "0.0001" }, plan);
    expect(r.problems).toEqual([
      "Machines must be a whole number.",
      "vCPU 16 is above the plan's 8.",
      "Disk is finer than a MiB.",
      "Networks must be a number.",
    ]);
  });

  it("checks nothing against a plan the caller may not read", () => {
    expect(readDraft(base, { ...draftOf(base), cpu: "16" }, undefined).problems).toEqual([]);
  });
});
