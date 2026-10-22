// Port forwards as the console shows them (docs/decisions/networking.md,
// "Port forwards"): one public port, drawn at random from 10000 to 32767
// when the PortForward is made and kept until it is deleted, open at the
// shared IPv4 address of the node the target runs on and leading to a port
// of one interface of a container or virtual machine. Inbound traffic still
// passes that interface's security groups, so a forward no group rule lets
// in reaches nothing; inboundOf says which case a forward is in.

import { useQuery } from "@connectrpc/connect-query";
import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import { Protocol, type PortForward } from "@hangar/api/hangar/network/v1alpha1/port_forward_pb";
import { SecurityGroupProtocol, type IngressRule, type SecurityGroup } from "@hangar/api/hangar/network/v1alpha1/security_group_pb";
import { ProjectService } from "@hangar/api/hangar/tenancy/v1alpha1/project_pb";
import type { MachineKind } from "@/lib/machines";
import { targetOf, useAttachments, useSecurityGroups, type Attachment, type ListState } from "@/lib/network";

export type ForwardProtocol = "TCP" | "UDP";

/** The ports of a node's shared IPv4 address forwards are drawn from; the machines' own connections leave from those above. */
export const publicPorts = { min: 10000, max: 32767 } as const;

/** The interface a forward leads to; iface is eth0 when the API leaves it unset. */
export type ForwardTarget = { kind: MachineKind; name: string; iface: string };

/** The interface a forward names, or undefined when it names none. */
export function forwardTargetOf(pf: PortForward): ForwardTarget | undefined {
  const t = targetOf(pf.spec?.target);
  return t && { ...t, iface: pf.spec?.target?.interface || "eth0" };
}

/** The API's protocol in the console's words; UDP only when it says UDP. */
export const protocolOf = (p: Protocol | undefined): ForwardProtocol => (p === Protocol.UDP ? "UDP" : "TCP");
export const protocolEnum = (p: ForwardProtocol): Protocol => (p === "UDP" ? Protocol.UDP : Protocol.TCP);

/** A forward's spec in the console's words, for the pieces that show where it leads. */
export function specOf(pf: PortForward): { target?: ForwardTarget; protocol: ForwardProtocol; targetPort: number } {
  return { target: forwardTargetOf(pf), protocol: protocolOf(pf.spec?.protocol), targetPort: pf.spec?.targetPort ?? 0 };
}

/** The address a forward is reached at, such as 198.51.100.3:23817; undefined until it has both. */
export function publicAddress(pf: PortForward): string | undefined {
  const s = pf.status;
  return s?.ipv4 && s.port ? `${s.ipv4}:${s.port}` : undefined;
}

/** "22/tcp", "19132/udp". */
export const portLabel = (port: number, protocol: ForwardProtocol) => `${port}/${protocol.toLowerCase()}`;

export type ForwardState = {
  label: string;
  light: "ok" | "pending" | "off";
  pulse?: boolean;
  reason?: string;
  /** What the API says, or the console's sentence for a reason it explains. */
  message?: string;
};

/**
 * Where a forward stands, from its Ready condition (the apiserver's
 * Forwarding, TargetNotFound, InterfaceNotFound, PortPending and
 * NoSharedIPv4): Ready, Pending while the target is on its way to a node
 * or no condition is written yet, Waiting while a target, an interface or
 * the node's shared address is missing, and Deleting until the node that
 * held the port has closed it.
 */
export function stateOf(pf: PortForward): ForwardState {
  const node = pf.status?.node;
  if (pf.metadata?.deleteTime) {
    return {
      label: "Deleting",
      light: "off",
      message: node ? `Its port is released once node ${node} has closed it.` : "Its port is released.",
    };
  }
  const ready = pf.status?.conditions.find((c) => c.type === "Ready");
  if (!ready) return { label: "Pending", light: "pending", pulse: true };
  if (ready.status === ConditionStatus.TRUE) return { label: "Ready", light: "ok" };
  const base = { reason: ready.reason, message: ready.message };
  switch (ready.reason) {
    case "PortPending":
      return { label: "Pending", light: "pending", pulse: true, ...base };
    case "NoSharedIPv4":
      return {
        label: "Waiting",
        light: "pending",
        ...base,
        message: `Node ${node || "of the target"} has no shared IPv4 address yet, so the port is open nowhere. It opens once the platform gives that node one.`,
      };
    default:
      return { label: "Waiting", light: "pending", ...base };
  }
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
  const { ipv4, port } = pf.status!;
  const { protocol, targetPort } = specOf(pf);
  if (protocol === "TCP" && targetPort === 22) return [{ label: "SSH", command: `ssh -p ${port} ${user}@${ipv4}` }];
  if (protocol === "UDP") return [{ label: "UDP, with netcat", command: `nc -u ${ipv4} ${port}` }];
  return [{ label: "Address", command: addr }];
}

/** A rule of one of the interface's groups that lets the forward's traffic in. */
export type Admitting = { group: string; rule: IngressRule; from: string };

/**
 * Whether the target interface's security groups let the forward's traffic
 * in from the internet over IPv4: the client's own address reaches the
 * machine, so a rule from any address (open), from some ranges only
 * (some), or none (none). The built-in default group lets in only its own
 * members, so it never admits a forward; a group the project lacks lets
 * nothing in.
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

const groupProtocol: Record<ForwardProtocol, SecurityGroupProtocol> = { TCP: SecurityGroupProtocol.TCP, UDP: SecurityGroupProtocol.UDP };

export function inboundOf(target: ForwardTarget, protocol: ForwardProtocol, port: number, attachments: Attachment[], groups: SecurityGroup[]): Inbound {
  const att = attachments.find((a) => a.kind === target.kind && a.name === target.name && a.iface === target.iface);
  const names = att?.securityGroups ?? [];
  const admitting: Admitting[] = [];
  for (const name of names) {
    const g = groups.find((x) => x.metadata?.name === name && x.spec?.network === att?.network);
    for (const rule of g?.spec?.ingress ?? []) {
      if (rule.protocol !== SecurityGroupProtocol.ALL && rule.protocol !== groupProtocol[protocol]) continue;
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

/**
 * How many port forwards the project holds against its plan's limit, from
 * the project's status as the new-machine form reads it (lib/over-quota.ts);
 * limit is undefined when there is none, or while it is not known.
 */
export function usePortForwardQuota(project: string): { used: number; limit?: number; loading: boolean } {
  const q = useQuery(ProjectService.method.getProject, { name: project }, { retry: false });
  const st = q.data?.project?.status;
  return { used: st?.usage?.portForwards ?? 0, limit: st?.limits?.portForwards, loading: q.isPending };
}

/** The project's interfaces and security groups, which say whether a forward's traffic gets in. */
export function useForwardTargets(project: string): { attachments: ListState<Attachment>; groups: ListState<SecurityGroup> } {
  return { attachments: useAttachments(project), groups: useSecurityGroups(project) };
}
