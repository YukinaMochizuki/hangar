import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { GetOverviewResponseSchema, GetSeriesRequestSchema, GetSeriesResponseSchema, Health, Layer, SeriesName, Unit } from "@hangar/api/hangar/monitoring/v1alpha1/monitoring_pb";
import { describe, expect, it } from "vitest";
import { axisMax, plotPaths } from "@/components/series-chart";
import { formatValue, isUnavailable, since, toAlerts, toOverview, toSeries } from "@/lib/monitoring";
import { alerts, nodeOverview, now, overview, series } from "@/test/monitoring";

describe("toOverview", () => {
  it("reads the platform's layers and each node's, two nodes kept apart", () => {
    const o = toOverview(overview([nodeOverview("a"), nodeOverview("b", Health.WARNING)]));
    expect(o.observeTime).toEqual(now);
    expect(o.reachability?.health).toBe("unknown");
    expect(o.machines?.health).toBe("critical");
    expect(o.nodes.map((n) => [n.name, n.host?.health, n.services?.health])).toEqual([
      ["a", "ok", "ok"],
      ["b", "warning", "ok"],
    ]);
    expect(o.nodes[1].services?.checks.at(-1)).toEqual({ name: "Node b", health: "ok", value: "Ready" });
  });

  it("reads UNKNOWN, and an unset health, as no data", () => {
    const o = toOverview(overview());
    const host = o.nodes[0].host!;
    expect(host.checks.find((c) => c.name === "md127 mirror")?.health).toBe("unknown");
    const unset = toOverview(create(GetOverviewResponseSchema, { layers: [{ layer: Layer.REACHABILITY, health: Health.UNSPECIFIED }] }));
    expect(unset.reachability?.health).toBe("unknown");
  });

  it("keeps the six phases in the order the API gives, and the machines' counts", () => {
    const m = toOverview(overview()).machines!.machines!;
    expect(m.phases.map((p) => p.phase)).toEqual(["Running", "Pending", "Creating", "Stopped", "Suspended", "Failed"]);
    expect(m.platformFailures).toEqual([{ reason: "DiskFailed", count: 1 }]);
    expect(m.programFailures).toEqual([{ reason: "Exited", count: 1 }]);
  });

  it("leaves out a layer the answer lacks", () => {
    const o = toOverview(overview([{ name: "bare", layers: [] }]));
    expect(o.nodes[0]).toEqual({ name: "bare", host: undefined, services: undefined });
  });
});

describe("toSeries", () => {
  it("keeps capacity and alert_above when set", () => {
    const s = toSeries("HOST_MEMORY", series(create(GetSeriesRequestSchema, { name: SeriesName.HOST_MEMORY, node: "dev" })));
    expect(s.unit).toBe("bytes");
    expect(s.capacity).toBe(8 * 1024 ** 3);
    expect(s.alertAbove).toBeCloseTo(0.9 * 8 * 1024 ** 3);
    expect(s.points).toHaveLength(3);
  });

  it("has no threshold when the API gives none, as for HOST_CPU", () => {
    const s = toSeries("HOST_CPU", series(create(GetSeriesRequestSchema, { name: SeriesName.HOST_CPU, node: "dev" })));
    expect("alertAbove" in s).toBe(false);
    expect("capacity" in s).toBe(false);
    expect(axisMax(s)).toBe(1);
  });

  it("drops a point without a time", () => {
    const s = toSeries("API_REQUEST_RATE", create(GetSeriesResponseSchema, { unit: Unit.PER_SECOND, stepSeconds: 60, points: [{ value: 1 }] }));
    expect(s.points).toEqual([]);
  });
});

describe("toAlerts", () => {
  it("reads severity, a severity unlabelled counting as critical", () => {
    expect(toAlerts(alerts())[0]).toMatchObject({ name: "TestAlert", severity: "warning", summary: "A test alert." });
    const r = alerts();
    r.alerts[0].severity = 0;
    expect(toAlerts(r)[0].severity).toBe("critical");
  });
});

describe("isUnavailable", () => {
  it("is Unavailable alone", () => {
    expect(isUnavailable(new ConnectError("down", Code.Unavailable))).toBe(true);
    expect(isUnavailable(new ConnectError("no", Code.PermissionDenied))).toBe(false);
    expect(isUnavailable(new Error("x"))).toBe(false);
  });
});

describe("plotPaths", () => {
  it("places points by time over the range, and breaks the line where a step has none", () => {
    const at = (min: number, value: number) => ({ time: new Date(now.getTime() - min * 60_000), value });
    const s = { name: "API_REQUEST_RATE" as const, unit: "per_second" as const, stepSeconds: 60, points: [at(60, 1), at(59, 1), at(1, 1), at(0, 1)] };
    const { line } = plotPaths(s, "1h", 2, now);
    expect(line.match(/M/g)).toHaveLength(2);
    expect(line.startsWith("M0.00,50.00")).toBe(true);
    expect(line.endsWith("300.00,50.00")).toBe(true);
  });

  it("draws a point with no neighbour as a dot, as a line through it would have no length", () => {
    const at = (min: number, value: number) => ({ time: new Date(now.getTime() - min * 60_000), value });
    const s = { name: "HOST_CPU" as const, unit: "ratio" as const, stepSeconds: 900, points: [at(0, 0.5)] };
    const { line, area, dots } = plotPaths(s, "24h", 1, now);
    expect(line).toBe("");
    expect(area).toBe("");
    expect(dots).toBe("M300.00,50.00L300.00,50.00");
  });
});

describe("formatValue", () => {
  it("drops trailing zeros, so an axis reads round numbers", () => {
    expect(formatValue(128 * 1024 ** 3, "bytes")).toBe("128 GiB");
    expect(formatValue(74.24 * 1024 ** 3, "bytes")).toBe("74.2 GiB");
    expect(formatValue(512 * 1024 ** 2, "bytes")).toBe("512 MiB");
    expect(formatValue(1.44 * 1024 ** 4, "bytes")).toBe("1.44 TiB");
    expect(formatValue(0.05, "ratio")).toBe("5%");
    expect(formatValue(0.0011, "ratio")).toBe("0.11%");
    expect(formatValue(0.234, "ratio")).toBe("23%");
    expect(formatValue(1, "seconds")).toBe("1 s");
    expect(formatValue(0.1114, "seconds")).toBe("111 ms");
    expect(formatValue(20, "per_second")).toBe("20/s");
    expect(formatValue(0.25, "per_second")).toBe("0.25/s");
  });
});

describe("since", () => {
  it("reads minutes, hours and days", () => {
    expect(since(new Date("2026-10-20T11:13:00Z"), now)).toBe("47 min");
    expect(since(new Date("2026-10-20T08:48:00Z"), now)).toBe("3 h 12 min");
    expect(since(new Date("2026-10-18T08:00:00Z"), now)).toBe("2 d 4 h");
  });
});
