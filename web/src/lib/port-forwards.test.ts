import { create } from "@bufbuild/protobuf";
import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import { PortForwardSchema, Protocol, type PortForward } from "@hangar/api/hangar/network/v1alpha1/port_forward_pb";
import { SecurityGroupProtocol, SecurityGroupSchema } from "@hangar/api/hangar/network/v1alpha1/security_group_pb";
import { describe, expect, it } from "vitest";
import { portForwardYaml, type Attachment } from "@/lib/network";
import { examplesOf, forwardTargetOf, inboundOf, portProblem, portsHold, stateOf, suggestName } from "@/lib/port-forwards";

const web = { kind: "Container" as const, name: "web", iface: "eth0" };

function forward(o: {
  protocol?: Protocol;
  targetPort?: number;
  port?: number;
  ipv4?: string;
  reason?: string;
  message?: string;
  node?: string;
  deleting?: boolean;
  noCondition?: boolean;
}): PortForward {
  const ok = !o.reason || o.reason === "Forwarding";
  return create(PortForwardSchema, {
    metadata: { name: "web-22", project: "demo", deleteTime: o.deleting ? { seconds: 1n } : undefined },
    spec: { target: { object: { case: "container", value: "web" } }, protocol: o.protocol ?? Protocol.TCP, targetPort: o.targetPort ?? 22 },
    status: {
      ipv4: o.ipv4 ?? "198.51.100.3",
      port: o.port ?? 23817,
      node: o.node ?? "node-1",
      conditions: o.noCondition
        ? []
        : [{ type: "Ready", status: ok ? ConditionStatus.TRUE : ConditionStatus.FALSE, reason: o.reason ?? "Forwarding", message: o.message ?? "" }],
    },
  });
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
    expect(examplesOf(forward({}))).toEqual([{ label: "SSH", command: "ssh -p 23817 user@198.51.100.3" }]);
  });

  it("is nc -u for UDP", () => {
    expect(examplesOf(forward({ protocol: Protocol.UDP, targetPort: 19132, port: 29405 }))[0]!.command).toBe("nc -u 198.51.100.3 29405");
  });

  it("is the address for any other TCP port, and nothing before the address is known", () => {
    expect(examplesOf(forward({ targetPort: 5432, port: 27640 }))[0]!.command).toBe("198.51.100.3:27640");
    expect(examplesOf(forward({ ipv4: "" }))).toEqual([]);
  });
});

describe("a forward's state, from the apiserver's reasons", () => {
  it("is Ready when forwarding", () => {
    expect(stateOf(forward({}))).toMatchObject({ label: "Ready", light: "ok" });
  });

  it("is Pending while the target is on its way to a node, or nothing is written yet", () => {
    expect(stateOf(forward({ reason: "PortPending", ipv4: "", message: "Container web is not on a node yet" }))).toMatchObject({
      label: "Pending",
      reason: "PortPending",
      message: "Container web is not on a node yet",
    });
    expect(stateOf(forward({ noCondition: true }))).toMatchObject({ label: "Pending", pulse: true });
  });

  it("is Waiting for a missing target or interface, with the API's message", () => {
    expect(stateOf(forward({ reason: "TargetNotFound", message: "Container db does not exist, or has no machine yet" }))).toMatchObject({
      label: "Waiting",
      reason: "TargetNotFound",
      message: "Container db does not exist, or has no machine yet",
    });
    expect(stateOf(forward({ reason: "InterfaceNotFound" }))).toMatchObject({ label: "Waiting", reason: "InterfaceNotFound" });
  });

  it("explains a node with no shared IPv4 address", () => {
    const s = stateOf(forward({ reason: "NoSharedIPv4", ipv4: "", node: "node-2" }));
    expect(s).toMatchObject({ label: "Waiting", reason: "NoSharedIPv4" });
    expect(s.message).toMatch(/^Node node-2 has no shared IPv4 address yet/);
  });

  it("is Deleting until the node that held the port has closed it", () => {
    expect(stateOf(forward({ deleting: true }))).toMatchObject({ label: "Deleting", message: "Its port is released once node node-1 has closed it." });
    expect(stateOf(forward({ deleting: true, node: "" })).message).toBe("Its port is released.");
  });

  it("reads the target's interface as eth0 when unset", () => {
    expect(forwardTargetOf(forward({}))).toEqual(web);
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

  it("writes the manifest with its spec alone: the public port is not the person's to choose", () => {
    const yaml = portForwardYaml(forward({}));
    expect(yaml).toContain("kind: PortForward");
    expect(yaml).toContain("container: web");
    expect(yaml).toContain("targetPort: 22");
    expect(yaml).toContain("PROTOCOL_TCP");
    expect(yaml).not.toContain("23817");
  });
});
