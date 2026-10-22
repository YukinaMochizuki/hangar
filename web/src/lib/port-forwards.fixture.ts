// Placeholder data for the port forward screens, until the PortForward kind
// and its client exist. Only the hooks in port-forwards.ts read it; wiring
// the API deletes this file and replaces their bodies.
//
// ?fixture= in the page's address picks a state to look at: empty, loading,
// error or quota (the plan's limit reached). Anything else is the full list.

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { ConditionSchema, ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import { SecurityGroupProtocol, SecurityGroupSchema, type SecurityGroup } from "@hangar/api/hangar/network/v1alpha1/security_group_pb";
import type { Attachment, ListState } from "@/lib/network";
import type { ForwardProtocol, ForwardTarget, PortForward } from "@/lib/port-forwards";

type Scenario = "full" | "empty" | "loading" | "error" | "quota";

function scenario(): Scenario {
  if (typeof window === "undefined") return "full";
  const s = new URLSearchParams(window.location.search).get("fixture");
  return s === "empty" || s === "loading" || s === "error" || s === "quota" ? s : "full";
}

const shared = "203.0.113.20";
const day = 86_400_000;
const now = Date.now();

const ready = (ago: number) => create(ConditionSchema, { type: "Ready", status: ConditionStatus.TRUE, reason: "Forwarding", lastTransitionTime: timestampFromDate(new Date(now - ago)) });
const notReady = (reason: string, message: string, status = ConditionStatus.FALSE) =>
  create(ConditionSchema, { type: "Ready", status, reason, message, lastTransitionTime: timestampFromDate(new Date(now - 40_000)) });

function forward(
  project: string,
  name: string,
  target: ForwardTarget,
  protocol: ForwardProtocol,
  targetPort: number,
  port: number,
  ageDays: number,
  conditions = [ready(ageDays * day)],
): PortForward {
  return {
    metadata: { name, project, uid: `7f3c${port.toString(16).padStart(4, "0")}-1d2e-4b8a-9c61-0e5d${name.length}a2b4c6d8`, labels: {}, createTime: new Date(now - ageDays * day) },
    spec: { target, protocol, targetPort },
    status: { ipv4: port ? shared : "", port, machine: port && target.name !== "db" ? `${target.name}-m${port % 97}` : "", conditions },
  };
}

const ct = (name: string, iface = "eth0"): ForwardTarget => ({ kind: "Container", name, iface });
const vm = (name: string, iface = "eth0"): ForwardTarget => ({ kind: "VirtualMachine", name, iface });

const items = (project: string): PortForward[] => [
  // SSH into a VM, let in only from one range.
  forward(project, "builder-22", vm("builder"), "TCP", 22, 31544, 12),
  // A game's TCP port, which its group lets in.
  forward(project, "game-25565", ct("game"), "TCP", 25565, 18466, 3),
  // The same game's UDP port, which no group lets in: the forward exists, and nothing gets through.
  forward(project, "game-19132-udp", ct("game"), "UDP", 19132, 29405, 3),
  // A machine that does not exist yet.
  forward(project, "db-5432", ct("db"), "TCP", 5432, 27640, 1, [notReady("TargetNotFound", "Container db does not exist in this project.")]),
  // Just made: the apiserver has not drawn its port yet.
  forward(project, "web-8080", ct("web"), "TCP", 8080, 0, 0, [notReady("PortPending", "The public port has not been drawn yet.", ConditionStatus.UNKNOWN)]),
  forward(project, "web-22", ct("web"), "TCP", 22, 23817, 21),
];

const list = <T>(get: () => T[]): ListState<T> => {
  const s = scenario();
  if (s === "loading") return { items: [], loading: true, error: null, refetch: () => {} };
  if (s === "error") return { items: [], loading: false, error: new Error("unavailable: the apiserver did not answer"), refetch: () => {} };
  return { items: s === "empty" ? [] : get(), loading: false, error: null, refetch: () => {} };
};

/** Every fixture forward, for the test that holds their ports to the public range. */
export const fixtureItems = items;

export function fixtureForwards(project: string): ListState<PortForward> {
  return list(() => items(project).sort((a, b) => a.metadata.name.localeCompare(b.metadata.name)));
}

export function fixtureQuota(project: string): { used: number; limit?: number; loading: boolean } {
  const used = scenario() === "empty" ? 0 : items(project).length;
  return { used, limit: scenario() === "quota" ? used : 10, loading: false };
}

const attachment = (kind: Attachment["kind"], name: string, iface: string, network: string, ipv4: string, securityGroups: string[]): Attachment => ({
  kind,
  name,
  iface,
  network,
  ipv4,
  ipv6: `2001:db8:4a::${ipv4.split(".").at(-1)}`,
  securityGroups,
});

const attachments: Attachment[] = [
  attachment("Container", "api", "eth0", "default", "10.0.0.11", ["default", "api"]),
  attachment("VirtualMachine", "builder", "eth0", "default", "10.0.0.21", ["default", "ssh-office"]),
  attachment("VirtualMachine", "builder", "eth1", "backend", "10.1.0.21", ["default"]),
  attachment("Container", "game", "eth0", "default", "10.0.0.14", ["default", "game"]),
  attachment("Container", "web", "eth0", "default", "10.0.0.12", ["default", "web"]),
];

const tcp = SecurityGroupProtocol.TCP;
const group = (name: string, ingress: { protocol: SecurityGroupProtocol; ports: string; from: string }[]): SecurityGroup =>
  create(SecurityGroupSchema, {
    metadata: { name },
    spec: { network: "default", ingress: ingress.map((r) => ({ protocol: r.protocol, ports: r.ports, from: { peer: { case: "cidr" as const, value: r.from } } })) },
  });

const groups: SecurityGroup[] = [
  group("api", [{ protocol: tcp, ports: "22", from: "0.0.0.0/0" }]),
  group("game", [{ protocol: tcp, ports: "25565", from: "0.0.0.0/0" }]),
  group("ssh-office", [{ protocol: tcp, ports: "22", from: "198.51.100.0/24" }]),
  group("web", [
    { protocol: tcp, ports: "22", from: "0.0.0.0/0" },
    { protocol: tcp, ports: "80,443", from: "::/0" },
  ]),
];

export function fixtureTargets(project: string): { attachments: ListState<Attachment>; groups: ListState<SecurityGroup> } {
  void project;
  // The machines and groups load whatever ?fixture= says of the forwards.
  const held = <T>(items: T[]): ListState<T> => ({ items, loading: false, error: null, refetch: () => {} });
  return { attachments: held(attachments), groups: held(groups) };
}

export function fixtureCan(project: string): { can: (permission: string) => boolean } {
  void project;
  return { can: () => true };
}
