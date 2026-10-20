// @vitest-environment jsdom

// Every change the network pages make (security groups have their own:
// security-group-pages.test.tsx), as the RPCs it sends and the whole
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
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { AddressDetail } from "@/pages/addresses";
import { CreateAddress } from "@/pages/create-address";
import { CreateNetwork } from "@/pages/create-network";
import { CreatePublicEndpoint } from "@/pages/create-public-endpoint";
import { NetworkDetail } from "@/pages/networks";
import { PublicEndpointDetail } from "@/pages/public-endpoints";
import { never, polyfillDialog, withApi } from "@/test/harness";
import { addresses, endpoints, networkApi, type NetworkApi, type Overrides } from "@/test/network-api";

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
  /** The list's button that opens a create dialog; unset for a create page. */
  open?: string;
  /** The name field's label. */
  name?: string;
  submit: string;
  taken: string;
  hide: keyof Overrides;
  fill?: () => Promise<void>;
};

const creates: CreateCase[] = [
  { noun: "network", page: () => <CreateNetwork />, name: "Network name", submit: "Create network", taken: "sandbox", hide: "listNetworks" },
  { noun: "address", page: () => <CreateAddress />, name: "Address name", submit: "Reserve address", taken: "spare", hide: "listAddresses" },
  {
    noun: "public endpoint",
    page: () => <CreatePublicEndpoint />,
    name: "Endpoint name",
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
    if (c.open) fireEvent.click((await screen.findAllByRole("button", { name: c.open }))[0]);
    else await screen.findByLabelText(c.name!);
    await c.fill?.();
    await waitFor(() => expect(screen.queryByText(/still loading/)).toBeNull());
    type(c.name ?? /^Name/, c.taken);
    click(c.submit);
    expect(await screen.findByText(`A ${c.noun} named ${c.taken} exists in this project.`)).toBeTruthy();
    expect(writes(api)).toHaveLength(1);
    expect(writes(api)[0].request).toMatchObject({ createOnly: true });
    expect(api.server).toEqual(before);
    expect(navigate).not.toHaveBeenCalled();
  });
});

describe("networks", () => {
  it("creates a network with one create-only Apply of exactly the form's object, then opens it in the page's place", async () => {
    const api = show(<CreateNetwork />);
    await screen.findByLabelText("Network name");
    await waitFor(() => expect(screen.queryByText(/still loading/)).toBeNull());
    type("Network name", "db");
    type("IPv4 range", "10.30.0.0/28");
    expect(screen.getByText(/hangar apply --create-only -p demo -f db\.yaml/)).toBeTruthy();
    click("Create network");
    await waitFor(() => expect(navigate).toHaveBeenCalledWith({ to: "/projects/$project/networks/$name", params: { project: "demo", name: "db" }, replace: true }));
    expect(methods(api)).toEqual(["ApplyNetwork"]);
    expect(lastApply<ApplyNetworkRequest>(api).createOnly).toBe(true);
    const want = create(NetworkSchema, { metadata: { name: "db", project: "demo" }, spec: { ipv4Cidr: "10.30.0.0/28" } });
    expect(equals(NetworkSchema, lastApply<ApplyNetworkRequest>(api).network!, want)).toBe(true);
  });

  it("sends nothing while the networks load", async () => {
    const api = show(<CreateNetwork />, { listNetworks: () => never() });
    await screen.findByLabelText("Network name");
    type("Network name", "db");
    expect(await screen.findByText("The project's networks are still loading.")).toBeTruthy();
    expect(button("Loading names…").disabled).toBe(true);
    expect(writes(api)).toEqual([]);
  });

  it("sends nothing for a name the project has", async () => {
    const api = show(<CreateNetwork />);
    await screen.findByLabelText("Network name");
    type("Network name", "default");
    expect(await screen.findByText("A network named default exists in this project.")).toBeTruthy();
    expect(button("Create network").disabled).toBe(true);
    fireEvent.submit(button("Create network").closest("form")!);
    expect(writes(api)).toEqual([]);
  });

  it("shows the API's refusal in the form", async () => {
    show(<CreateNetwork />, { apply: refuse(Code.PermissionDenied, "may not apply networks") });
    await screen.findByLabelText("Network name");
    await waitFor(() => expect(screen.queryByText(/still loading/)).toBeNull());
    type("Network name", "db");
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

  it("says a network a file share is on is not deleted, and offers no Delete", async () => {
    const api = show(<NetworkDetail name="sandbox" />, {
      listFileShares: () => ({ items: [{ metadata: { name: "cache", project: "demo" }, spec: { networks: ["default", "sandbox"], size: "10Gi" } }] }),
    });
    await screen.findByText("Summary");
    await waitFor(() => expect(screen.queryAllByLabelText("Loading")).toHaveLength(0));
    click("Delete");
    type("Name, to confirm", "sandbox");
    expect(await screen.findByText("1 file share is on this network.")).toBeTruthy();
    expect(button("Delete").disabled).toBe(true);
    expect(writes(api)).toEqual([]);
  });

  it("keeps Delete off while whether a file share is on the network is not known", async () => {
    show(<NetworkDetail name="sandbox" />, { listFileShares: refuse(Code.Unavailable, "down") });
    await screen.findByText("Summary");
    await waitFor(() => expect(screen.queryAllByLabelText("Loading")).toHaveLength(0));
    click("Delete");
    type("Name, to confirm", "sandbox");
    expect(await screen.findByText("The project's file shares did not load, so whether one is on this network is not known.")).toBeTruthy();
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

describe("addresses", () => {
  it("reserves an unbound address", async () => {
    const api = show(<CreateAddress />);
    await screen.findByLabelText("Address name");
    await waitFor(() => expect(screen.queryByText(/still loading/)).toBeNull());
    type("Address name", "edge");
    expect(screen.getByText(/hangar apply --create-only -p demo -f edge\.yaml/)).toBeTruthy();
    click("Reserve address");
    await waitFor(() => expect(methods(api)).toEqual(["ApplyAddress"]));
    expect(lastApply<ApplyAddressRequest>(api).createOnly).toBe(true);
    const want = create(AddressSchema, { metadata: { name: "edge", project: "demo" }, spec: {} });
    expect(equals(AddressSchema, lastApply<ApplyAddressRequest>(api).address!, want)).toBe(true);
    expect(navigate).toHaveBeenCalledWith({ to: "/projects/$project/addresses/$name", params: { project: "demo", name: "edge" }, replace: true });
  });

  it("reserves an address bound to the interface chosen", async () => {
    const api = show(<CreateAddress />);
    await screen.findByRole("option", { name: /api · eth0/ });
    await waitFor(() => expect(screen.queryByText(/still loading/)).toBeNull());
    fireEvent.change(screen.getByRole("combobox", { name: "Interface" }), { target: { value: "Container/api/eth0" } });
    type("Address name", "edge");
    click("Reserve address");
    await waitFor(() => expect(methods(api)).toEqual(["ApplyAddress"]));
    const want = create(AddressSchema, {
      metadata: { name: "edge", project: "demo" },
      spec: { target: { object: { case: "container", value: "api" }, interface: "eth0" } },
    });
    expect(equals(AddressSchema, lastApply<ApplyAddressRequest>(api).address!, want)).toBe(true);
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
    const api = show(<CreatePublicEndpoint />);
    await screen.findByRole("option", { name: "api (container)" });
    await waitFor(() => expect(screen.queryByText(/still loading/)).toBeNull());
    type("Endpoint name", "blog");
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
    const api = show(<CreatePublicEndpoint />);
    await screen.findByRole("option", { name: "api (container)" });
    type("Endpoint name", "blog");
    type("Port", "70000");
    expect(screen.getByText(/The port is not between 1 and 65535/)).toBeTruthy();
    expect(button("Create public endpoint").disabled).toBe(true);
    expect(writes(api)).toEqual([]);
  });

  it("shows no hostname it cannot know", async () => {
    show(<CreatePublicEndpoint />);
    await screen.findByLabelText("Endpoint name");
    type("Endpoint name", "blog");
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
