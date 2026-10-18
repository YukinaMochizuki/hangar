// Containers and VirtualMachines as the console shows them: each joined with
// the Machine it runs as, which is where its phase, node, addresses and boot
// time are (docs/decisions/machine-model.md). A Machine is found by its
// owner reference, never by guessing its name.

import { useQuery } from "@connectrpc/connect-query";
import { timestampDate, type Timestamp } from "@bufbuild/protobuf/wkt";
import {
  ContainerRunState,
  ContainerService,
  type Container,
} from "@hangar/api/hangar/compute/v1alpha1/container_pb";
import {
  MachinePhase,
  MachineService,
  type Machine,
  type MachineInterface,
} from "@hangar/api/hangar/compute/v1alpha1/machine_pb";
import {
  VirtualMachineRunState,
  VirtualMachineService,
  type VirtualMachine,
} from "@hangar/api/hangar/compute/v1alpha1/virtual_machine_pb";
import type { Condition } from "@hangar/api/hangar/meta/v1/meta_pb";
import { quantityCPU, quantityGiB } from "@/lib/quantity";

export type MachineKind = "Container" | "VirtualMachine";

export type MachineView = {
  kind: MachineKind;
  name: string;
  project: string;
  phase: MachinePhase;
  cpu: number;
  memoryGi: number;
  diskGi: number;
  /** A container's image reference, a VM's image or root disk name. */
  image: string;
  imageDigest: string;
  node: string;
  created?: Date;
  booted?: Date;
  deleting: boolean;
  stopped: boolean;
  interfaces: MachineInterface[];
  /** The object's conditions, then the machine's. */
  conditions: Condition[];
  labels: Record<string, string>;
  uid: string;
  resourceVersion: bigint;
  container?: Container;
  vm?: VirtualMachine;
  machine?: Machine;
};

// How often lists are asked again (docs/decisions/console-design.md).
export const refetchInterval = 5000;

// The API's largest page: a project holds far fewer machines than this.
const pageSize = 1000;

export function date(ts: Timestamp | undefined): Date | undefined {
  return ts ? timestampDate(ts) : undefined;
}

function ownerOf(m: Machine): { kind: string; name: string } | undefined {
  const ref = m.metadata?.ownerReferences.find((r) => r.controller) ?? m.metadata?.ownerReferences[0];
  return ref ? { kind: ref.kind, name: ref.name } : undefined;
}

export function containerView(c: Container, m: Machine | undefined): MachineView {
  const res = m?.spec?.resources ?? c.spec?.resources;
  return {
    kind: "Container",
    name: c.metadata?.name ?? "",
    project: c.metadata?.project ?? "",
    phase: m?.status?.phase ?? MachinePhase.UNSPECIFIED,
    cpu: quantityCPU(res?.cpu),
    memoryGi: quantityGiB(res?.memory),
    diskGi: quantityGiB(res?.disk),
    image: c.spec?.image ?? "",
    imageDigest: m?.status?.imageDigest ?? "",
    node: m?.spec?.node ?? "",
    created: date(c.metadata?.createTime),
    booted: date(m?.status?.bootTime),
    deleting: !!c.metadata?.deleteTime,
    stopped: c.spec?.runState === ContainerRunState.STOPPED,
    interfaces: m?.spec?.interfaces ?? [],
    conditions: [...(c.status?.conditions ?? []), ...(m?.status?.conditions ?? [])],
    labels: c.metadata?.labels ?? {},
    uid: c.metadata?.uid ?? "",
    resourceVersion: c.metadata?.resourceVersion ?? 0n,
    container: c,
    machine: m,
  };
}

export function vmView(v: VirtualMachine, m: Machine | undefined): MachineView {
  const root = v.spec?.rootDisk?.source;
  const image = root?.case === "fromImage" ? (root.value.image?.name ?? "") : root?.case === "disk" ? root.value : "";
  const disk = root?.case === "fromImage" ? root.value.size || "10Gi" : "";
  const res = m?.spec?.resources;
  return {
    kind: "VirtualMachine",
    name: v.metadata?.name ?? "",
    project: v.metadata?.project ?? "",
    phase: m?.status?.phase ?? MachinePhase.UNSPECIFIED,
    cpu: quantityCPU(res?.cpu || v.spec?.resources?.cpu || "1"),
    memoryGi: quantityGiB(res?.memory || v.spec?.resources?.memory || "1Gi"),
    diskGi: quantityGiB(disk),
    image,
    imageDigest: "",
    node: m?.spec?.node ?? "",
    created: date(v.metadata?.createTime),
    booted: date(m?.status?.bootTime),
    deleting: !!v.metadata?.deleteTime,
    stopped: v.spec?.runState === VirtualMachineRunState.STOPPED,
    interfaces: m?.spec?.interfaces ?? [],
    conditions: [...(v.status?.conditions ?? []), ...(m?.status?.conditions ?? [])],
    labels: v.metadata?.labels ?? {},
    uid: v.metadata?.uid ?? "",
    resourceVersion: v.metadata?.resourceVersion ?? 0n,
    vm: v,
    machine: m,
  };
}

/** The project's machines of one kind, or of both, with their state. */
export function useMachines(project: string, kind?: MachineKind) {
  const opts = { refetchInterval };
  const containers = useQuery(ContainerService.method.listContainers, { project, pageSize }, { ...opts, enabled: kind !== "VirtualMachine" });
  const vms = useQuery(VirtualMachineService.method.listVirtualMachines, { project, pageSize }, { ...opts, enabled: kind !== "Container" });
  const machines = useQuery(MachineService.method.listMachines, { project, pageSize }, opts);

  const parts = [machines, ...(kind !== "VirtualMachine" ? [containers] : []), ...(kind !== "Container" ? [vms] : [])];
  const error = parts.find((q) => q.error)?.error ?? null;
  const loading = parts.some((q) => q.isPending);

  const byOwner = new Map<string, Machine>();
  for (const m of machines.data?.items ?? []) {
    const o = ownerOf(m);
    if (o) byOwner.set(`${o.kind}/${o.name}`, m);
  }
  const items: MachineView[] = [];
  if (!loading && !error) {
    // A query that is off can still hold data the same query fetched for
    // another component, so the kind is checked here too.
    if (kind !== "VirtualMachine") {
      for (const c of containers.data?.items ?? []) items.push(containerView(c, byOwner.get(`Container/${c.metadata?.name}`)));
    }
    if (kind !== "Container") {
      for (const v of vms.data?.items ?? []) items.push(vmView(v, byOwner.get(`VirtualMachine/${v.metadata?.name}`)));
    }
    items.sort((a, b) => a.name.localeCompare(b.name));
  }
  return { items, loading, error, refetch: () => parts.forEach((q) => void q.refetch()) };
}

/** Whether a machine counts as live: asked to run and not stopped. */
export function isLive(m: MachineView): boolean {
  return !m.stopped && m.phase !== MachinePhase.STOPPED;
}

/** The route a machine's page is at. */
export function machinePath(kind: MachineKind): "/projects/$project/containers/$name" | "/projects/$project/vms/$name" {
  return kind === "Container" ? "/projects/$project/containers/$name" : "/projects/$project/vms/$name";
}

/** How long ago d was, shortly: "12s", "3m", "5h", "2d". */
export function ago(d: Date | undefined, now = Date.now()): string {
  if (!d) return "";
  const s = Math.max(0, Math.round((now - d.getTime()) / 1000));
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h`;
  return `${Math.floor(s / 86400)}d`;
}

/** A duration since d, as "1d 4h" or "35m". */
export function uptime(d: Date | undefined, now = Date.now()): string {
  if (!d) return "";
  const m = Math.max(0, Math.floor((now - d.getTime()) / 60000));
  const days = Math.floor(m / 1440);
  const hours = Math.floor((m % 1440) / 60);
  if (days > 0) return `${days}d ${hours}h`;
  if (hours > 0) return `${hours}h ${m % 60}m`;
  return `${m}m`;
}
