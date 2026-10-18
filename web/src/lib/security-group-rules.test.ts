import { create } from "@bufbuild/protobuf";
import {
  EgressRuleSchema,
  IngressRuleSchema,
  SecurityGroupProtocol,
  SecurityGroupSchema,
} from "@hangar/api/hangar/network/v1alpha1/security_group_pb";
import { describe, expect, it } from "vitest";
import type { Attachment } from "@/lib/network";
import {
  cidrProblem,
  draftOf,
  draftProblems,
  egressOf,
  ingressOf,
  newlyRestricted,
  portsProblem,
  sentence,
  type Draft,
} from "@/lib/security-group-rules";

const { TCP, UDP, ICMP, ALL } = SecurityGroupProtocol;
const d = (protocol: SecurityGroupProtocol, ports: string, peer: Draft["peer"], value = ""): Draft => ({ protocol, ports, peer, value });

describe("draftOf and back", () => {
  it("reads 0.0.0.0/0 and ::/0 as any IPv4 and any IPv6, and writes them back as those ranges", () => {
    const rules = [
      create(IngressRuleSchema, { protocol: TCP, ports: "443", from: { peer: { case: "cidr", value: "::/0" } } }),
      create(IngressRuleSchema, { protocol: TCP, ports: "22", from: { peer: { case: "cidr", value: "0.0.0.0/0" } } }),
      create(IngressRuleSchema, { protocol: TCP, ports: "22", from: { peer: { case: "cidr", value: "203.0.113.0/24" } } }),
      create(IngressRuleSchema, { protocol: ALL, ports: "", from: { peer: { case: "group", value: "web" } } }),
    ];
    const drafts = rules.map(draftOf);
    expect(drafts.map((x) => x.peer)).toEqual(["any6", "any4", "cidr", "group"]);
    expect(ingressOf(drafts)).toEqual(rules);
  });

  it("drops ports from a rule whose protocol takes none, and spaces from a list", () => {
    const [icmp, list] = egressOf([d(ICMP, "22", "any4"), d(TCP, " 80, 443 ", "group", "db")]);
    expect(icmp!.ports).toBe("");
    expect(list!.ports).toBe("80,443");
    expect(list).toEqual(create(EgressRuleSchema, { protocol: TCP, ports: "80,443", to: { peer: { case: "group", value: "db" } } }));
  });
});

describe("sentence", () => {
  it.each([
    [d(TCP, "443", "any6"), "in", "Lets in TCP port 443 from any IPv6 address"],
    [d(TCP, "", "any4"), "in", "Lets in TCP on every port from any IPv4 address"],
    [d(UDP, "8000-8080", "cidr", "10.0.0.0/24"), "in", "Lets in UDP ports 8000–8080 from 10.0.0.0/24"],
    [d(TCP, "80,443,8443", "any6"), "in", "Lets in TCP ports 80, 443 and 8443 from any IPv6 address"],
    [d(ALL, "", "group", "web"), "in", "Lets in everything from members of web"],
    [d(ICMP, "", "any4"), "out", "Lets out ICMP to any IPv4 address"],
    [d(TCP, "5432", "group", "db"), "out", "Lets out TCP port 5432 to members of db"],
  ] as const)("%j %s reads %s", (draft, dir, want) => {
    expect(sentence(draft, dir)).toBe(want);
  });
});

describe("what keeps rules from being applied", () => {
  it("takes ports, ranges and lists, and refuses anything else", () => {
    for (const ok of ["", "443", "8000-8080", "80,443", "1-65535"]) expect(portsProblem(ok)).toBeUndefined();
    for (const bad of ["0", "65536", "80-79", "http", "80,"]) expect(portsProblem(bad)).toMatch(/is not a port/);
  });

  it("takes IPv4 and IPv6 ranges, and refuses an address with no length", () => {
    for (const ok of ["10.0.0.0/24", "2001:db8::/32", "::/0"]) expect(cidrProblem(ok)).toBeUndefined();
    expect(cidrProblem("10.0.0.1")).toMatch(/is not an IPv4 or IPv6 range/);
    expect(cidrProblem(" ")).toBe("A rule's range is not given.");
  });

  it("says each problem once, and none of ports on a rule that takes none", () => {
    expect(draftProblems([d(TCP, "x", "any4"), d(UDP, "x", "any4"), d(ICMP, "x", "group", "")])).toEqual([
      "x is not a port, a range such as 8000-8080, or a list such as 80,443.",
      "A rule's group is not chosen.",
    ]);
  });
});

describe("newlyRestricted", () => {
  const att = (name: string, groups: string[], network = "default"): Attachment => ({
    kind: "Container",
    name,
    iface: "eth0",
    network,
    ipv4: "",
    ipv6: "",
    securityGroups: groups,
  });
  const group = (name: string, egress: number, network = "default") =>
    create(SecurityGroupSchema, {
      metadata: { name },
      spec: { network, egress: Array.from({ length: egress }, () => create(EgressRuleSchema, { protocol: ALL, to: { peer: { case: "cidr", value: "::/0" } } })) },
    });

  it("is the group's interfaces whose other groups let everything out", () => {
    const got = newlyRestricted(
      "web",
      "default",
      [att("a", ["web"]), att("b", ["default", "web"]), att("c", ["web", "locked"]), att("d", ["db"]), att("e", ["web"], "other")],
      [group("web", 0), group("locked", 1), group("db", 0)],
    );
    expect(got.map((a) => a.name)).toEqual(["a", "b"]);
  });

  it("leaves out an interface in a group the project lacks, which already sends nothing", () => {
    expect(newlyRestricted("web", "default", [att("a", ["web", "missing"])], [group("web", 0)])).toEqual([]);
  });
});
