// A pooled machine's usage says its CPU pool, and the line under its CPU
// plot says what it shares and how often that slowed it.

import { create } from "@bufbuild/protobuf";
import { UsageResponseSchema } from "@hangar/api/hangar/compute/v1alpha1/machine_capability_pb";
import { describe, expect, it } from "vitest";
import { cpuPoolLine } from "@/components/machine-usage";
import { toUsage } from "@/lib/usage";

describe("toUsage's CPU pool", () => {
  it("carries the pool's CPUs and slowed share when the node says them, and nothing otherwise", () => {
    expect(toUsage(create(UsageResponseSchema, { cpuPoolMilliCpu: 2500n, cpuPoolSlowed: 0.3 }), "Container").cpuPool).toEqual({ cpus: 2.5, slowed: 0.3 });
    expect(toUsage(create(UsageResponseSchema, { cpuPoolMilliCpu: 4000n }), "Container").cpuPool).toEqual({ cpus: 4, slowed: undefined });
    expect(toUsage(create(UsageResponseSchema, {}), "Container").cpuPool).toBeUndefined();
  });
});

describe("cpuPoolLine", () => {
  it("says what the machine shares, and how often it was slowed from 1%", () => {
    expect(cpuPoolLine({ cpus: 4, slowed: 0.12 })).toBe("Shares your plan's 4 CPUs with your other machines · slowed 12% in the last hour");
    expect(cpuPoolLine({ cpus: 4, slowed: 0.004 })).toBe("Shares your plan's 4 CPUs with your other machines.");
    expect(cpuPoolLine({ cpus: 2.5 })).toBe("Shares your plan's 2.5 CPUs with your other machines.");
  });
});
