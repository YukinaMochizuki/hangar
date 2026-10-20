// MonitoringService answers for tests, as the apiserver gives them, and an
// in-memory API serving them with TestPermissions.

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { AccountService } from "@hangar/api/hangar/iam/v1alpha1/account_pb";
import {
  GetOverviewResponseSchema,
  GetSeriesResponseSchema,
  Health,
  Layer,
  ListAlertsResponseSchema,
  MonitoringService,
  SeriesName,
  Severity,
  Unit,
  type GetOverviewResponse,
  type GetSeriesRequest,
  type GetSeriesResponse,
  type ListAlertsResponse,
} from "@hangar/api/hangar/monitoring/v1alpha1/monitoring_pb";
import { fakeApi } from "@/test/harness";

export const now = new Date("2026-10-20T12:00:00Z");

const check = (name: string, health: Health, value: string) => ({ name, health, value });

/** A node's layers: its host with a check of no data, and its services. */
export function nodeOverview(name: string, hostHealth = Health.OK) {
  return {
    name,
    layers: [
      {
        layer: Layer.HOST,
        health: hostHealth,
        checks: [check("CPU", Health.OK, "4% · load 0.3"), check("md127 mirror", Health.UNKNOWN, "no data"), check("nvme0", Health.UNKNOWN, "")],
      },
      { layer: Layer.SERVICES, health: Health.OK, checks: [check("hangar-apiserver", Health.OK, "active · 0 restarts 24 h"), check(`Node ${name}`, Health.OK, "Ready")] },
    ],
  };
}

export function overview(nodes = [nodeOverview("dev")]): GetOverviewResponse {
  return create(GetOverviewResponseSchema, {
    observeTime: timestampFromDate(now),
    layers: [
      { layer: Layer.REACHABILITY, health: Health.UNKNOWN, checks: [check("API /healthz", Health.UNKNOWN, "no data")] },
      {
        layer: Layer.MACHINES,
        health: Health.CRITICAL,
        machines: {
          phases: ["Running", "Pending", "Creating", "Stopped", "Suspended", "Failed"].map((phase, i) => ({ phase, count: [3, 0, 0, 1, 0, 2][i] })),
          platformFailures: [{ reason: "DiskFailed", count: 1 }],
          programFailures: [{ reason: "Exited", count: 1 }],
          crashLooping: 0,
        },
      },
    ],
    nodes,
  });
}

export function series(req: GetSeriesRequest): GetSeriesResponse {
  const cpu = req.name === SeriesName.HOST_CPU;
  return create(GetSeriesResponseSchema, {
    name: req.name,
    node: req.node,
    unit: cpu ? Unit.RATIO : req.name === SeriesName.HOST_MEMORY ? Unit.BYTES : Unit.PER_SECOND,
    stepSeconds: 60,
    points: [0, 1, 2].map((i) => ({ time: timestampFromDate(new Date(now.getTime() - (2 - i) * 60_000)), value: 0.2 + i / 10 })),
    ...(req.name === SeriesName.HOST_MEMORY ? { capacity: 8 * 1024 ** 3, alertAbove: 0.9 * 8 * 1024 ** 3 } : {}),
  });
}

export function alerts(): ListAlertsResponse {
  return create(ListAlertsResponseSchema, {
    alerts: [{ name: "TestAlert", severity: Severity.WARNING, summary: "A test alert.", startTime: timestampFromDate(new Date(now.getTime() - 5 * 60_000)) }],
  });
}

export const unavailable = () => new ConnectError("Prometheus at http://127.0.0.1:9090: connection refused", Code.Unavailable);

/** An API serving MonitoringService, the caller holding what perms names. */
export function monitoringApi(f: {
  perms?: string[];
  overview?: () => GetOverviewResponse;
  alerts?: () => ListAlertsResponse;
  series?: (req: GetSeriesRequest) => GetSeriesResponse;
} = {}) {
  return fakeApi((r) => {
    r.service(AccountService, {
      testPermissions: (req) => ({ permissions: req.permissions.filter((p) => (f.perms ?? ["platform.monitoring.read"]).includes(p)) }),
    });
    r.service(MonitoringService, {
      getOverview: () => (f.overview ?? overview)(),
      listAlerts: () => (f.alerts ?? alerts)(),
      getSeries: (req) => (f.series ?? series)(req),
    });
  });
}
