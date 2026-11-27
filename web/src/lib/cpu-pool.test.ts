// What decides a tenant's CPU pool on the console, and its vCPU tile's figures.

import { create } from "@bufbuild/protobuf";
import { BillingType } from "@hangar/api/hangar/billing/v1alpha1/billing_account_pb";
import { LimitsSchema, UsageSchema } from "@hangar/api/hangar/tenancy/v1alpha1/plan_pb";
import { describe, expect, it } from "vitest";
import { cpuPoolFigures, cpuPoolTerms } from "@/lib/cpu-pool";

const limits = (cpu: string | undefined, cpuLimitPercent?: number) => create(LimitsSchema, { cpu, cpuLimitPercent });

describe("cpuPoolTerms", () => {
  it("needs a percent above 100 and a cpu limit, and no PAY_AS_YOU_GO account", () => {
    expect(cpuPoolTerms(limits("4", 200))).toEqual({ pooled: true, percent: 200 });
    expect(cpuPoolTerms(limits("4", 200), BillingType.FIXED).pooled).toBe(true);
    expect(cpuPoolTerms(limits("4", 200), BillingType.PAY_AS_YOU_GO).pooled).toBe(false);
    expect(cpuPoolTerms(limits("4", 100)).pooled).toBe(false);
    expect(cpuPoolTerms(limits("4")).pooled).toBe(false);
    expect(cpuPoolTerms(limits(undefined, 200)).pooled).toBe(false);
    expect(cpuPoolTerms(undefined)).toEqual({ pooled: false, percent: 100 });
  });

  it("is apart from the memory pool's percent", () => {
    expect(cpuPoolTerms(create(LimitsSchema, { cpu: "4", memory: "8Gi", memoryLimitPercent: 200 })).pooled).toBe(false);
  });
});

describe("cpuPoolFigures", () => {
  it("is the plan's CPUs, the vCPUs asked to run, and their cap at the percent", () => {
    const usage = create(UsageSchema, { cpu: "6" });
    expect(cpuPoolFigures(limits("4", 200), usage, 200, 12)).toEqual({ planCPU: 4, vcpus: 6, capCPU: 8, slowedPercent: 12 });
    expect(cpuPoolFigures(limits("2500m", 400), undefined, 400)).toEqual({ planCPU: 2.5, vcpus: 0, capCPU: 10, slowedPercent: undefined });
  });
});
