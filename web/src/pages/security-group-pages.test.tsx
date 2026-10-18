// @vitest-environment jsdom

// A security group's pages: the group's rules said as sentences, and the
// pages that make a group or edit its rules, as the RPCs they send and the
// whole object each Apply carries. Creating is one create-only Apply;
// editing applies at the version the page opened on and never over a newer
// group; delete is the API's Delete.

import { clone, create, equals } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  EgressRuleSchema,
  IngressRuleSchema,
  SecurityGroupProtocol,
  SecurityGroupSchema,
  type ApplySecurityGroupRequest,
} from "@hangar/api/hangar/network/v1alpha1/security_group_pb";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { EditSecurityGroup, NewSecurityGroup, SecurityGroupDetail, SecurityGroupList } from "@/pages/security-groups";
import { polyfillDialog, withApi } from "@/test/harness";
import { groups, networkApi, type NetworkApi, type Overrides } from "@/test/network-api";

const navigate = vi.fn();
vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  useNavigate: () => navigate,
  Link: (await import("@/test/fake-link")).FakeLink,
}));

beforeAll(polyfillDialog);
beforeEach(() => navigate.mockReset());
afterEach(cleanup);

const { TCP, UDP, ICMP, ALL } = SecurityGroupProtocol;

function show(page: ReactNode, o: Overrides = {}): NetworkApi {
  const api = networkApi(o);
  render(page, { wrapper: withApi(api) });
  return api;
}

const type = (label: string | RegExp, value: string, at = 0) => fireEvent.change(screen.getAllByLabelText(label)[at]!, { target: { value } });
const click = (name: string | RegExp, at = -1) => fireEvent.click(screen.getAllByRole("button", { name }).at(at)!);
const button = (name: string | RegExp) => screen.getAllByRole("button", { name }).at(-1) as HTMLButtonElement;
const writes = (api: NetworkApi) => api.requests.filter((r) => !r.method.startsWith("List") && r.method !== "TestPermissions");
const methods = (api: NetworkApi) => writes(api).map((w) => w.method);
const lastApply = (api: NetworkApi) => writes(api).filter((w) => w.method.startsWith("Apply")).at(-1)!.request as ApplySecurityGroupRequest;
const opened = (name: string) => ({ to: "/projects/$project/security-groups/$name", params: { project: "demo", name }, replace: true });

/** A group on default with no outbound rules, which the api container is in with web. */
const idle = () => {
  const g = clone(SecurityGroupSchema, groups()[0]);
  g.metadata!.name = "idle";
  g.spec!.egress = [];
  return g;
};

describe("a security group's page", () => {
  it("says each rule as what it lets through", async () => {
    show(<SecurityGroupDetail name="web" />);
    expect(await screen.findByText("Lets in TCP ports 80 and 443 from any IPv6 address.")).toBeTruthy();
    expect(screen.getByText("Lets in TCP port 8080 from members of default.")).toBeTruthy();
    expect(screen.getByText("Lets in ICMP from any IPv6 address.")).toBeTruthy();
    expect(screen.getByText("Lets in everything from members of web.")).toBeTruthy();
    expect(screen.getByText("Lets out UDP port 53 to 192.0.2.53/32.")).toBeTruthy();
    expect(screen.getByText("A member sends only what some outbound rule of its groups lets out.")).toBeTruthy();
  });

  it("opens the edit page from Edit rules", async () => {
    show(<SecurityGroupDetail name="web" />);
    const edit = await screen.findByRole("link", { name: /Edit rules/ });
    expect(edit.getAttribute("href")).toBe("/projects/demo/security-groups/web/edit");
  });

  it("says a group interfaces are in is not deleted", async () => {
    const api = show(<SecurityGroupDetail name="web" />);
    await screen.findByText("Members");
    await waitFor(() => expect(screen.getAllByText("api").length).toBeGreaterThan(0));
    click("Delete");
    type("Name, to confirm", "web");
    expect(screen.getByText("1 interface is in this group.")).toBeTruthy();
    expect(button("Delete").disabled).toBe(true);
    expect(writes(api)).toEqual([]);
  });

  it("deletes a group no interface is in", async () => {
    const g = idle();
    const api = show(<SecurityGroupDetail name="idle" />, { listSecurityGroups: () => ({ items: [g] }) });
    await screen.findByText("Inbound");
    await waitFor(() => expect(screen.queryAllByLabelText("Loading")).toHaveLength(0));
    click("Delete");
    type("Name, to confirm", "idle");
    click("Delete");
    await waitFor(() => expect(writes(api)).toEqual([{ method: "DeleteSecurityGroup", request: expect.objectContaining({ project: "demo", name: "idle" }) }]));
    expect(navigate).toHaveBeenCalledWith({ to: "/projects/$project/security-groups", params: { project: "demo" } });
  });
});

describe("the security groups list", () => {
  it("links New security group to its page", async () => {
    show(<SecurityGroupList />);
    const link = await screen.findByRole("link", { name: /New security group/ });
    expect(link.getAttribute("href")).toBe("/projects/demo/new/security-group");
  });
});

describe("the new security group page", () => {
  it("creates a group with one create-only Apply of exactly the page's rules, and opens it", async () => {
    const api = show(<NewSecurityGroup />);
    await screen.findByText("Inbound rules");
    await waitFor(() => expect((screen.getByLabelText("Network") as HTMLSelectElement).value).toBe("default"));
    type("Group name", "ssh");
    expect(screen.getByText(/hangar apply --create-only -p demo -f ssh\.yaml/)).toBeTruthy();
    click("SSH");
    click("This group");
    click("Rule", 0);
    type("Ports", "8000-8080", 2);
    type("From", "cidr", 2);
    type("Range", " 2001:db8::/32 ");
    click("Rule", 1);
    type("Protocol", String(ALL), 3);
    type("To", "any6");
    expect(screen.getByText("Lets in TCP port 22 from any IPv4 address.")).toBeTruthy();
    expect(screen.getByText("Lets out everything to any IPv6 address.")).toBeTruthy();
    click("Create security group");
    await waitFor(() => expect(navigate).toHaveBeenCalledWith(opened("ssh")));
    expect(methods(api)).toEqual(["ApplySecurityGroup"]);
    expect(lastApply(api).createOnly).toBe(true);
    const want = create(SecurityGroupSchema, {
      metadata: { name: "ssh", project: "demo" },
      spec: {
        network: "default",
        ingress: [
          { protocol: TCP, ports: "22", from: { peer: { case: "cidr", value: "0.0.0.0/0" } } },
          { protocol: ALL, ports: "", from: { peer: { case: "group", value: "ssh" } } },
          { protocol: TCP, ports: "8000-8080", from: { peer: { case: "cidr", value: "2001:db8::/32" } } },
        ],
        egress: [{ protocol: ALL, ports: "", to: { peer: { case: "cidr", value: "::/0" } } }],
      },
    });
    expect(equals(SecurityGroupSchema, lastApply(api).securityGroup!, want)).toBe(true);
  });

  it("sends no ports for a rule switched to ICMP", async () => {
    const api = show(<NewSecurityGroup />);
    await screen.findByText("Inbound rules");
    type("Group name", "ping");
    click("Rule", 0);
    type("Ports", "22");
    type("Protocol", String(ICMP));
    click("Create security group");
    await waitFor(() => expect(methods(api)).toEqual(["ApplySecurityGroup"]));
    const rule = lastApply(api).securityGroup!.spec!.ingress[0]!;
    expect(rule.protocol).toBe(ICMP);
    expect(rule.ports).toBe("");
  });

  it("sends nothing while a rule's ports or range are not ones, and says why", async () => {
    const api = show(<NewSecurityGroup />);
    await screen.findByText("Inbound rules");
    type("Group name", "bad");
    click("Rule", 0);
    type("Ports", "http");
    expect(screen.getByText(/http is not a port/)).toBeTruthy();
    type("Ports", "443");
    type("From", "cidr");
    type("Range", "10.0.0.1");
    expect(screen.getByText(/10\.0\.0\.1 is not an IPv4 or IPv6 range/)).toBeTruthy();
    expect(button("Create security group").disabled).toBe(true);
    expect(writes(api)).toEqual([]);
  });

  it("says a name the project has, and sends nothing", async () => {
    const api = show(<NewSecurityGroup />);
    await screen.findByText("Inbound rules");
    type("Group name", "web");
    expect(await screen.findByText("A security group named web exists in this project.")).toBeTruthy();
    expect(button("Create security group").disabled).toBe(true);
    expect(writes(api)).toEqual([]);
  });

  it("says a name taken after the list loaded as the form does, from the create-only Apply's refusal", async () => {
    const api = show(<NewSecurityGroup />, { listSecurityGroups: () => ({ items: [] }) });
    const before = structuredClone(api.server);
    await screen.findByText("Inbound rules");
    await waitFor(() => expect(screen.queryByText(/still loading/)).toBeNull());
    type("Group name", "web");
    click("Create security group");
    expect(await screen.findByText("A security group named web exists in this project.")).toBeTruthy();
    expect(writes(api)).toHaveLength(1);
    expect(api.server).toEqual(before);
    expect(navigate).not.toHaveBeenCalled();
  });

  it("states it when the caller's roles hold no creating here", async () => {
    show(<NewSecurityGroup />, { permissions: () => ({ permissions: [] }) });
    expect(await screen.findByText(/do not include creating security groups/)).toBeTruthy();
    expect(screen.queryByText("Inbound rules")).toBeNull();
  });
});

describe("the edit rules page", () => {
  it("applies the edited rules at the version it opened on, the rest untouched, and opens the group", async () => {
    const api = show(<EditSecurityGroup name="web" />);
    await screen.findByText("Inbound rules");
    click("Rule", 1);
    // Four inbound rules and the outbound one come first.
    type("To", "any4", 1);
    type("Ports", "443", 5);
    click("Apply rules");
    await waitFor(() => expect(navigate).toHaveBeenCalledWith(opened("web")));
    expect(methods(api)).toEqual(["ApplySecurityGroup"]);
    expect(lastApply(api).createOnly).toBe(false);
    const want = clone(SecurityGroupSchema, groups()[0]);
    want.spec!.egress.push(create(EgressRuleSchema, { protocol: TCP, ports: "443", to: { peer: { case: "cidr", value: "0.0.0.0/0" } } }));
    expect(equals(SecurityGroupSchema, lastApply(api).securityGroup!, want)).toBe(true);
  });

  it("applies unchanged rules as they were, ICMP, ALL and group peers included", async () => {
    const api = show(<EditSecurityGroup name="web" />);
    await screen.findByText("Inbound rules");
    expect(screen.getByText(/^hangar apply -p demo -f web\.yaml$/)).toBeTruthy();
    click("Apply rules");
    await waitFor(() => expect(methods(api)).toEqual(["ApplySecurityGroup"]));
    expect(equals(SecurityGroupSchema, lastApply(api).securityGroup!, groups()[0])).toBe(true);
  });

  it("removes a rule and only that rule", async () => {
    const api = show(<EditSecurityGroup name="web" />);
    await screen.findByText("Inbound rules");
    click("Remove rule", 1);
    click("Apply rules");
    await waitFor(() => expect(methods(api)).toEqual(["ApplySecurityGroup"]));
    const want = clone(SecurityGroupSchema, groups()[0]);
    want.spec!.ingress.splice(1, 1);
    expect(equals(SecurityGroupSchema, lastApply(api).securityGroup!, want)).toBe(true);
  });

  it("warns which interfaces a group's first outbound rule restricts", async () => {
    const g = idle();
    show(<EditSecurityGroup name="idle" />, {
      listSecurityGroups: () => ({ items: [g] }),
      listMachines: () => ({
        items: [
          { metadata: { name: "api-x1", project: "demo", ownerReferences: [{ kind: "Container", name: "api", controller: true }] }, spec: { interfaces: [{ name: "eth0", network: "default", securityGroups: ["default", "idle"] }] } },
        ],
      }),
    });
    await screen.findByText("Inbound rules");
    expect(screen.queryByText(/Outbound rules restrict/)).toBeNull();
    click("Rule", 1);
    expect(await screen.findByText("Outbound rules restrict 1 interface")).toBeTruthy();
    expect(screen.getByText(/api on network default then send only what an outbound rule/)).toBeTruthy();
  });

  it("does not overwrite a newer group: says the rules changed, and reloads from it", async () => {
    const newer = clone(SecurityGroupSchema, groups()[0]);
    newer.metadata!.resourceVersion = 12n;
    newer.spec!.ingress = [create(IngressRuleSchema, { protocol: UDP, ports: "51820", from: { peer: { case: "cidr", value: "::/0" } } })];
    const api = show(<EditSecurityGroup name="web" />, { server: { groups: [newer] } });
    await screen.findByText("Inbound rules");
    click("Apply rules");
    expect(await screen.findByText("The rules changed after this page opened.")).toBeTruthy();
    expect(button("Apply rules").disabled).toBe(true);
    expect(screen.queryByText("Applying the rules failed")).toBeNull();
    expect(equals(SecurityGroupSchema, api.server.groups[0]!, newer)).toBe(true);

    click("Reload");
    await waitFor(() => expect(screen.queryByText("The rules changed after this page opened.")).toBeNull());
    expect((screen.getAllByLabelText("Ports")[0] as HTMLInputElement).value).toBe("51820");
    click("Apply rules");
    await waitFor(() => expect(methods(api)).toEqual(["ApplySecurityGroup", "GetSecurityGroup", "ApplySecurityGroup"]));
    expect(equals(SecurityGroupSchema, lastApply(api).securityGroup!, newer)).toBe(true);
  });

  it("shows another refusal of the rules above its button", async () => {
    show(<EditSecurityGroup name="web" />, {
      apply: () => {
        throw new ConnectError("bad ports", Code.InvalidArgument);
      },
    });
    await screen.findByText("Inbound rules");
    click("Apply rules");
    expect(await screen.findByText("Applying the rules failed")).toBeTruthy();
    expect(screen.getByText(/bad ports/)).toBeTruthy();
    expect(navigate).not.toHaveBeenCalled();
  });

  it("states it when the caller's roles hold no changing here", async () => {
    show(<EditSecurityGroup name="web" />, { permissions: () => ({ permissions: [] }) });
    expect(await screen.findByText(/do not include changing security groups/)).toBeTruthy();
  });
});
