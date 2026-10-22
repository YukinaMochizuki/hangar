// Port forwards as the console shows them: one public port on the
// platform's shared IPv4 address, drawn at random by the apiserver and kept
// until the object is deleted, forwarded to a port of one interface of a
// container or virtual machine. Inbound traffic still passes that
// interface's security groups, so a forward no group rule lets in reaches
// nothing; inboundOf says which case a forward is in.
//
// The API kind does not exist yet. PortForward below is the agreed shape,
// and the hooks at the end of this file read port-forwards.fixture.ts: they
// are the one seam where the generated client replaces the fixture.

import { ConditionStatus, type Condition } from "@hangar/api/hangar/meta/v1/meta_pb";
import { SecurityGroupProtocol, type IngressRule, type SecurityGroup } from "@hangar/api/hangar/network/v1alpha1/security_group_pb";
import type { JsonValue } from "@bufbuild/protobuf";
import { toYaml } from "@/lib/manifest";
import type { MachineKind } from "@/lib/machines";
import type { Attachment, ListState } from "@/lib/network";
import { fixtureCan, fixtureForwards, fixtureQuota, fixtureTargets } from "@/lib/port-forwards.fixture";

export type ForwardProtocol = "TCP" | "UDP";

/** The interface a forward sends to; iface is eth0 when the API leaves it unset. */
export type ForwardTarget = { kind: MachineKind; name: string; iface: string };

/** A PortForward as the agreed API shape has it. */
export type PortForward = {
  metadata: { name: string; project: string; uid: string; labels: Record<string, string>; createTime?: Date; deleteTime?: Date };
  spec: { target: ForwardTarget; protocol: ForwardProtocol; targetPort: number };
  /** ipv4 and port are empty until the apiserver has drawn the port. */
  status: { ipv4: string; port: number; machine: string; conditions: Condition[] };
};

/** The address a forward is reached at, such as 203.0.113.20:23817; undefined until its port is drawn. */
export function publicAddress(pf: PortForward): string | undefined {
  return pf.status.ipv4 && pf.status.port ? `${pf.status.ipv4}:${pf.status.port}` : undefined;
}

/** "22/tcp", "19132/udp". */
export const portLabel = (port: number, protocol: ForwardProtocol) => `${port}/${protocol.toLowerCase()}`;

export type ForwardState = {
  label: string;
  light: "ok" | "pending" | "off";
  pulse?: boolean;
  /** True while the apiserver has not drawn the public port. */
  unassigned?: boolean;
  reason?: string;
  message?: string;
};

/** Where a forward stands: its Ready condition, and whether its port is drawn yet. */
export function stateOf(pf: PortForward): ForwardState {
  if (pf.metadata.deleteTime) return { label: "Deleting", light: "off" };
  const ready = pf.status.conditions.find((c) => c.type === "Ready");
  if (!publicAddress(pf)) {
    return { label: "Pending", light: "pending", pulse: true, unassigned: true, reason: ready?.reason, message: ready?.message };
  }
  if (ready?.status === ConditionStatus.TRUE) return { label: "Ready", light: "ok" };
  return { label: "Waiting", light: "pending", reason: ready?.reason, message: ready?.message };
}

/** A command that reaches the forward, and what it is. */
export type Example = { label: string; command: string };

/**
 * How to reach a forward from outside: ssh for TCP port 22, nc for UDP,
 * and otherwise the address itself. user stands in for the login, which
 * the console does not know.
 */
export function examplesOf(pf: PortForward, user = "user"): Example[] {
  const addr = publicAddress(pf);
  if (!addr) return [];
  const { ipv4, port } = pf.status;
  if (pf.spec.protocol === "TCP" && pf.spec.targetPort === 22) return [{ label: "SSH", command: `ssh -p ${port} ${user}@${ipv4}` }];
  if (pf.spec.protocol === "UDP") return [{ label: "UDP, with netcat", command: `nc -u ${ipv4} ${port}` }];
  return [{ label: "Address", command: addr }];
}

/** A rule of one of the interface's groups that lets the forward's traffic in. */
export type Admitting = { group: string; rule: IngressRule; from: string };

/**
 * Whether the target interface's security groups let the forward's traffic
 * in from the internet over IPv4: from any address (open), from some ranges
 * only (some), or not at all (none). The built-in default group lets in only
 * its own members, so it never admits a forward; a group the project lacks
 * lets nothing in.
 */
export type Inbound = {
  verdict: "open" | "some" | "none";
  /** False when no machine of the project has the interface yet, so its groups are not known. */
  known: boolean;
  groups: string[];
  admitting: Admitting[];
};

const v4 = /^\d{1,3}(\.\d{1,3}){3}\/\d{1,2}$/;

/** Whether ports as a rule writes them ("", "22", "80,443", "8000-8080") hold port. */
export function portsHold(ports: string, port: number): boolean {
  const p = ports.replace(/\s+/g, "");
  if (!p) return true;
  return p.split(",").some((part) => {
    const [lo, hi = lo] = part.split("-").map(Number);
    return port >= lo! && port <= hi!;
  });
}

const protocolOf: Record<ForwardProtocol, SecurityGroupProtocol> = { TCP: SecurityGroupProtocol.TCP, UDP: SecurityGroupProtocol.UDP };

export function inboundOf(target: ForwardTarget, protocol: ForwardProtocol, port: number, attachments: Attachment[], groups: SecurityGroup[]): Inbound {
  const att = attachments.find((a) => a.kind === target.kind && a.name === target.name && a.iface === target.iface);
  const names = att?.securityGroups ?? [];
  const admitting: Admitting[] = [];
  for (const name of names) {
    const g = groups.find((x) => x.metadata?.name === name && x.spec?.network === att?.network);
    for (const rule of g?.spec?.ingress ?? []) {
      if (rule.protocol !== SecurityGroupProtocol.ALL && rule.protocol !== protocolOf[protocol]) continue;
      if (!portsHold(rule.ports, port)) continue;
      const peer = rule.from?.peer;
      if (peer?.case !== "cidr" || !v4.test(peer.value)) continue;
      admitting.push({ group: name, rule, from: peer.value });
    }
  }
  const verdict = admitting.some((a) => a.from === "0.0.0.0/0") ? "open" : admitting.length > 0 ? "some" : "none";
  return { verdict, known: !!att, groups: names, admitting };
}

/** portProblem says why a target port as typed is not one, or undefined when it is. */
export function portProblem(port: string): string | undefined {
  const p = port.trim();
  if (!p) return "The target port is empty.";
  const n = Number(p);
  if (!/^\d+$/.test(p) || n < 1 || n > 65535) return `${p} is not a port: a whole number from 1 to 65535.`;
  return undefined;
}

/** The name the create form suggests: the machine and the port, "game-19132-udp". */
export const suggestName = (machine: string, protocol: ForwardProtocol, port: number) =>
  machine && port ? `${machine}-${port}${protocol === "UDP" ? "-udp" : ""}` : "";

/** What a person writes of a forward, as `hangar apply -f` takes it. */
export function portForwardYaml(pf: Pick<PortForward, "spec"> & { metadata: { name: string; labels?: Record<string, string> } }): string {
  const t = pf.spec.target;
  const metadata: { [k: string]: JsonValue } = { name: pf.metadata.name };
  const labels = Object.entries(pf.metadata.labels ?? {}).filter(([k]) => !/\.hangarlab\.cloud\//.test(k));
  if (labels.length) metadata.labels = Object.fromEntries(labels);
  const target: Record<string, string> = { [t.kind === "Container" ? "container" : "virtualMachine"]: t.name };
  if (t.iface && t.iface !== "eth0") target.interface = t.iface;
  return toYaml({
    apiVersion: "network.hangarlab.cloud/v1alpha1",
    kind: "PortForward",
    metadata,
    spec: { target, protocol: pf.spec.protocol, targetPort: pf.spec.targetPort },
  });
}

// The seam. Each hook reads the fixture today; wiring the API replaces its
// body with the generated client (a list, the project's quota, and
// useAttachments with useSecurityGroups) and leaves its callers as they are.

/** The project's port forwards, by name. */
export function usePortForwards(project: string): ListState<PortForward> {
  return fixtureForwards(project);
}

/** How many port forwards the project holds, against its plan's portForwards limit (undefined: no limit). */
export function usePortForwardQuota(project: string): { used: number; limit?: number; loading: boolean } {
  return fixtureQuota(project);
}

/** The project's interfaces and security groups, which say whether a forward's traffic gets in. */
export function useForwardTargets(project: string): { attachments: ListState<Attachment>; groups: ListState<SecurityGroup> } {
  return fixtureTargets(project);
}

/** What the caller may do to port forwards in the project, as useNetworkCan answers for the other kinds. */
export function usePortForwardCan(project: string): { can: (permission: string) => boolean } {
  return fixtureCan(project);
}

export const portForwardPerms = { apply: "network.portforwards.apply", delete: "network.portforwards.delete" } as const;
