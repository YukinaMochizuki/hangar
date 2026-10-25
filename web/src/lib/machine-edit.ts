// Editing a container or VM on the new-machine form: the form's fields read
// from the object, the object written back with only what the person changed
// replaced, and what applying it does. The effects are the node's: a field
// that reaches the spec its node boots the machine with (driver.VMSpec,
// built in internal/node/machines.go and vmboot.go) changes that spec's hash,
// and ensureVM stops and starts the machine; a container's image of another
// digest also rebuilds its root disk. The rest reach no VMSpec.

import { clone, create, toJson, type JsonValue } from "@bufbuild/protobuf";
import {
  ContainerSchema,
  ContainerSpecSchema,
  ContainerVolumeSchema,
  EnvVarSchema,
  ResourcesSchema,
  RestartPolicy,
  type Container,
  type ContainerVolume,
} from "@hangar/api/hangar/compute/v1alpha1/container_pb";
import { NetworkInterfaceSchema, type NetworkInterface } from "@hangar/api/hangar/compute/v1alpha1/interface_pb";
import {
  VirtualMachineDiskSchema,
  VirtualMachineFileShareSchema,
  VirtualMachinePlatformSchema,
  VirtualMachineResourcesSchema,
  VirtualMachineSchema,
  VirtualMachineSpecSchema,
  type VirtualMachine,
  type VirtualMachineFileShare,
} from "@hangar/api/hangar/compute/v1alpha1/virtual_machine_pb";
import type { Advanced, Restart } from "@/components/container-advanced";
import type { VolumeRow } from "@/components/volume-picker";
import { formatNumber, quantityCPU, quantityGiB } from "@/lib/quantity";
import { joinWords } from "@/lib/shell-words";

export type MachineObject = Container | VirtualMachine;

/** An interface after the first, on its network in the groups it names. */
export type ExtraInterface = { network: string; groups: string[] };

/** The form's network: the first interface, and those after it. */
export type NetworkFields = { network: string; groups: string[]; extra: ExtraInterface[] };

// What the apiserver gives a machine whose spec leaves its resources out.
export const containerDefaults = { cpu: "1", memory: "512Mi", disk: "10Gi" };
export const vmDefaults = { cpu: "1", memory: "1Gi", disk: "10Gi" };

let nextEnvId = -1;

const restartOf: Record<RestartPolicy, Restart> = {
  [RestartPolicy.UNSPECIFIED]: "always",
  [RestartPolicy.ALWAYS]: "always",
  [RestartPolicy.ON_FAILURE]: "on-failure",
  [RestartPolicy.NEVER]: "no",
};

/** The form's interfaces from a spec's: empty is one interface on default, and no group the default group. */
export function networkFieldsOf(nics: NetworkInterface[]): NetworkFields {
  const groups = (n: NetworkInterface) => (n.securityGroups.length ? [...n.securityGroups] : ["default"]);
  const [first, ...rest] = nics;
  return {
    network: first?.network || "default",
    groups: first ? groups(first) : ["default"],
    extra: rest.map((n) => ({ network: n.network, groups: groups(n) })),
  };
}

/** The interfaces the form's network fields amount to; one on default in its default group is the spec's empty. */
export function interfacesOf(f: NetworkFields): NetworkInterface[] {
  if (f.network === "default" && f.groups.length === 1 && f.groups[0] === "default" && f.extra.length === 0) return [];
  return [
    create(NetworkInterfaceSchema, { network: f.network, securityGroups: f.groups }),
    ...f.extra.map((x) => create(NetworkInterfaceSchema, { network: x.network, securityGroups: x.groups })),
  ];
}

/** A container's process as the Advanced section holds it. */
export function advancedOf(c: Container): Advanced {
  const s = c.spec;
  return {
    entrypoint: joinWords(s?.command ?? []),
    command: joinWords(s?.args ?? []),
    restart: restartOf[s?.restartPolicy ?? RestartPolicy.UNSPECIFIED] ?? "always",
    env: (s?.env ?? []).map((e) => ({ id: nextEnvId--, name: e.name, value: e.value })),
  };
}

/** A container's volumes as the volume picker holds them. */
export function volumeRowsOf(c: Container): VolumeRow[] {
  return (c.spec?.volumes ?? []).map((v) => ({
    source: v.source,
    kind: v.sourceKind === "FileShare" ? "FileShare" : "Disk",
    target: v.target,
    readOnly: v.readOnly,
  }));
}

/** A VM's file shares as the volume picker holds them, with no target. */
export function vmShareRowsOf(v: VirtualMachine): VolumeRow[] {
  return (v.spec?.fileShares ?? []).map((s) => ({ source: s.name, kind: "FileShare", target: "", readOnly: s.readOnly }));
}

/** The file shares a VM's rows amount to. */
export function vmSharesOf(rows: VolumeRow[]): VirtualMachineFileShare[] {
  return rows.map((r) => create(VirtualMachineFileShareSchema, { name: r.source, readOnly: r.readOnly }));
}

/**
 * The volumes the rows amount to on top of the ones a container had: a row
 * of the same source and kind keeps that volume's type and options, which the
 * picker does not show.
 */
export function volumesOf(rows: VolumeRow[], was: ContainerVolume[] = []): ContainerVolume[] {
  const left = [...was];
  return rows.map((r) => {
    const i = left.findIndex((v) => v.source === r.source && (v.sourceKind === "FileShare") === (r.kind === "FileShare"));
    const base = i >= 0 ? left.splice(i, 1)[0] : undefined;
    if (!base) return create(ContainerVolumeSchema, { source: r.source, sourceKind: r.kind === "FileShare" ? "FileShare" : "", target: r.target, readOnly: r.readOnly });
    const v = clone(ContainerVolumeSchema, base);
    v.target = r.target;
    v.readOnly = r.readOnly;
    return v;
  });
}

/** The spec's fields the form shows, each written only when the person changed it. */
export type FormSpec = {
  image?: string;
  command?: string[];
  args?: string[];
  env?: { name: string; value: string }[];
  restartPolicy?: RestartPolicy;
  interfaces?: NetworkInterface[];
  volumes?: VolumeRow[];
  cpu?: string;
  memory?: string;
  disk?: string;
  /** A VM's data disks, by name. */
  disks?: string[];
  /** A VM's file shares. */
  fileShares?: VolumeRow[];
  sshAuthorizedKeys?: string[];
  userData?: string;
  /** A VM's SMBIOS serial number; empty is the platform's. */
  serialNumber?: string;
};

/**
 * The object an edit applies: the object as the page opened on it, at its
 * resource version, with only the fields set in `changed` replaced. Labels,
 * the run state and every field the form does not show stay as they were.
 */
export function editedObject(base: MachineObject, changed: FormSpec): MachineObject {
  if (base.$typeName === ContainerSchema.typeName) {
    const o = clone(ContainerSchema, base as Container);
    const s = (o.spec ??= create(ContainerSpecSchema));
    if (changed.image !== undefined) s.image = changed.image;
    if (changed.command !== undefined) s.command = changed.command;
    if (changed.args !== undefined) s.args = changed.args;
    if (changed.env !== undefined) s.env = changed.env.map((e) => create(EnvVarSchema, e));
    if (changed.restartPolicy !== undefined) s.restartPolicy = changed.restartPolicy;
    if (changed.interfaces !== undefined) s.interfaces = changed.interfaces;
    if (changed.volumes !== undefined) s.volumes = volumesOf(changed.volumes, (base as Container).spec?.volumes);
    if (changed.cpu !== undefined || changed.memory !== undefined || changed.disk !== undefined) {
      s.resources = create(ResourcesSchema, {
        cpu: changed.cpu ?? s.resources?.cpu ?? "",
        memory: changed.memory ?? s.resources?.memory ?? "",
        disk: changed.disk ?? s.resources?.disk ?? "",
      });
    }
    return o;
  }
  const o = clone(VirtualMachineSchema, base as VirtualMachine);
  const s = (o.spec ??= create(VirtualMachineSpecSchema));
  if (changed.interfaces !== undefined) s.interfaces = changed.interfaces;
  if (changed.disks !== undefined) s.disks = changed.disks.map((disk) => create(VirtualMachineDiskSchema, { disk }));
  if (changed.fileShares !== undefined) s.fileShares = vmSharesOf(changed.fileShares);
  if (changed.sshAuthorizedKeys !== undefined) s.sshAuthorizedKeys = changed.sshAuthorizedKeys;
  if (changed.userData !== undefined) s.userData = changed.userData;
  if (changed.serialNumber !== undefined) s.platform = changed.serialNumber ? create(VirtualMachinePlatformSchema, { serialNumber: changed.serialNumber }) : undefined;
  if (changed.cpu !== undefined || changed.memory !== undefined) {
    s.resources = create(VirtualMachineResourcesSchema, {
      cpu: changed.cpu ?? s.resources?.cpu ?? "",
      memory: changed.memory ?? s.resources?.memory ?? "",
    });
  }
  return o;
}

/** What a change does once applied, as the field's comment in the proto says. */
export type Effect = "rebuild" | "restart" | "nothing";

export type Change = { field: string; label: string; detail: string; effect: Effect };

type Json = Record<string, JsonValue>;

function specJson(o: MachineObject): Json {
  return (
    o.$typeName === ContainerSchema.typeName
      ? toJson(ContainerSpecSchema, (o as Container).spec ?? create(ContainerSpecSchema))
      : toJson(VirtualMachineSpecSchema, (o as VirtualMachine).spec ?? create(VirtualMachineSpecSchema))
  ) as Json;
}

const same = (a: JsonValue | undefined, b: JsonValue | undefined) => JSON.stringify(a ?? null) === JSON.stringify(b ?? null);

const words = (v: JsonValue | undefined) => ((v as string[] | undefined)?.length ? joinWords(v as string[]) : "the image's");

function envDetail(a: JsonValue | undefined, b: JsonValue | undefined): string {
  const before = new Map(((a ?? []) as { name: string; value?: string }[]).map((e) => [e.name, e.value ?? ""]));
  const after = new Map(((b ?? []) as { name: string; value?: string }[]).map((e) => [e.name, e.value ?? ""]));
  const out: string[] = [];
  for (const [k, v] of after) if (!before.has(k)) out.push(`${k} added`);
  else if (before.get(k) !== v) out.push(`${k} changed`);
  for (const k of before.keys()) if (!after.has(k)) out.push(`${k} removed`);
  return out.join(", ") || "reordered";
}

function nicsText(v: JsonValue | undefined): string {
  const nics = (v ?? []) as { network: string; securityGroups?: string[] }[];
  if (nics.length === 0) return "default";
  return nics.map((n) => `${n.network} (${(n.securityGroups ?? []).join(", ") || "default"})`).join(", ");
}

function listDetail(a: JsonValue | undefined, b: JsonValue | undefined, key: (x: Json) => string): string {
  const before = ((a ?? []) as Json[]).map((x) => [key(x), JSON.stringify(x)] as const);
  const after = ((b ?? []) as Json[]).map((x) => [key(x), JSON.stringify(x)] as const);
  const bm = new Map(before);
  const am = new Map(after);
  const out: string[] = [];
  for (const [k, v] of after) if (!bm.has(k)) out.push(`${k} added`);
  else if (bm.get(k) !== v) out.push(`${k} changed`);
  for (const [k] of before) if (!am.has(k)) out.push(`${k} removed`);
  return out.join(", ") || "reordered";
}

const restartText = (v: JsonValue | undefined) =>
  ({ RESTART_POLICY_ON_FAILURE: "on-failure", RESTART_POLICY_NEVER: "no" } as Record<string, string>)[String(v)] ?? "always";

function sizeText(cpu: string, memory: string): string {
  return `${formatNumber(quantityCPU(cpu))} vCPU · ${formatNumber(quantityGiB(memory))} GiB`;
}

/**
 * What applying `next` over `base` changes, field by field, each with what
 * it does: a container's image rebuilds its root disk; its interfaces,
 * volumes, entrypoint, command, environment, restart policy, cpu, memory and
 * disk reach its VMSpec and restart it; image pull secrets are read only at
 * a pull and restart nothing. A VM's interfaces, cpu, memory and serial
 * number restart it; its file shares (its shares' exports) and user data
 * (the metadata service, cloud-init's first boot) restart nothing.
 */
export function changesOf(base: MachineObject, next: MachineObject): Change[] {
  const a = specJson(base);
  const b = specJson(next);
  const out: Change[] = [];
  const add = (field: string, label: string, effect: Effect, detail: string) => {
    if (!same(a[field], b[field])) out.push({ field, label, effect, detail });
  };
  if (base.$typeName === ContainerSchema.typeName) {
    const d = containerDefaults;
    add("image", "Image", "rebuild", `${a.image ?? ""} → ${b.image ?? ""}`);
    add("interfaces", "Interfaces", "restart", `${nicsText(a.interfaces)} → ${nicsText(b.interfaces)}`);
    add("volumes", "Volumes", "restart", listDetail(a.volumes, b.volumes, (v) => String(v.target ?? "")));
    add("command", "Entrypoint", "restart", `${words(a.command)} → ${words(b.command)}`);
    add("args", "Command", "restart", `${words(a.args)} → ${words(b.args)}`);
    add("env", "Environment", "restart", envDetail(a.env, b.env));
    add("restartPolicy", "Restart", "restart", `${restartText(a.restartPolicy)} → ${restartText(b.restartPolicy)}`);
    const ra = (a.resources ?? {}) as Json;
    const rb = (b.resources ?? {}) as Json;
    const cpuA = String(ra.cpu || d.cpu), memA = String(ra.memory || d.memory), cpuB = String(rb.cpu || d.cpu), memB = String(rb.memory || d.memory);
    if (!same(ra.cpu, rb.cpu) || !same(ra.memory, rb.memory)) out.push({ field: "size", label: "Size", effect: "restart", detail: `${sizeText(cpuA, memA)} → ${sizeText(cpuB, memB)}` });
    if (!same(ra.disk, rb.disk)) {
      out.push({ field: "disk", label: "Disk", effect: "restart", detail: `${formatNumber(quantityGiB(String(ra.disk || d.disk)))} → ${formatNumber(quantityGiB(String(rb.disk || d.disk)))} GiB` });
    }
    add("imagePullSecrets", "Image pull secrets", "nothing", `${((a.imagePullSecrets ?? []) as string[]).join(", ") || "none"} → ${((b.imagePullSecrets ?? []) as string[]).join(", ") || "none"}`);
  } else {
    const d = vmDefaults;
    const ra = (a.resources ?? {}) as Json;
    const rb = (b.resources ?? {}) as Json;
    if (!same(ra, rb)) {
      out.push({ field: "size", label: "Size", effect: "restart", detail: `${sizeText(String(ra.cpu || d.cpu), String(ra.memory || d.memory))} → ${sizeText(String(rb.cpu || d.cpu), String(rb.memory || d.memory))}` });
    }
    add("interfaces", "Interfaces", "restart", `${nicsText(a.interfaces)} → ${nicsText(b.interfaces)}`);
    add("disks", "Data disks", "restart", listDetail(a.disks, b.disks, (d) => String(d.disk ?? "")));
    add("fileShares", "File shares", "nothing", listDetail(a.fileShares, b.fileShares, (s) => String(s.name ?? "")));
    add("sshAuthorizedKeys", "SSH keys", "nothing", "changed; cloud-init reads them on the first boot only");
    add("userData", "User data", "nothing", "changed; cloud-init runs it on the first boot only");
    const serial = (v: JsonValue | undefined) => String((v as Json | undefined)?.serialNumber ?? "") || "the platform's";
    add("platform", "Serial number", "restart", `${serial(a.platform)} → ${serial(b.platform)}`);
  }
  return out;
}

/** How much more of each limit an edit takes: the new resources less the old, never the machine's whole size. */
export function quotaDelta(base: MachineObject, next: MachineObject): { cpu: number; memoryGi: number; diskGi: number } {
  const r = (o: MachineObject) => {
    if (o.$typeName === ContainerSchema.typeName) {
      const x = (o as Container).spec?.resources;
      const d = containerDefaults;
      return { cpu: quantityCPU(x?.cpu || d.cpu), memoryGi: quantityGiB(x?.memory || d.memory), diskGi: quantityGiB(x?.disk || d.disk) };
    }
    const x = (o as VirtualMachine).spec?.resources;
    return { cpu: quantityCPU(x?.cpu || vmDefaults.cpu), memoryGi: quantityGiB(x?.memory || vmDefaults.memory), diskGi: 0 };
  };
  const a = r(base);
  const b = r(next);
  return { cpu: b.cpu - a.cpu, memoryGi: b.memoryGi - a.memoryGi, diskGi: b.diskGi - a.diskGi };
}
