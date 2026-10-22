import { create } from "@bufbuild/protobuf";
import { ConditionSchema, ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import { SecurityGroupProtocol, SecurityGroupSchema } from "@hangar/api/hangar/network/v1alpha1/security_group_pb";
import { describe, expect, it } from "vitest";
import type { Attachment } from "@/lib/network";
import { examplesOf, inboundOf, portForwardYaml, portProblem, portsHold, publicPorts, stateOf, suggestName, type PortForward } from "@/lib/port-forwards";
import { fixtureItems } from "@/lib/port-forwards.fixture";

const web = { kind: "Container" as const, name: "web", iface: "eth0" };

function forward(o: { protocol?: "TCP" | "UDP"; targetPort?: number; port?: number; ready?: boolean }): PortForward {
  const port = o.port ?? 23817;
  return {
    metadata: { name: "web-22", project: "demo", uid: "u", labels: {} },
    spec: { target: web, protocol: o.protocol ?? "TCP", targetPort: o.targetPort ?? 22 },
    status: {
      ipv4: port ? "203.0.113.20" : "",
      port,
      machine: "",
      conditions: [create(ConditionSchema, { type: "Ready", status: o.ready === false ? ConditionStatus.FALSE : ConditionStatus.TRUE, reason: o.ready === false ? "TargetNotFound" : "" })],
    },
  };
}

const att = (groups: string[]): Attachment => ({ kind: "Container", name: "web", iface: "eth0", network: "default", ipv4: "10.0.0.2", ipv6: "", securityGroups: groups });
const group = (name: string, rules: { protocol: SecurityGroupProtocol; ports: string; from: string; group?: boolean }[], network = "default") =>
  create(SecurityGroupSchema, {
    metadata: { name },
    spec: {
      network,
      ingress: rules.map((r) => ({ protocol: r.protocol, ports: r.ports, from: { peer: r.group ? { case: "group" as const, value: r.from } : { case: "cidr" as const, value: r.from } } })),
    },
  });
const { TCP, UDP, ALL } = SecurityGroupProtocol;

describe("whether a forward's traffic gets in", () => {
  it("is open when a rule of the interface's groups lets the port in from any IPv4 address", () => {
    const r = inboundOf(web, "TCP", 22, [att(["default", "web"])], [group("web", [{ protocol: TCP, ports: "22", from: "0.0.0.0/0" }])]);
    expect(r).toMatchObject({ verdict: "open", known: true, groups: ["default", "web"] });
  });

  it("counts a rule of every protocol and every port", () => {
    expect(inboundOf(web, "UDP", 19132, [att(["web"])], [group("web", [{ protocol: ALL, ports: "", from: "0.0.0.0/0" }])]).verdict).toBe("open");
  });

  it("is some when only narrower IPv4 ranges are let in", () => {
    const r = inboundOf(web, "TCP", 22, [att(["office"])], [group("office", [{ protocol: TCP, ports: "22", from: "198.51.100.0/24" }])]);
    expect(r.verdict).toBe("some");
    expect(r.admitting.map((a) => a.from)).toEqual(["198.51.100.0/24"]);
  });

  it("is none for another protocol, another port, IPv6 only, a group peer, or the built-in default", () => {
    const groups = [
      group("web", [
        { protocol: TCP, ports: "19132", from: "0.0.0.0/0" },
        { protocol: UDP, ports: "80,443", from: "0.0.0.0/0" },
        { protocol: UDP, ports: "", from: "::/0" },
        { protocol: UDP, ports: "", from: "web", group: true },
      ]),
    ];
    expect(inboundOf(web, "UDP", 19132, [att(["default", "web"])], groups)).toMatchObject({ verdict: "none", known: true, admitting: [] });
  });

  it("does not count a group of the same name on another network", () => {
    const g = group("web", [{ protocol: TCP, ports: "22", from: "0.0.0.0/0" }], "backend");
    expect(inboundOf(web, "TCP", 22, [att(["web"])], [g]).verdict).toBe("none");
  });

  it("says the groups are not known for an interface no machine has", () => {
    expect(inboundOf(web, "TCP", 22, [], [])).toMatchObject({ verdict: "none", known: false, groups: [] });
  });
});

describe("ports as a rule writes them", () => {
  it("holds a port in a list or a range, and every port when empty", () => {
    expect(portsHold("", 5)).toBe(true);
    expect(portsHold("80,443", 443)).toBe(true);
    expect(portsHold("8000-8080", 8080)).toBe(true);
    expect(portsHold("8000-8080", 8081)).toBe(false);
    expect(portsHold("22", 2222)).toBe(false);
  });
});

describe("a forward's connection examples", () => {
  it("is ssh for TCP port 22, with the drawn port", () => {
    expect(examplesOf(forward({}))).toEqual([{ label: "SSH", command: "ssh -p 23817 user@203.0.113.20" }]);
  });

  it("is nc -u for UDP", () => {
    expect(examplesOf(forward({ protocol: "UDP", targetPort: 19132, port: 29405 }))[0]!.command).toBe("nc -u 203.0.113.20 29405");
  });

  it("is the address for any other TCP port, and nothing before the port is drawn", () => {
    expect(examplesOf(forward({ targetPort: 5432, port: 27640 }))[0]!.command).toBe("203.0.113.20:27640");
    expect(examplesOf(forward({ port: 0 }))).toEqual([]);
  });
});

describe("the placeholder data", () => {
  it("draws every public port from the shared address's range", () => {
    const drawn = fixtureItems("demo").map((f) => f.status.port).filter(Boolean);
    expect(drawn.length).toBeGreaterThan(0);
    for (const p of drawn) {
      expect(p).toBeGreaterThanOrEqual(publicPorts.min);
      expect(p).toBeLessThanOrEqual(publicPorts.max);
    }
  });
});

describe("a forward's state", () => {
  it("is Pending until the port is drawn, Waiting when not ready, else Ready", () => {
    expect(stateOf(forward({ port: 0 }))).toMatchObject({ label: "Pending", unassigned: true });
    expect(stateOf(forward({ ready: false }))).toMatchObject({ label: "Waiting", reason: "TargetNotFound" });
    expect(stateOf(forward({}))).toMatchObject({ label: "Ready", light: "ok" });
  });
});

describe("the create form", () => {
  it("suggests a name from the machine and the port", () => {
    expect(suggestName("game", "UDP", 19132)).toBe("game-19132-udp");
    expect(suggestName("web", "TCP", 22)).toBe("web-22");
    expect(suggestName("", "TCP", 22)).toBe("");
  });

  it("takes a target port from 1 to 65535", () => {
    expect(portProblem("22")).toBeUndefined();
    expect(portProblem("")).toBe("The target port is empty.");
    expect(portProblem("0")).toMatch(/not a port/);
    expect(portProblem("65536")).toMatch(/not a port/);
    expect(portProblem("2x")).toMatch(/not a port/);
  });

  it("writes the manifest without the public port, which is not the person's to choose", () => {
    const yaml = portForwardYaml({ metadata: { name: "web-22" }, spec: { target: web, protocol: "TCP", targetPort: 22 } });
    expect(yaml).toContain("kind: PortForward");
    expect(yaml).toContain("container: web");
    expect(yaml).not.toContain("interface:");
    expect(yaml).toContain("targetPort: 22");
    expect(yaml).not.toMatch(/\bport:/);
  });
});
