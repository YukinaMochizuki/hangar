// @vitest-environment jsdom

// Every change the network pages make, as the RPCs it sends and the whole
// object each Apply carries: creating asks for the name first and applies
// only when it is not found; editing rules and binding or moving an address
// apply at the version the dialog opened from and never over a newer
// object; making an endpoint public or private and unbinding read the
// object and apply it back with one value changed; delete is the API's
// Delete. The API's refusal is shown where the action was asked for.

import { clone, create, equals } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { AddressSchema, AddressTargetSchema, type ApplyAddressRequest } from "@hangar/api/hangar/network/v1alpha1/address_pb";
import { NetworkSchema, type ApplyNetworkRequest } from "@hangar/api/hangar/network/v1alpha1/network_pb";
import {
  PublicEndpointAccess,
  PublicEndpointSchema,
  type ApplyPublicEndpointRequest,
} from "@hangar/api/hangar/network/v1alpha1/public_endpoint_pb";
import {
  EgressRuleSchema,
  IngressRuleSchema,
  SecurityGroupProtocol,
  SecurityGroupSchema,
  type ApplySecurityGroupRequest,
} from "@hangar/api/hangar/network/v1alpha1/security_group_pb";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { AddressDetail, AddressList } from "@/pages/addresses";
import { NetworkDetail, NetworkList } from "@/pages/networks";
import { PublicEndpointDetail, PublicEndpointList } from "@/pages/public-endpoints";
import { SecurityGroupDetail, SecurityGroupList } from "@/pages/security-groups";
import { never, polyfillDialog, withApi } from "@/test/harness";
import { addresses, endpoints, groups, networkApi, type NetworkApi, type Overrides } from "@/test/network-api";

const navigate = vi.fn();
vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  useNavigate: () => navigate,
  Link: ({ children, className }: { children: ReactNode; className?: string }) => <a className={className}>{children}</a>,
}));

beforeAll(polyfillDialog);
beforeEach(() => navigate.mockReset());
afterEach(cleanup);

function show(page: ReactNode, o: Overrides = {}): NetworkApi {
  const api = networkApi(o);
  render(page, { wrapper: withApi(api) });
  return api;
}

const type = (label: string | RegExp, value: string) => fireEvent.change(screen.getByLabelText(label), { target: { value } });
const click = (name: string | RegExp) => fireEvent.click(screen.getAllByRole("button", { name }).at(-1)!);
const button = (name: string | RegExp) => screen.getAllByRole("button", { name }).at(-1) as HTMLButtonElement;
/** The writes sent, in order: everything but the lists and the permission check. */
const writes = (api: NetworkApi) => api.requests.filter((r) => !r.method.startsWith("List") && r.method !== "TestPermissions");
const methods = (api: NetworkApi) => writes(api).map((w) => w.method);
const lastApply = <T,>(api: NetworkApi) => writes(api).filter((w) => w.method.startsWith("Apply")).at(-1)!.request as T;
const refuse = (code: Code, msg: string) => () => {
  throw new ConnectError(msg, code);
};

async function confirmDelete(name: string) {
  click("Delete");
  type("Name, to confirm", name);
  click("Delete");
}

type CreateCase = {
  noun: string;
  page: () => ReactNode;
  open: string;
  submit: string;
  taken: string;
  hide: keyof Overrides;
  fill?: () => Promise<void>;
};

const creates: CreateCase[] = [
  { noun: "network", page: () => <NetworkList />, open: "New network", submit: "Create network", taken: "sandbox", hide: "listNetworks" },
  { noun: "security group", page: () => <SecurityGroupList />, open: "New security group", submit: "Create security group", taken: "web", hide: "listSecurityGroups" },
  { noun: "address", page: () => <AddressList />, open: "Reserve address", submit: "Reserve", taken: "spare", hide: "listAddresses" },
  {
    noun: "public endpoint",
    page: () => <PublicEndpointList />,
    open: "New public endpoint",
    submit: "Create public endpoint",
    taken: "docs",
    hide: "listPublicEndpoints",
    fill: async () => void (await screen.findByRole("option", { name: "api (container)" })),
  },
];

describe.each(creates)("creating a $noun whose name was taken after the list loaded", (c) => {
  it("is refused by the create-only Apply, said in the form's words, and replaces nothing", async () => {
    // The list the form checks against lacks the name; the server holds it.
    const api = show(c.page(), { [c.hide]: () => ({ items: [] }) });
    const before = structuredClone(api.server);
    // An empty list offers the button twice: in the header and in the empty state.
    fireEvent.click((await screen.findAllByRole("button", { name: c.open }))[0]);
    await c.fill?.();
    await waitFor(() => expect(screen.queryByText(/still loading/)).toBeNull());
    type(/^Name/, c.taken);
    click(c.submit);
    expect(await screen.findByText(`A ${c.noun} named ${c.taken} exists in this project.`)).toBeTruthy();
    expect(writes(api)).toHaveLength(1);
    expect(writes(api)[0].request).toMatchObject({ createOnly: true });
    expect(api.server).toEqual(before);
    expect(navigate).not.toHaveBeenCalled();
  });
});

describe("networks", () => {
  it("creates a network with one create-only Apply of exactly the form's object", async () => {
    const api = show(<NetworkList />);
    fireEvent.click(await screen.findByRole("button", { name: "New network" }));
    await waitFor(() => expect(screen.queryByText(/still loading/)).toBeNull());
    type("Name", "db");
    type(/^IPv4 range/, "10.30.0.0/28");
    click("Create network");
    await waitFor(() => expect(navigate).toHaveBeenCalledWith({ to: "/projects/$project/networks/$name", params: { project: "demo", name: "db" } }));
    expect(methods(api)).toEqual(["ApplyNetwork"]);
    expect(lastApply<ApplyNetworkRequest>(api).createOnly).toBe(true);
    const want = create(NetworkSchema, { metadata: { name: "db", project: "demo" }, spec: { ipv4Cidr: "10.30.0.0/28" } });
    expect(equals(NetworkSchema, lastApply<ApplyNetworkRequest>(api).network!, want)).toBe(true);
    expect(screen.getByText(/hangar apply --create-only -p demo -f db\.yaml/)).toBeTruthy();
  });

  it("sends nothing for a name the project has", async () => {
    const api = show(<NetworkList />);
    fireEvent.click(await screen.findByRole("button", { name: "New network" }));
    type("Name", "default");
    expect(await screen.findByText("A network named default exists in this project.")).toBeTruthy();
    expect(button("Create network").disabled).toBe(true);
    expect(writes(api)).toEqual([]);
  });

  it("shows the API's refusal in the form", async () => {
    show(<NetworkList />, { apply: refuse(Code.PermissionDenied, "may not apply networks") });
    fireEvent.click(await screen.findByRole("button", { name: "New network" }));
    await waitFor(() => expect(screen.queryByText(/still loading/)).toBeNull());
    type("Name", "db");
    click("Create network");
    expect(await screen.findByText("Creating the network failed")).toBeTruthy();
    expect(screen.getByText(/may not apply networks/)).toBeTruthy();
    expect(navigate).not.toHaveBeenCalled();
  });

  it("deletes a network once its name is typed, and only then", async () => {
    const api = show(<NetworkDetail name="sandbox" />);
    await screen.findByText("Summary");
    await waitFor(() => expect(screen.queryAllByLabelText("Loading")).toHaveLength(0));
    click("Delete");
    type("Name, to confirm", "sandbo");
    expect(button("Delete").disabled).toBe(true);
    type("Name, to confirm", "sandbox");
    click("Delete");
    await waitFor(() => expect(navigate).toHaveBeenCalledWith({ to: "/projects/$project/networks", params: { project: "demo" } }));
    expect(writes(api)).toEqual([{ method: "DeleteNetwork", request: expect.objectContaining({ project: "demo", name: "sandbox" }) }]);
  });

  it("says a network machines are on is not deleted, and offers no Delete", async () => {
    const api = show(<NetworkDetail name="default" />);
    await screen.findAllByText("10.0.0.2");
    click("Delete");
    type("Name, to confirm", "default");
    expect(screen.getByText("1 machine is on this network.")).toBeTruthy();
    expect(button("Delete").disabled).toBe(true);
    expect(writes(api)).toEqual([]);
  });

  it("keeps Delete off while the machines load", async () => {
    show(<NetworkDetail name="sandbox" />, { listMachines: () => never() });
    await screen.findByText("Summary");
    click("Delete");
    type("Name, to confirm", "sandbox");
    expect(screen.getByText("The project's machines are still loading.")).toBeTruthy();
    expect(button("Delete").disabled).toBe(true);
  });

  it("says when the machines did not load, and keeps Delete off", async () => {
    show(<NetworkDetail name="sandbox" />, { listMachines: refuse(Code.Unavailable, "down") });
    await screen.findByText("The project's machines did not load");
    click("Delete");
    type("Name, to confirm", "sandbox");
    expect(screen.getByText("The project's machines did not load, so whether any uses it is not known.")).toBeTruthy();
    expect(button("Delete").disabled).toBe(true);
  });

  it("shows a refused delete in the dialog", async () => {
    show(<NetworkDetail name="sandbox" />, { del: refuse(Code.FailedPrecondition, "network is in use") });
    await screen.findByText("Summary");
    await waitFor(() => expect(screen.queryAllByLabelText("Loading")).toHaveLength(0));
    await confirmDelete("sandbox");
    expect(await screen.findByText("Deleting failed")).toBeTruthy();
    expect(screen.getByText(/network is in use/)).toBeTruthy();
    expect(navigate).not.toHaveBeenCalled();
  });
});

describe("security groups", () => {
  it("creates a group with its rules, peers trimmed", async () => {
    const api = show(<SecurityGroupList />);
    fireEvent.click(await screen.findByRole("button", { name: "New security group" }));
    await waitFor(() => expect(screen.queryByText(/still loading/)).toBeNull());
    type("Name", "ssh");
    expect(screen.getByText(/hangar apply --create-only -p demo -f ssh\.yaml/)).toBeTruthy();
    type("Ports", "22");
    type("From", "  2001:db8::/32 ");
    fireEvent.click(screen.getAllByRole("button", { name: "Rule" })[0]);
    fireEvent.change(screen.getAllByLabelText("Protocol")[1], { target: { value: String(SecurityGroupProtocol.ICMP) } });
    fireEvent.change(screen.getAllByLabelText("From")[1], { target: { value: " web " } });
    fireEvent.click(screen.getAllByRole("button", { name: "Rule" })[1]);
    fireEvent.change(screen.getAllByLabelText("Protocol")[2], { target: { value: String(SecurityGroupProtocol.ALL) } });
    type("To", "::/0");
    click("Create security group");
    await waitFor(() => expect(methods(api)).toEqual(["ApplySecurityGroup"]));
    expect(lastApply<ApplySecurityGroupRequest>(api).createOnly).toBe(true);
    const want = create(SecurityGroupSchema, {
      metadata: { name: "ssh", project: "demo" },
      spec: {
        network: "default",
        ingress: [
          { protocol: SecurityGroupProtocol.TCP, ports: "22", from: { peer: { case: "cidr", value: "2001:db8::/32" } } },
          { protocol: SecurityGroupProtocol.ICMP, ports: "", from: { peer: { case: "group", value: "web" } } },
        ],
        egress: [{ protocol: SecurityGroupProtocol.ALL, ports: "", to: { peer: { case: "cidr", value: "::/0" } } }],
      },
    });
    expect(equals(SecurityGroupSchema, lastApply<ApplySecurityGroupRequest>(api).securityGroup!, want)).toBe(true);
  });

  it("sends no ports for a rule switched to ICMP", async () => {
    const api = show(<SecurityGroupList />);
    fireEvent.click(await screen.findByRole("button", { name: "New security group" }));
    await waitFor(() => expect(screen.queryByText(/still loading/)).toBeNull());
    type("Name", "ping");
    type("Ports", "22");
    type("Protocol", String(SecurityGroupProtocol.ICMP));
    click("Create security group");
    await waitFor(() => expect(methods(api)).toEqual(["ApplySecurityGroup"]));
    const rule = lastApply<ApplySecurityGroupRequest>(api).securityGroup!.spec!.ingress[0];
    expect(rule.protocol).toBe(SecurityGroupProtocol.ICMP);
    expect(rule.ports).toBe("");
  });

  it("applies edited rules at the version the dialog opened from, the rest untouched", async () => {
    const api = show(<SecurityGroupDetail name="web" />);
    await screen.findByText("Ingress");
    click("Edit rules");
    fireEvent.click(screen.getAllByRole("button", { name: "Rule" })[1]);
    fireEvent.change(screen.getAllByLabelText("To")[1], { target: { value: "0.0.0.0/0" } });
    // Four ingress rules, the egress rule, then the new one.
    fireEvent.change(screen.getAllByLabelText("Ports")[5], { target: { value: "443" } });
    click("Apply rules");
    await waitFor(() => expect(screen.queryByRole("button", { name: "Apply rules" })).toBeNull());
    expect(methods(api)).toEqual(["ApplySecurityGroup"]);
    const want = clone(SecurityGroupSchema, groups()[0]);
    want.spec!.egress.push(create(EgressRuleSchema, { protocol: SecurityGroupProtocol.TCP, ports: "443", to: { peer: { case: "cidr", value: "0.0.0.0/0" } } }));
    expect(equals(SecurityGroupSchema, lastApply<ApplySecurityGroupRequest>(api).securityGroup!, want)).toBe(true);
  });

  it("applies unchanged rules as they were, ICMP, ALL and group peers included", async () => {
    const api = show(<SecurityGroupDetail name="web" />);
    await screen.findByText("Ingress");
    click("Edit rules");
    // The dialog's own command; the page's CLI card shows a like one.
    expect(within(screen.getByRole("dialog")).getByText(/^hangar apply -p demo -f web\.yaml$/)).toBeTruthy();
    click("Apply rules");
    await waitFor(() => expect(methods(api)).toEqual(["ApplySecurityGroup"]));
    expect(equals(SecurityGroupSchema, lastApply<ApplySecurityGroupRequest>(api).securityGroup!, groups()[0])).toBe(true);
  });

  it("removes a rule and only that rule", async () => {
    const api = show(<SecurityGroupDetail name="web" />);
    await screen.findByText("Ingress");
    click("Edit rules");
    fireEvent.click(screen.getAllByRole("button", { name: "Remove rule" })[1]);
    click("Apply rules");
    await waitFor(() => expect(methods(api)).toEqual(["ApplySecurityGroup"]));
    const want = clone(SecurityGroupSchema, groups()[0]);
    want.spec!.ingress.splice(1, 1);
    expect(equals(SecurityGroupSchema, lastApply<ApplySecurityGroupRequest>(api).securityGroup!, want)).toBe(true);
  });

  it("does not overwrite a newer group: says the rules changed, and reloads from it", async () => {
    const newer = clone(SecurityGroupSchema, groups()[0]);
    newer.metadata!.resourceVersion = 12n;
    newer.spec!.ingress = [create(IngressRuleSchema, { protocol: SecurityGroupProtocol.UDP, ports: "51820", from: { peer: { case: "cidr", value: "::/0" } } })];
    const api = show(<SecurityGroupDetail name="web" />, { server: { groups: [newer] } });
    await screen.findByText("Ingress");
    click("Edit rules");
    click("Apply rules");
    expect(await screen.findByText("The rules changed after this dialog opened.")).toBeTruthy();
    expect(button("Apply rules").disabled).toBe(true);
    expect(screen.queryByText("Applying the rules failed")).toBeNull();
    expect(equals(SecurityGroupSchema, api.server.groups[0], newer)).toBe(true);

    click("Reload");
    await waitFor(() => expect(screen.queryByText("The rules changed after this dialog opened.")).toBeNull());
    expect((screen.getAllByLabelText("Ports")[0] as HTMLInputElement).value).toBe("51820");
    click("Apply rules");
    await waitFor(() => expect(methods(api)).toEqual(["ApplySecurityGroup", "GetSecurityGroup", "ApplySecurityGroup"]));
    expect(equals(SecurityGroupSchema, lastApply<ApplySecurityGroupRequest>(api).securityGroup!, newer)).toBe(true);
  });

  it("shows another refusal of the rules in the dialog", async () => {
    show(<SecurityGroupDetail name="web" />, { apply: refuse(Code.InvalidArgument, "bad ports") });
    await screen.findByText("Ingress");
    click("Edit rules");
    click("Apply rules");
    expect(await screen.findByText("Applying the rules failed")).toBeTruthy();
    expect(screen.getByText(/bad ports/)).toBeTruthy();
    expect(screen.queryByText("The rules changed after this dialog opened.")).toBeNull();
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
    const idle = groups()[0];
    idle.metadata!.name = "idle";
    const api = show(<SecurityGroupDetail name="idle" />, { listSecurityGroups: () => ({ items: [idle] }) });
    await screen.findByText("Ingress");
    await waitFor(() => expect(screen.queryAllByLabelText("Loading")).toHaveLength(0));
    await confirmDelete("idle");
    await waitFor(() =>
      expect(writes(api)).toEqual([{ method: "DeleteSecurityGroup", request: expect.objectContaining({ project: "demo", name: "idle" }) }]),
    );
    expect(navigate).toHaveBeenCalledWith({ to: "/projects/$project/security-groups", params: { project: "demo" } });
  });
});

describe("addresses", () => {
  it("reserves an unbound address", async () => {
    const api = show(<AddressList />);
    fireEvent.click(await screen.findByRole("button", { name: "Reserve address" }));
    await waitFor(() => expect(screen.queryByText(/still loading/)).toBeNull());
    type("Name", "edge");
    expect(screen.getByText(/hangar apply --create-only -p demo -f edge\.yaml/)).toBeTruthy();
    click("Reserve");
    await waitFor(() => expect(methods(api)).toEqual(["ApplyAddress"]));
    expect(lastApply<ApplyAddressRequest>(api).createOnly).toBe(true);
    const want = create(AddressSchema, { metadata: { name: "edge", project: "demo" }, spec: {} });
    expect(equals(AddressSchema, lastApply<ApplyAddressRequest>(api).address!, want)).toBe(true);
    expect(navigate).toHaveBeenCalledWith({ to: "/projects/$project/addresses/$name", params: { project: "demo", name: "edge" } });
  });

  it("binds an address at the version the dialog opened from, the rest untouched", async () => {
    const api = show(<AddressDetail name="spare" />);
    await screen.findByText("Summary");
    click("Bind");
    await screen.findByRole("option", { name: /api · eth0/ });
    fireEvent.change(screen.getByRole("combobox", { name: "Interface" }), { target: { value: "Container/api/eth0" } });
    expect(within(screen.getByRole("dialog")).getByText(/^hangar apply -p demo -f spare\.yaml$/)).toBeTruthy();
    click("Bind");
    await waitFor(() => expect(methods(api)).toEqual(["ApplyAddress"]));
    const want = clone(AddressSchema, addresses()[1]);
    want.spec!.target = create(AddressTargetSchema, { object: { case: "container", value: "api" }, interface: "eth0" });
    expect(equals(AddressSchema, lastApply<ApplyAddressRequest>(api).address!, want)).toBe(true);
  });

  it("binds to a machine typed by name, which need not exist yet", async () => {
    const api = show(<AddressDetail name="spare" />);
    await screen.findByText("Summary");
    click("Bind");
    fireEvent.change(screen.getByRole("combobox", { name: "Interface" }), { target: { value: "typed" } });
    expect(screen.getByText(/A machine that does not exist yet is waited for/)).toBeTruthy();
    expect(screen.getByText(/The machine's name is empty\./)).toBeTruthy();
    expect(button("Bind").disabled).toBe(true);
    type("Kind", "VirtualMachine");
    type("Machine", " builder ");
    type("On", "eth1");
    click("Bind");
    await waitFor(() => expect(methods(api)).toEqual(["ApplyAddress"]));
    const want = clone(AddressSchema, addresses()[1]);
    want.spec!.target = create(AddressTargetSchema, { object: { case: "virtualMachine", value: "builder" }, interface: "eth1" });
    expect(equals(AddressSchema, lastApply<ApplyAddressRequest>(api).address!, want)).toBe(true);
  });

  it("binds a typed machine's eth0 when no interface is given", async () => {
    const api = show(<AddressDetail name="spare" />);
    await screen.findByText("Summary");
    click("Bind");
    fireEvent.change(screen.getByRole("combobox", { name: "Interface" }), { target: { value: "typed" } });
    type("Machine", "builder");
    type("On", "");
    click("Bind");
    await waitFor(() => expect(methods(api)).toEqual(["ApplyAddress"]));
    expect(lastApply<ApplyAddressRequest>(api).address!.spec!.target!.interface).toBe("eth0");
  });

  it("does not move an address that moved since the dialog opened: says so, and reloads", async () => {
    const moved = clone(AddressSchema, addresses()[0]);
    moved.metadata!.resourceVersion = 25n;
    moved.spec!.target = create(AddressTargetSchema, { object: { case: "virtualMachine", value: "builder" }, interface: "eth0" });
    const api = show(<AddressDetail name="api" />, { server: { addresses: [moved, addresses()[1]] } });
    await screen.findByText("Summary");
    click("Move");
    fireEvent.change(screen.getByRole("combobox", { name: "Interface" }), { target: { value: "typed" } });
    type("Machine", "worker");
    click("Move");
    expect(await screen.findByText("The address changed after this dialog opened.")).toBeTruthy();
    expect(button("Move").disabled).toBe(true);
    expect(equals(AddressSchema, api.server.addresses[0], moved)).toBe(true);

    click("Reload");
    await waitFor(() => expect(screen.queryByText("The address changed after this dialog opened.")).toBeNull());
    expect((screen.getByRole("combobox", { name: "Interface" }) as HTMLSelectElement).value).toBe("VirtualMachine/builder/eth0");
    expect(methods(api)).toEqual(["ApplyAddress", "GetAddress"]);
  });

  it("unbinds an address by reading it and applying it back with no target", async () => {
    const newer = clone(AddressSchema, addresses()[0]);
    newer.metadata!.resourceVersion = 30n;
    newer.metadata!.labels = { role: "front", owner: "ops" };
    const api = show(<AddressDetail name="api" />, { server: { addresses: [newer, addresses()[1]] } });
    await screen.findByText("Summary");
    click("Unbind");
    click("Unbind");
    await waitFor(() => expect(methods(api)).toEqual(["GetAddress", "ApplyAddress"]));
    const want = clone(AddressSchema, newer);
    want.spec!.target = undefined;
    expect(equals(AddressSchema, lastApply<ApplyAddressRequest>(api).address!, want)).toBe(true);
    expect(lastApply<ApplyAddressRequest>(api).createOnly).toBe(false);
    await waitFor(() => expect(api.server.addresses[0].metadata?.resourceVersion).toBe(31n));
    expect(api.server.addresses[0].spec?.target).toBeUndefined();
  });

  it("deletes an address once its name is typed", async () => {
    const api = show(<AddressDetail name="spare" />);
    await screen.findByText("Summary");
    await confirmDelete("spare");
    await waitFor(() =>
      expect(writes(api)).toEqual([{ method: "DeleteAddress", request: expect.objectContaining({ project: "demo", name: "spare" }) }]),
    );
  });
});

describe("public endpoints", () => {
  it("creates a public endpoint", async () => {
    const api = show(<PublicEndpointList />);
    fireEvent.click(await screen.findByRole("button", { name: "New public endpoint" }));
    await screen.findByRole("option", { name: "api (container)" });
    await waitFor(() => expect(screen.queryByText(/still loading/)).toBeNull());
    type(/^Name/, "blog");
    expect(screen.getByText(/hangar apply --create-only -p demo -f blog\.yaml/)).toBeTruthy();
    type("Port", "3000");
    fireEvent.click(screen.getByRole("radio", { name: /Public/ }));
    click("Create public endpoint");
    await waitFor(() => expect(methods(api)).toEqual(["ApplyPublicEndpoint"]));
    expect(lastApply<ApplyPublicEndpointRequest>(api).createOnly).toBe(true);
    const want = create(PublicEndpointSchema, {
      metadata: { name: "blog", project: "demo" },
      spec: { target: { object: { case: "container", value: "api" } }, port: 3000, access: PublicEndpointAccess.PUBLIC },
    });
    expect(equals(PublicEndpointSchema, lastApply<ApplyPublicEndpointRequest>(api).publicEndpoint!, want)).toBe(true);
  });

  it("refuses a port outside 1 to 65535", async () => {
    const api = show(<PublicEndpointList />);
    fireEvent.click(await screen.findByRole("button", { name: "New public endpoint" }));
    await screen.findByRole("option", { name: "api (container)" });
    type(/^Name/, "blog");
    type("Port", "70000");
    expect(screen.getByText(/The port is not between 1 and 65535/)).toBeTruthy();
    expect(button("Create public endpoint").disabled).toBe(true);
    expect(writes(api)).toEqual([]);
  });

  it("shows no hostname it cannot know", async () => {
    show(<PublicEndpointList />);
    fireEvent.click(await screen.findByRole("button", { name: "New public endpoint" }));
    type(/^Name/, "blog");
    expect(screen.queryByText(/blog-demo/)).toBeNull();
  });

  it("makes an endpoint public by reading it and applying it back with that alone changed", async () => {
    const newer = clone(PublicEndpointSchema, endpoints()[0]);
    newer.metadata!.resourceVersion = 33n;
    newer.spec!.port = 3001;
    const api = show(<PublicEndpointDetail name="docs" />, { server: { endpoints: [newer, endpoints()[1]] } });
    await screen.findByText("Summary");
    click("Make public");
    click("Make public");
    await waitFor(() => expect(methods(api)).toEqual(["GetPublicEndpoint", "ApplyPublicEndpoint"]));
    const want = clone(PublicEndpointSchema, newer);
    want.spec!.access = PublicEndpointAccess.PUBLIC;
    expect(equals(PublicEndpointSchema, lastApply<ApplyPublicEndpointRequest>(api).publicEndpoint!, want)).toBe(true);
    expect(lastApply<ApplyPublicEndpointRequest>(api).createOnly).toBe(false);
    // The server took it: stored at the next version.
    await waitFor(() => expect(api.server.endpoints[0].metadata?.resourceVersion).toBe(34n));
  });

  it("makes a public endpoint private", async () => {
    const pub = clone(PublicEndpointSchema, endpoints()[0]);
    pub.spec!.access = PublicEndpointAccess.PUBLIC;
    const api = show(<PublicEndpointDetail name="docs" />, {
      listPublicEndpoints: () => ({ items: [pub] }),
      server: { endpoints: [clone(PublicEndpointSchema, pub)] },
    });
    await screen.findByText("Summary");
    click("Make private");
    click("Make private");
    await waitFor(() => expect(methods(api)).toEqual(["GetPublicEndpoint", "ApplyPublicEndpoint"]));
    const want = clone(PublicEndpointSchema, pub);
    want.spec!.access = PublicEndpointAccess.PRIVATE;
    expect(equals(PublicEndpointSchema, lastApply<ApplyPublicEndpointRequest>(api).publicEndpoint!, want)).toBe(true);
    expect(lastApply<ApplyPublicEndpointRequest>(api).createOnly).toBe(false);
    await waitFor(() => expect(api.server.endpoints[0].spec?.access).toBe(PublicEndpointAccess.PRIVATE));
  });

  it("shows a refused access change in the dialog", async () => {
    show(<PublicEndpointDetail name="docs" />, { apply: refuse(Code.PermissionDenied, "may not edit endpoints") });
    await screen.findByText("Summary");
    click("Make public");
    click("Make public");
    expect(await screen.findByText("Making the endpoint public failed")).toBeTruthy();
    expect(screen.getByText(/may not edit endpoints/)).toBeTruthy();
  });

  it("deletes an endpoint, and shows a refusal in the dialog", async () => {
    const api = show(<PublicEndpointDetail name="docs" />, { del: refuse(Code.PermissionDenied, "may not delete endpoints") });
    await screen.findByText("Summary");
    await confirmDelete("docs");
    expect(await screen.findByText(/may not delete endpoints/)).toBeTruthy();
    expect(writes(api)).toEqual([{ method: "DeletePublicEndpoint", request: expect.objectContaining({ project: "demo", name: "docs" }) }]);
    expect(navigate).not.toHaveBeenCalled();
  });

  it("goes to the list after a delete the API took", async () => {
    show(<PublicEndpointDetail name="docs" />);
    await screen.findByText("Summary");
    await confirmDelete("docs");
    await waitFor(() => expect(navigate).toHaveBeenCalledWith({ to: "/projects/$project/public-endpoints", params: { project: "demo" } }));
  });
});
