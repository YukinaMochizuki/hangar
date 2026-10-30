// MonitoringService and NodeService answers for tests, as the apiserver
// gives them, and an in-memory API serving them with TestPermissions and
// what ListNodeUsage sums per node.

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { NodeService } from "@hangar/api/hangar/compute/v1alpha1/node_pb";
import { AccountService } from "@hangar/api/hangar/iam/v1alpha1/account_pb";
import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
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
import { fakeApi, never } from "@/test/harness";

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

/** One alert of the platform's, naming no node; given nodes, one more for each, naming it. */
export function alerts(nodes: string[] = []): ListAlertsResponse {
  const at = timestampFromDate(new Date(now.getTime() - 5 * 60_000));
  return create(ListAlertsResponseSchema, {
    alerts: [
      { name: "TestAlert", severity: Severity.WARNING, summary: "A test alert.", startTime: at },
      ...nodes.map((node) => ({ name: `HostAlert-${node}`, severity: Severity.CRITICAL, summary: `Something on ${node}.`, startTime: at, node })),
    ],
  });
}

/**
 * A Node as the apiserver answers it: Ready unless said, with its placement,
 * 32 vCPUs, 128Gi and 840Gi, of which it offers machines 20, 76Gi and 496Gi
 * unless allocatable says otherwise.
 */
export function node(name: string, f: { ready?: ConditionStatus; cordoned?: boolean; allocatable?: { cpu: string; memory: string; disk: string } } = {}) {
  return {
    metadata: { name },
    spec: { unschedulable: f.cordoned ?? false },
    status: {
      conditions: [{ type: "Ready", status: f.ready ?? ConditionStatus.TRUE, reason: f.ready === ConditionStatus.UNKNOWN ? "NodeStatusUnknown" : "AgentReporting" }],
      capacity: { cpu: "32", memory: "128Gi", disk: "840Gi" },
      allocatable: f.allocatable ?? { cpu: "20", memory: "76Gi", disk: "496Gi" },
      lastHeartbeatTime: timestampFromDate(new Date(now.getTime() - 10_000)),
      agentVersion: "v0.14.2",
      placement: { region: "region-1", site: "site-a", cpuModel: "Example CPU 1000", chassis: "generic-1u-2x-u2" },
    },
  };
}

export const unavailable = () => new ConnectError("Prometheus at http://127.0.0.1:9090: connection refused", Code.Unavailable);

/** An API serving MonitoringService, the caller holding what perms names. */
export function monitoringApi(f: {
  perms?: string[];
  overview?: () => GetOverviewResponse;
  alerts?: () => ListAlertsResponse;
  series?: (req: GetSeriesRequest) => GetSeriesResponse;
  /** The Nodes NodeService lists; one Ready "dev" unless said. */
  nodes?: ReturnType<typeof node>[];
  /** The node of each machine ListNodeUsage sums; each holds 2 vCPUs, 4Gi and 20Gi. */
  machinesOn?: string[];
  /** The node of each machine its node has stopped; each holds 20Gi, and takes 4Gi back if started. */
  stoppedOn?: string[];
  /** What TestPermissions answers: as perms names, or never. */
  permissions?: "never";
  /** ListNodes fails, as an apiserver that cannot read them. */
  nodesFail?: boolean;
} = {}) {
  return fakeApi((r) => {
    r.service(AccountService, {
      testPermissions: (req) =>
        f.permissions === "never"
          ? never()
          : { permissions: req.permissions.filter((p) => (f.perms ?? ["platform.monitoring.read", "compute.nodes.list"]).includes(p)) },
    });
    r.service(NodeService, {
      listNodes: () => {
        if (f.nodesFail) throw new ConnectError("the store is unavailable", Code.Unavailable);
        return { items: f.nodes ?? [node("dev")] };
      },
    });
    r.service(MonitoringService, {
      listNodeUsage: () => ({
        nodes: (f.nodes ?? [node("dev")]).map((n) => {
          const count = (f.machinesOn ?? []).filter((on) => on === n.metadata?.name).length;
          const stopped = (f.stoppedOn ?? []).filter((on) => on === n.metadata?.name).length;
          return {
            node: n.metadata?.name ?? "",
            machines: count + stopped,
            cpu: `${2 * count}`,
            memory: `${4 * count}Gi`,
            disk: `${20 * (count + stopped)}Gi`,
            stoppedMachines: stopped,
            stoppedMemory: `${4 * stopped}Gi`,
          };
        }),
      }),
      getOverview: () => (f.overview ?? overview)(),
      listAlerts: () => (f.alerts ?? alerts)(),
      getSeries: (req) => (f.series ?? series)(req),
    });
  });
}
