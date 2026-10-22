// The platform's Nodes, for its administrators and sponsors: what each host
// is and how it stands, from NodeService, and the machines placed on it,
// counted, never named, from MonitoringService.ListNodeUsage
// (docs/decisions/monitoring.md, "No tenant in a label"). Read only:
// Node is system-managed, written by the apiserver alone
// (docs/decisions/apply-semantics.md).

import { useQuery } from "@connectrpc/connect-query";
import { skipToken } from "@tanstack/react-query";
import { NodeService, type Node } from "@hangar/api/hangar/compute/v1alpha1/node_pb";
import { MonitoringService } from "@hangar/api/hangar/monitoring/v1alpha1/monitoring_pb";
import { ConditionStatus, type Condition } from "@hangar/api/hangar/meta/v1/meta_pb";
import { date, refetchInterval } from "@/lib/machines";
import { placementOf, type HostInfo } from "@/lib/placement";
import { formatNumber, quantityCPU, quantityGiB } from "@/lib/quantity";

/** What the Nodes pages read at the platform: the Nodes. What is placed on them is the monitoring's. */
export const nodePerms = { list: "compute.nodes.list" } as const;

export type Readiness = "ready" | "notReady" | "unknown";

export type Resources = { cpu: string; memory: string; disk: string };

/** What is placed on a node, summed, as the scheduler counts it: vCPUs and GiB. */
export type Held = { cpu: number; memory: number; disk: number };

/**
 * One resource of a host, in vCPUs or GiB, as the scheduler counts it
 * (internal/apiserver/resources.go): all the host has; what it keeps for
 * itself, the part of that not allocatable; what is placed on it holds, as
 * ListNodeUsage answers it (the machines' spec.resources, and the disks and
 * file shares there); and what is left for another machine, the
 * allocatable less what is held. vCPUs are shared, never reserved
 * (docs/decisions/machine-model.md), so the machines' vCPUs are what is
 * assigned and may pass what is allocatable. total is NaN when the node
 * reports none; held is undefined until the machines are known.
 */
export type Capacity = { kind: "cpu" | "memory" | "disk"; total: number; reserved: number; held?: number; free?: number };

function capacity(kind: Capacity["kind"], total: number, allocatable: number, held: number | undefined): Capacity {
  // A node that reports no allocatable amount offers all it has, as the scheduler takes it.
  const room = Number.isFinite(allocatable) ? allocatable : total;
  return { kind, total, reserved: Math.max(total - room, 0), held, free: held === undefined ? undefined : Math.max(room - held, 0) };
}

/** A node's three resources, in the order they are shown. */
export function capacities(n: NodeView, held: Held | undefined): Capacity[] {
  return [
    capacity("cpu", quantityCPU(n.capacity.cpu), quantityCPU(n.allocatable.cpu), held?.cpu),
    capacity("memory", quantityGiB(n.capacity.memory), quantityGiB(n.allocatable.memory), held?.memory),
    capacity("disk", quantityGiB(n.capacity.disk), quantityGiB(n.allocatable.disk), held?.disk),
  ];
}

/** An amount as people read it: "32 vCPU", "128 GiB", "1.8 TiB". */
export function formatAmount(kind: Capacity["kind"], v: number): string {
  if (!Number.isFinite(v)) return "–";
  if (kind === "cpu") return `${formatNumber(v)} vCPU`;
  return v >= 1024 ? `${formatNumber(Math.round((v / 1024) * 10) / 10)} TiB` : `${formatNumber(v)} GiB`;
}

export type NodeView = {
  name: string;
  readiness: Readiness;
  /** The Ready condition's reason, when it is not True. */
  reason: string;
  cordoned: boolean;
  capacity: Resources;
  allocatable: Resources;
  agentVersion: string;
  heartbeat?: Date;
  ipv6Prefix: string;
  host?: HostInfo;
  conditions: Condition[];
};

const resources = (r?: { cpu: string; memory: string; disk: string }): Resources => ({ cpu: r?.cpu ?? "", memory: r?.memory ?? "", disk: r?.disk ?? "" });

export function toNodeView(n: Node): NodeView {
  const name = n.metadata?.name ?? "";
  const conditions = n.status?.conditions ?? [];
  const ready = conditions.find((c) => c.type === "Ready");
  const readiness: Readiness = ready?.status === ConditionStatus.TRUE ? "ready" : ready?.status === ConditionStatus.FALSE ? "notReady" : "unknown";
  return {
    name,
    readiness,
    reason: readiness === "ready" ? "" : (ready?.reason ?? ""),
    cordoned: n.spec?.unschedulable ?? false,
    capacity: resources(n.status?.capacity),
    allocatable: resources(n.status?.allocatable),
    agentVersion: n.status?.agentVersion ?? "",
    heartbeat: date(n.status?.lastHeartbeatTime),
    ipv6Prefix: n.status?.ipv6Prefix ?? "",
    host: placementOf(n.status?.placement, name),
    conditions,
  };
}

/** Every Node, by name; nothing is asked while not enabled. */
export function useNodes(enabled = true) {
  const q = useQuery(NodeService.method.listNodes, enabled ? { pageSize: 1000 } : skipToken, { refetchInterval });
  const items = (q.data?.items ?? []).map(toNodeView).sort((a, b) => a.name.localeCompare(b.name));
  return { items, loading: q.isPending && enabled, error: q.error, refetch: () => void q.refetch() };
}

/**
 * How many machines each node has across every project, and what is held
 * there, as the apiserver sums them (MonitoringService.ListNodeUsage):
 * counts and sums alone, never which machines. Asked as often as the
 * monitoring overview, not as a list page.
 */
export function useMachinesPerNode(enabled = true) {
  const q = useQuery(MonitoringService.method.listNodeUsage, enabled ? {} : skipToken, { refetchInterval: 30_000 });
  const num = (v: number) => (Number.isFinite(v) ? v : 0);
  const per = new Map<string, { count: number; held: Held }>();
  for (const u of q.data?.nodes ?? []) {
    per.set(u.node, { count: u.machines, held: { cpu: num(quantityCPU(u.cpu)), memory: num(quantityGiB(u.memory)), disk: num(quantityGiB(u.disk)) } });
  }
  const known = q.data !== undefined;
  return {
    count: (node: string) => (known ? (per.get(node)?.count ?? 0) : undefined),
    held: (node: string): Held | undefined => (known ? (per.get(node)?.held ?? { cpu: 0, memory: 0, disk: 0 }) : undefined),
    error: q.error,
  };
}
