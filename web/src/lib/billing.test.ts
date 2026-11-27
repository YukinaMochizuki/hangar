import { create } from "@bufbuild/protobuf";
import { PricesSchema } from "@hangar/api/hangar/billing/v1alpha1/price_list_pb";
import { describe, expect, it } from "vitest";
import { formatMoney, formatPeriod, groupLines, itemLabels, outstanding, pricesOf, projected, sizePrice, vcpuPrice } from "@/lib/billing";
import { statement } from "@/test/billing";

describe("billing", () => {
  it("formats an amount with its currency's minor digits, and more when it carries them", () => {
    expect(formatMoney("10220.16", "TWD")).toBe("TWD 10,220.16");
    expect(formatMoney("300", "TWD")).toBe("TWD 300.00");
    expect(formatMoney("0.005", "TWD")).toBe("TWD 0.005");
    expect(formatMoney("1500", "JPY")).toBe("JPY 1,500");
    expect(formatMoney(undefined, "TWD")).toBe("—");
  });

  it("names a period by its days in the account's zone, the last one included", () => {
    // Midnight of Sep 1 to midnight of Dec 1 in Taipei.
    const start = new Date("2026-08-31T16:00:00Z");
    const end = new Date("2026-11-30T16:00:00Z");
    expect(formatPeriod(start, end, "Asia/Taipei")).toBe("Sep 1, 2026 – Nov 30, 2026");
    // Not at midnight there: to the minute.
    expect(formatPeriod(start, end, "UTC")).toBe("Aug 31, 2026, 16:00 – Nov 30, 2026, 16:00");
  });

  it("groups a statement's lines by item, in their order", () => {
    const groups = groupLines(statement("s").spec!.lines);
    expect(groups.map((g) => [g.item, g.lines.length, g.amount])).toEqual([
      ["cpu", 2, 5150.4],
      ["disk", 1, 1024.8],
    ]);
  });

  it("adds up what is due", () => {
    expect(outstanding([statement("a", { total: "10" }), statement("b", { total: "5.5" }), statement("c", { paid: true })])).toEqual({ count: 2, total: 15.5 });
  });

  it("projects a period from a day on", () => {
    const start = new Date("2026-11-01T00:00:00Z");
    const end = new Date("2026-12-01T00:00:00Z");
    expect(projected("100", start, end, new Date("2026-11-01T12:00:00Z"))).toBeUndefined();
    expect(projected("100", start, end, new Date("2026-11-11T00:00:00Z"))).toBeCloseTo(300);
  });

  it("names the burstable items, and lists their prices beside cpu's", () => {
    expect(itemLabels.cpu_burstable_20).toBe("vCPU, burstable 20%");
    expect(itemLabels.cpu_burstable_40).toBe("vCPU, burstable 40%");
    const p = create(PricesSchema, { cpu: "0.6", cpuBurstable20: "0.15", memory: "0.2" });
    expect(pricesOf(p).map((x) => x.item)).toEqual(["cpu", "cpu_burstable_20", "memory"]);
  });

  it("prices a vCPU at a baseline at its own price, else at cpu's times the baseline, as the meter does", () => {
    const p = create(PricesSchema, { cpu: "0.6", cpuBurstable20: "0.15", memory: "0.2" });
    expect(vcpuPrice(p, 100)).toBe("0.6");
    expect(vcpuPrice(p, 20)).toBe("0.15");
    expect(vcpuPrice(p, 40)).toBe("0.24");
    expect(vcpuPrice(create(PricesSchema, {}), 40)).toBeUndefined();
    expect(sizePrice(p, { cpu: 2, memoryGi: 4, baseline: 40 })).toBe("1.28");
  });
});
