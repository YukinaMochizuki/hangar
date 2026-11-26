// What decides a tenant's pool on the console, a Shared machine's default
// reservation, and when its node last killed it for memory.

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { BillingType } from "@hangar/api/hangar/billing/v1alpha1/billing_account_pb";
import { ContainerSchema } from "@hangar/api/hangar/compute/v1alpha1/container_pb";
import { MachineSchema } from "@hangar/api/hangar/compute/v1alpha1/machine_pb";
import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import { LimitsSchema } from "@hangar/api/hangar/tenancy/v1alpha1/plan_pb";
import { describe, expect, it } from "vitest";
import { containerView } from "@/lib/machines";
import { balloonStuck, defaultReservedMiB, killedRecently, lastOOMKill, machineMemory, outsidePool, poolFigures, poolTerms, reservedProblem } from "@/lib/memory-pool";

const limits = (memory: string | undefined, memoryLimitPercent?: number) => create(LimitsSchema, { memory, memoryLimitPercent });

describe("poolTerms", () => {
  it("needs a percent above 100 and a memory limit, and no PAY_AS_YOU_GO account", () => {
    expect(poolTerms(limits("8Gi", 200))).toEqual({ pooled: true, percent: 200 });
    expect(poolTerms(limits("8Gi", 200), BillingType.FIXED).pooled).toBe(true);
    expect(poolTerms(limits("8Gi", 200), BillingType.PAY_AS_YOU_GO).pooled).toBe(false);
    expect(poolTerms(limits("8Gi", 100)).pooled).toBe(false);
    expect(poolTerms(limits("8Gi")).pooled).toBe(false);
    expect(poolTerms(limits(undefined, 200)).pooled).toBe(false);
    expect(poolTerms(undefined)).toEqual({ pooled: false, percent: 100 });
  });
});

describe("defaultReservedMiB", () => {
  it("is memory divided by the percent, in 256 MiB steps, at least one", () => {
    expect(defaultReservedMiB(4096, 200)).toBe(2048);
    expect(defaultReservedMiB(4096, 300)).toBe(1280);
    expect(defaultReservedMiB(512, 400)).toBe(256);
    expect(defaultReservedMiB(256, 400)).toBe(256);
  });
});

describe("reservedProblem", () => {
  it("takes whole 256 MiB steps up to memory", () => {
    expect(reservedProblem(1536, 4096)).toBeUndefined();
    expect(reservedProblem(4096, 4096)).toBeUndefined();
    expect(reservedProblem(1500, 4096)).toMatch(/256 MiB steps/);
    expect(reservedProblem(0, 4096)).toMatch(/256 MiB steps/);
    expect(reservedProblem(NaN, 4096)).toMatch(/256 MiB steps/);
    expect(reservedProblem(4352, 4096)).toMatch(/at most/);
  });
});

describe("a machine's memory", () => {
  const view = (reserved: string, conditions: object[] = [], pool = true, stopped = false) =>
    containerView(
      create(ContainerSchema, { metadata: { name: "web", project: "demo" }, spec: { image: "nginx", runState: stopped ? 2 : 0, resources: { cpu: "2", memory: "4Gi", memoryReserved: reserved } } }),
      create(MachineSchema, { metadata: { name: "c-web" }, spec: { memoryPool: pool ? { name: "acme", size: "8192Mi" } : undefined }, status: { conditions } }),
    );

  it("is Shared when less than its memory is reserved, and empty is all of it", () => {
    expect(machineMemory(view("2Gi"))).toEqual({ memoryGi: 4, reservedGi: 2, shared: true });
    expect(machineMemory(view("1792Mi")).reservedGi).toBe(1.75);
    expect(machineMemory(view("")).shared).toBe(false);
    expect(machineMemory(view("4Gi")).shared).toBe(false);
  });

  it("takes when its VM was killed from the condition's message, news for a day", () => {
    const now = Date.parse("2026-11-26T10:04:07Z");
    const kill = (msg: string) => view("2Gi", [{ type: "OOMKilled", status: ConditionStatus.TRUE, reason: "OutOfMemory", message: msg, lastTransitionTime: timestampFromDate(new Date(now - 3 * 86_400_000)) }]);
    const m = kill("the host killed the machine's VM whole at 2026-11-26T09:41:07Z, its tenant's machines on the node out of the memory they share");
    expect(lastOOMKill(m)?.toISOString()).toBe("2026-11-26T09:41:07.000Z");
    expect(killedRecently(m, now)).toBeTruthy();
    expect(killedRecently(m, now + 86_400_000)).toBeUndefined();
    // A message without a time falls back on when the condition turned True.
    expect(lastOOMKill(kill("killed"))?.getTime()).toBe(now - 3 * 86_400_000);
    expect(lastOOMKill(view("2Gi"))).toBeUndefined();
  });

  it("knows a balloon that does not follow only from BalloonResponsive=False", () => {
    expect(balloonStuck(view("2Gi", [{ type: "BalloonResponsive", status: ConditionStatus.FALSE, reason: "BalloonUnresponsive" }]))?.reason).toBe("BalloonUnresponsive");
    expect(balloonStuck(view("2Gi", [{ type: "BalloonResponsive", status: ConditionStatus.TRUE }]))).toBeUndefined();
  });

  it("is outside its pool while it runs on a Machine that names none", () => {
    expect(outsidePool(view("2Gi", [], false))).toBe(true);
    expect(outsidePool(view("2Gi", [], true))).toBe(false);
    expect(outsidePool(view("2Gi", [], false, true))).toBe(false);
  });
});

describe("poolFigures", () => {
  it("counts the memory of machines asked to run against the plan's memory, and the cap at its percent", () => {
    expect(poolFigures(limits("8Gi", 200), { memoryCeiling: "11Gi" } as never, 200)).toEqual({ planGi: 8, ceilingGi: 11, capGi: 16 });
  });
});
