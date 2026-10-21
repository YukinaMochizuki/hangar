// The platform's monitoring, as the Monitoring page reads it, from
// MonitoringService (docs/decisions/monitoring.md): the answers mapped to
// the page's own shapes, so the page holds no protobuf enum and the mapping
// is tested on its own.

import { timestampDate, type Timestamp } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { useQuery } from "@connectrpc/connect-query";
import { skipToken } from "@tanstack/react-query";
import {
  Health as PbHealth,
  Layer as PbLayer,
  MonitoringService,
  SeriesName as PbSeriesName,
  SeriesRange as PbSeriesRange,
  Severity as PbSeverity,
  Unit as PbUnit,
  type GetOverviewResponse,
  type GetSeriesResponse,
  type LayerState,
  type ListAlertsResponse,
} from "@hangar/api/hangar/monitoring/v1alpha1/monitoring_pb";
import { shouldRetry } from "@/lib/api";

/** A layer's or a check's state. "info" is recorded and alerts no one; "unknown" is no data. */
export type Health = "ok" | "warning" | "critical" | "info" | "unknown";

export type Check = { name: string; health: Health; value: string };

export type ReasonCount = { reason: string; count: number };

export type MachineCounts = {
  phases: { phase: string; count: number }[];
  platformFailures: ReasonCount[];
  programFailures: ReasonCount[];
  crashLooping: number;
};

export type LayerName = "reachability" | "host" | "services" | "machines";

export type Layer = { layer: LayerName; health: Health; checks: Check[]; machines?: MachineCounts };

export type NodeLayers = { name: string; host?: Layer; services?: Layer };

/** GetOverview's answer: the platform's two layers, and each node's two. */
export type Overview = { observeTime?: Date; reachability?: Layer; machines?: Layer; nodes: NodeLayers[] };

export type SeriesName = "HOST_CPU" | "HOST_MEMORY" | "POOL_USED" | "API_REQUEST_RATE" | "API_ERROR_RATE" | "API_LATENCY_P99";

export type SeriesRange = "1h" | "24h" | "7d";

export type SeriesUnit = "ratio" | "bytes" | "per_second" | "seconds";

export type Point = { time: Date; value: number };

export type Series = {
  name: SeriesName;
  unit: SeriesUnit;
  stepSeconds: number;
  points: Point[];
  capacity?: number;
  alertAbove?: number;
};

export type Severity = "critical" | "warning";

/** An alert firing; node is the Node it is for, empty for the platform's. */
export type Alert = { name: string; severity: Severity; summary: string; startTime?: Date; node: string };

/** What reading the page's data needs at the platform. */
export const monitoringPermission = "platform.monitoring.read";

/** The node's series; the others are the platform's and name no node. */
export const nodeSeries: ReadonlySet<SeriesName> = new Set(["HOST_CPU", "HOST_MEMORY", "POOL_USED"]);

/** How long each range reaches back, in seconds. */
export const rangeSeconds: Record<SeriesRange, number> = { "1h": 3600, "24h": 86_400, "7d": 604_800 };

const health: Record<PbHealth, Health> = {
  [PbHealth.UNSPECIFIED]: "unknown",
  [PbHealth.OK]: "ok",
  [PbHealth.WARNING]: "warning",
  [PbHealth.CRITICAL]: "critical",
  [PbHealth.INFO]: "info",
  [PbHealth.UNKNOWN]: "unknown",
};

const layerName: Partial<Record<PbLayer, LayerName>> = {
  [PbLayer.REACHABILITY]: "reachability",
  [PbLayer.HOST]: "host",
  [PbLayer.SERVICES]: "services",
  [PbLayer.MACHINES]: "machines",
};

const date = (ts?: Timestamp) => (ts ? timestampDate(ts) : undefined);

function toLayer(l: LayerState): Layer | undefined {
  const layer = layerName[l.layer];
  if (!layer) return undefined;
  const m = l.machines;
  return {
    layer,
    health: health[l.health] ?? "unknown",
    checks: l.checks.map((c) => ({ name: c.name, health: health[c.health] ?? "unknown", value: c.value })),
    ...(layer === "machines" && m
      ? {
          machines: {
            phases: m.phases.map((p) => ({ phase: p.phase, count: p.count })),
            platformFailures: m.platformFailures.map((r) => ({ reason: r.reason, count: r.count })),
            programFailures: m.programFailures.map((r) => ({ reason: r.reason, count: r.count })),
            crashLooping: m.crashLooping,
          },
        }
      : {}),
  };
}

function byName(layers: LayerState[]): Partial<Record<LayerName, Layer>> {
  const out: Partial<Record<LayerName, Layer>> = {};
  for (const l of layers) {
    const v = toLayer(l);
    if (v) out[v.layer] = v;
  }
  return out;
}

/** The overview as the page reads it; a layer the answer lacks is left out. */
export function toOverview(r: GetOverviewResponse): Overview {
  const platform = byName(r.layers);
  return {
    observeTime: date(r.observeTime),
    reachability: platform.reachability,
    machines: platform.machines,
    nodes: r.nodes.map((n) => {
      const l = byName(n.layers);
      return { name: n.name, host: l.host, services: l.services };
    }),
  };
}

const seriesName: Record<SeriesName, PbSeriesName> = {
  HOST_CPU: PbSeriesName.HOST_CPU,
  HOST_MEMORY: PbSeriesName.HOST_MEMORY,
  POOL_USED: PbSeriesName.POOL_USED,
  API_REQUEST_RATE: PbSeriesName.API_REQUEST_RATE,
  API_ERROR_RATE: PbSeriesName.API_ERROR_RATE,
  API_LATENCY_P99: PbSeriesName.API_LATENCY_P99,
};

const seriesRange: Record<SeriesRange, PbSeriesRange> = {
  "1h": PbSeriesRange.ONE_HOUR,
  "24h": PbSeriesRange.ONE_DAY,
  "7d": PbSeriesRange.SEVEN_DAYS,
};

const unit: Partial<Record<PbUnit, SeriesUnit>> = {
  [PbUnit.RATIO]: "ratio",
  [PbUnit.BYTES]: "bytes",
  [PbUnit.PER_SECOND]: "per_second",
  [PbUnit.SECONDS]: "seconds",
};

/** A series as the page draws it; its name is the one asked for. */
export function toSeries(name: SeriesName, r: GetSeriesResponse): Series {
  return {
    name,
    unit: unit[r.unit] ?? "ratio",
    stepSeconds: r.stepSeconds,
    points: r.points.flatMap((p) => (p.time ? [{ time: timestampDate(p.time), value: p.value }] : [])),
    ...(r.capacity !== undefined ? { capacity: r.capacity } : {}),
    ...(r.alertAbove !== undefined ? { alertAbove: r.alertAbove } : {}),
  };
}

/** The alerts firing, in the order the API gives: most severe, then oldest, first. */
export function toAlerts(r: ListAlertsResponse): Alert[] {
  return r.alerts.map((a) => ({
    name: a.name,
    // A severity the rule does not label counts as critical, as Alertmanager routes it.
    severity: a.severity === PbSeverity.WARNING ? "warning" : "critical",
    summary: a.summary,
    startTime: date(a.startTime),
    node: a.node,
  }));
}

/** Whether the API said it has no Prometheus or Alertmanager to ask, or cannot reach it. */
export function isUnavailable(err: unknown): boolean {
  return err instanceof ConnectError && err.code === Code.Unavailable;
}

// Unavailable is MonitoringService's answer while Prometheus is down or
// unset: shown at once, not retried, and asked again at the next interval.
const retry = (failures: number, err: unknown) => !isUnavailable(err) && shouldRetry(failures, err);

type Answer<T> = { data: T | undefined; loading: boolean; error: unknown; refetch: () => void };

export function useMonitoringOverview(enabled: boolean): Answer<Overview> {
  const q = useQuery(MonitoringService.method.getOverview, enabled ? {} : skipToken, { refetchInterval: 30_000, retry });
  return { data: q.data && toOverview(q.data), loading: q.isPending && enabled, error: q.error, refetch: () => void q.refetch() };
}

export function useMonitoringAlerts(enabled: boolean): Answer<Alert[]> {
  const q = useQuery(MonitoringService.method.listAlerts, enabled ? {} : skipToken, { refetchInterval: 30_000, retry });
  return { data: q.data && toAlerts(q.data), loading: q.isPending && enabled, error: q.error, refetch: () => void q.refetch() };
}

/** A series over the range; a node's series is asked only once a node is named. */
export function useMonitoringSeries(name: SeriesName, range: SeriesRange, node: string): Answer<Series> {
  const perNode = nodeSeries.has(name);
  const ask = perNode ? node !== "" : true;
  const q = useQuery(
    MonitoringService.method.getSeries,
    ask ? { name: seriesName[name], range: seriesRange[range], node: perNode ? node : "" } : skipToken,
    { refetchInterval: 60_000, retry },
  );
  return { data: q.data && toSeries(name, q.data), loading: q.isPending && ask, error: q.error, refetch: () => void q.refetch() };
}

const GiB = 1024 ** 3;
const TiB = 1024 ** 4;

/** A series' value as people read it: "23%", "74.2 GiB", "1.42 TiB", "7.1/s", "112 ms". */
export function formatValue(v: number, unit: SeriesUnit): string {
  // Trailing zeros go, so an axis reads "128 GiB" and "5%".
  const f = (x: number, digits: number) => String(Number(x.toFixed(digits)));
  switch (unit) {
    case "ratio":
      return v < 0.01 && v > 0 ? `${f(v * 100, 2)}%` : v < 0.1 ? `${f(v * 100, 1)}%` : `${Math.round(v * 100)}%`;
    case "bytes":
      return v >= TiB ? `${f(v / TiB, 2)} TiB` : v >= GiB ? `${f(v / GiB, v >= 100 * GiB ? 0 : 1)} GiB` : `${f(v / 1024 ** 2, 0)} MiB`;
    case "per_second":
      return `${f(v, v < 1 ? 2 : 1)}/s`;
    case "seconds":
      return v >= 1 ? `${f(v, 2)} s` : `${Math.round(v * 1000)} ms`;
  }
}

/** How long ago, as "47 min", "3 h 12 min", "2 d 4 h". */
export function since(from: Date, now = new Date()): string {
  const m = Math.max(0, Math.floor((now.getTime() - from.getTime()) / 60_000));
  if (m < 60) return `${m} min`;
  const h = Math.floor(m / 60);
  if (h < 24) return m % 60 ? `${h} h ${m % 60} min` : `${h} h`;
  const d = Math.floor(h / 24);
  return h % 24 ? `${d} d ${h % 24} h` : `${d} d`;
}

/** A time on a chart's axis: the hour for ranges within a day, the date beyond. */
export function axisTime(t: Date, range: SeriesRange): string {
  const iso = t.toISOString();
  return range === "7d" ? iso.slice(5, 10) : iso.slice(11, 16);
}
