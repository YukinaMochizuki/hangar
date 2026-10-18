// A security group's rules as the console edits and states them: each rule
// as a draft whose peer is chosen by kind (any IPv4, any IPv6, a range, a
// group), read back from and written to the API's rules, and said as a
// sentence. How groups behave is docs/decisions/networking.md.

import { create } from "@bufbuild/protobuf";
import {
  EgressRuleSchema,
  IngressRuleSchema,
  SecurityGroupProtocol,
  type EgressRule,
  type IngressRule,
  type SecurityGroup,
  type SecurityGroupPeer,
} from "@hangar/api/hangar/network/v1alpha1/security_group_pb";
import type { Attachment } from "@/lib/network";

export type Direction = "in" | "out";

/** The kinds of peer a rule names; any IPv4 and any IPv6 are the ranges 0.0.0.0/0 and ::/0. */
export type PeerKind = "any4" | "any6" | "cidr" | "group";

/** A rule being edited. value is the range for cidr and the group's name for group. */
export type Draft = { protocol: SecurityGroupProtocol; ports: string; peer: PeerKind; value: string };

export type Rule = IngressRule | EgressRule;

const any4 = "0.0.0.0/0";
const any6 = "::/0";

/** Only TCP and UDP take ports. */
export const takesPorts = (p: SecurityGroupProtocol) => p === SecurityGroupProtocol.TCP || p === SecurityGroupProtocol.UDP;

export const protocols = [SecurityGroupProtocol.TCP, SecurityGroupProtocol.UDP, SecurityGroupProtocol.ICMP, SecurityGroupProtocol.ALL];

export const peerOf = (r: Rule): SecurityGroupPeer | undefined => (r.$typeName === "hangar.network.v1alpha1.IngressRule" ? r.from : r.to);

/** draftOf is a rule of the API as the editor shows it. */
export function draftOf(r: Rule): Draft {
  const p = peerOf(r)?.peer;
  const ports = r.ports;
  if (p?.case === "group") return { protocol: r.protocol, ports, peer: "group", value: p.value };
  if (p?.case === "cidr" && p.value === any4) return { protocol: r.protocol, ports, peer: "any4", value: "" };
  if (p?.case === "cidr" && p.value === any6) return { protocol: r.protocol, ports, peer: "any6", value: "" };
  return { protocol: r.protocol, ports, peer: "cidr", value: p?.case === "cidr" ? p.value : "" };
}

function peerFrom(d: Draft): SecurityGroupPeer["peer"] {
  switch (d.peer) {
    case "any4":
      return { case: "cidr", value: any4 };
    case "any6":
      return { case: "cidr", value: any6 };
    case "cidr":
      return { case: "cidr", value: d.value.trim() };
    case "group":
      return { case: "group", value: d.value };
  }
}

const portsOf = (d: Draft) => (takesPorts(d.protocol) ? d.ports.replace(/\s+/g, "") : "");

/** The API's ingress rules for the drafts. */
export const ingressOf = (ds: Draft[]) => ds.map((d) => create(IngressRuleSchema, { protocol: d.protocol, ports: portsOf(d), from: { peer: peerFrom(d) } }));

/** The API's egress rules for the drafts. */
export const egressOf = (ds: Draft[]) => ds.map((d) => create(EgressRuleSchema, { protocol: d.protocol, ports: portsOf(d), to: { peer: peerFrom(d) } }));

function portsText(ports: string): string {
  const p = ports.replace(/\s+/g, "");
  if (!p) return "on every port";
  const parts = p.split(",").map((x) => x.replace("-", "–"));
  if (parts.length === 1 && !parts[0]!.includes("–")) return `port ${parts[0]}`;
  if (parts.length === 1) return `ports ${parts[0]}`;
  return `ports ${parts.slice(0, -1).join(", ")} and ${parts.at(-1)}`;
}

function whatText(d: Pick<Draft, "protocol" | "ports">): string {
  switch (d.protocol) {
    case SecurityGroupProtocol.TCP:
      return `TCP ${portsText(d.ports)}`;
    case SecurityGroupProtocol.UDP:
      return `UDP ${portsText(d.ports)}`;
    case SecurityGroupProtocol.ICMP:
      return "ICMP";
    default:
      return "everything";
  }
}

function peerText(d: Pick<Draft, "peer" | "value">): string {
  switch (d.peer) {
    case "any4":
      return "any IPv4 address";
    case "any6":
      return "any IPv6 address";
    case "cidr":
      return d.value.trim() || "a range not given yet";
    case "group":
      return d.value ? `members of ${d.value}` : "a group not chosen yet";
  }
}

/** sentence says what a rule lets through, such as "Lets in TCP port 443 from any IPv6 address". */
export function sentence(d: Draft, dir: Direction): string {
  return dir === "in" ? `Lets in ${whatText(d)} from ${peerText(d)}` : `Lets out ${whatText(d)} to ${peerText(d)}`;
}

/** ruleSentence is sentence for a rule of the API. */
export const ruleSentence = (r: Rule, dir: Direction) => sentence(draftOf(r), dir);

const portRe = /^(\d{1,5})(-(\d{1,5}))?$/;

/** portsProblem says why ports as written are not ports, or undefined when they are. */
export function portsProblem(ports: string): string | undefined {
  const p = ports.replace(/\s+/g, "");
  if (!p) return undefined;
  for (const part of p.split(",")) {
    const m = portRe.exec(part);
    const lo = m ? Number(m[1]) : NaN;
    const hi = m?.[3] ? Number(m[3]) : lo;
    if (!m || lo < 1 || hi > 65535 || hi < lo) return `${ports.trim()} is not a port, a range such as 8000-8080, or a list such as 80,443.`;
  }
  return undefined;
}

const v4 = /^(\d{1,3})(\.(\d{1,3})){3}\/(\d{1,2})$/;

/** cidrProblem says why a range as written is not one, or undefined when it is. */
export function cidrProblem(cidr: string): string | undefined {
  const c = cidr.trim();
  if (!c) return "A rule's range is not given.";
  if (v4.test(c)) {
    const [addr, bits] = c.split("/");
    if (addr!.split(".").every((o) => Number(o) <= 255) && Number(bits) <= 32) return undefined;
  } else if (c.includes(":") && /^[0-9a-fA-F:.]+\/\d{1,3}$/.test(c) && Number(c.split("/")[1]) <= 128) {
    return undefined;
  }
  return `${c} is not an IPv4 or IPv6 range, such as 10.0.0.0/24 or 2001:db8::/32.`;
}

/** draftProblems says what keeps the drafts from being applied, each once. */
export function draftProblems(ds: Draft[]): string[] {
  const out = new Set<string>();
  for (const d of ds) {
    if (takesPorts(d.protocol)) {
      const p = portsProblem(d.ports);
      if (p) out.add(p);
    }
    if (d.peer === "cidr") {
      const p = cidrProblem(d.value);
      if (p) out.add(p);
    }
    if (d.peer === "group" && !d.value) out.add("A rule's group is not chosen.");
  }
  return [...out];
}

/** The inbound rules offered with one click. */
export function presets(group: string): { label: string; draft: Draft }[] {
  return [
    { label: "SSH", draft: { protocol: SecurityGroupProtocol.TCP, ports: "22", peer: "any4", value: "" } },
    { label: "HTTP/HTTPS", draft: { protocol: SecurityGroupProtocol.TCP, ports: "80,443", peer: "any6", value: "" } },
    { label: "ICMP", draft: { protocol: SecurityGroupProtocol.ICMP, ports: "", peer: "any4", value: "" } },
    { label: "This group", draft: { protocol: SecurityGroupProtocol.ALL, ports: "", peer: "group", value: group } },
  ];
}

/**
 * newlyRestricted is the interfaces in the group that its first egress rule
 * would restrict: those none of whose other groups has egress rules yet. An
 * interface in a group the project lacks already sends nothing, and one in a
 * group with egress rules already sends only what they allow.
 */
export function newlyRestricted(group: string, network: string, attachments: Attachment[], groups: SecurityGroup[]): Attachment[] {
  const egressOfGroup = new Map(groups.filter((g) => g.spec?.network === network).map((g) => [g.metadata?.name ?? "", g.spec?.egress.length ?? 0]));
  return attachments.filter(
    (a) =>
      a.network === network &&
      a.securityGroups.includes(group) &&
      a.securityGroups.every((g) => g === group || g === "default" || egressOfGroup.get(g) === 0),
  );
}
