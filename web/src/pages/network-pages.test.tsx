// @vitest-environment jsdom

// The network pages' states: each list and each object's page loading,
// failed, empty or not found, and full.

import { Code, ConnectError } from "@connectrpc/connect";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { AddressDetail, AddressList } from "@/pages/addresses";
import { NetworkDetail, NetworkList } from "@/pages/networks";
import { PortForwardDetail, PortForwardList } from "@/pages/port-forwards";
import { PublicEndpointDetail, PublicEndpointList } from "@/pages/public-endpoints";
import { SecurityGroupDetail, SecurityGroupList } from "@/pages/security-groups";
import { MachinePhase } from "@hangar/api/hangar/compute/v1alpha1/machine_pb";
import { never, polyfillDialog, withApi } from "@/test/harness";
import { containersNamed, machineOf, networkApi, type Overrides } from "@/test/network-api";

vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  useNavigate: () => vi.fn(),
  Link: ({ children, className }: { children: ReactNode; className?: string }) => <a className={className}>{children}</a>,
}));

beforeAll(polyfillDialog);
afterEach(cleanup);

const unavailable = () => {
  throw new ConnectError("connection refused", Code.Unavailable);
};

function show(page: ReactNode, o: Overrides = {}) {
  const api = networkApi(o);
  render(page, { wrapper: withApi(api) });
  return api;
}

type ListCase = {
  name: string;
  page: () => ReactNode;
  list: "listNetworks" | "listSecurityGroups" | "listAddresses" | "listPublicEndpoints" | "listPortForwards";
  failed: string;
  empty: string;
  full: string[];
  /** Other lists to empty for the empty state. */
  alsoEmpty?: (keyof Overrides)[];
};

const lists: ListCase[] = [
  { name: "networks", page: () => <NetworkList />, list: "listNetworks", failed: "The networks did not load", empty: "No networks", full: ["default", "sandbox", "10.0.0.1"] },
  {
    name: "security groups",
    page: () => <SecurityGroupList />,
    list: "listSecurityGroups",
    failed: "The security groups did not load",
    empty: "No security groups",
    full: ["web"],
    alsoEmpty: ["listNetworks"],
  },
  { name: "addresses", page: () => <AddressList />, list: "listAddresses", failed: "The addresses did not load", empty: "No addresses", full: ["spare", "2001:db8::90e4"] },
  {
    name: "public endpoints",
    page: () => <PublicEndpointList />,
    list: "listPublicEndpoints",
    failed: "The public endpoints did not load",
    empty: "No public endpoints",
    full: ["docs", "port of api"],
  },
  {
    name: "port forwards",
    page: () => <PortForwardList />,
    list: "listPortForwards",
    failed: "The port forwards did not load",
    empty: "No port forwards",
    full: ["api-ssh", "198.51.100.3:23817", "port 18466, no address yet", "NoSharedIPv4", "Deleting"],
  },
];

describe.each(lists)("the $name list", (c) => {
  it("shows skeletons while it loads", async () => {
    show(c.page(), { [c.list]: () => never() });
    expect(await screen.findByLabelText("Loading")).toBeTruthy();
  });

  it("shows the API's error in place, and asks again on Retry", async () => {
    const api = show(c.page(), { [c.list]: unavailable });
    expect(await screen.findByText(c.failed)).toBeTruthy();
    expect(screen.getAllByText(/connection refused/).length).toBeGreaterThan(0);
    const method = c.list.replace(/^l/, "L");
    const before = api.calls.filter((m) => m === method).length;
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await vi.waitFor(() => expect(api.calls.filter((m) => m === method).length).toBeGreaterThan(before));
  });

  it("says when there is none", async () => {
    const empty = Object.fromEntries([c.list, ...(c.alsoEmpty ?? [])].map((k) => [k, () => ({ items: [] })]));
    show(c.page(), empty);
    expect(await screen.findByText(c.empty)).toBeTruthy();
  });

  it("lists the project's objects", async () => {
    show(c.page());
    for (const text of c.full) expect((await screen.findAllByText(text, { exact: false })).length).toBeGreaterThan(0);
  });
});

describe("the security groups list", () => {
  it("lists each network's built-in default group, with what it does", async () => {
    show(<SecurityGroupList />);
    expect(await screen.findAllByText("built in")).toHaveLength(4); // phone list and table, two networks
    expect(screen.getAllByText("its own members").length).toBe(2);
  });
});

type DetailCase = {
  name: string;
  page: (n: string) => ReactNode;
  list: ListCase["list"];
  present: string;
  failed: string;
  missing: string;
  full: string[];
};

const details: DetailCase[] = [
  {
    name: "network",
    page: (n) => <NetworkDetail name={n} />,
    list: "listNetworks",
    present: "default",
    failed: "The network did not load",
    missing: "No such network",
    full: ["10.0.0.1", "Built in. Lets in its own members and lets everything out.", "10.0.0.2"],
  },
  {
    name: "security group",
    page: (n) => <SecurityGroupDetail name={n} />,
    list: "listSecurityGroups",
    present: "web",
    failed: "The security group did not load",
    missing: "No such security group",
    full: [
      "Lets in TCP ports 80 and 443 from any IPv6 address.",
      "Lets out UDP port 53 to 192.0.2.53/32.",
      "A member sends only what some outbound rule of its groups lets out.",
      "Members",
    ],
  },
  {
    name: "address",
    page: (n) => <AddressDetail name={n} />,
    list: "listAddresses",
    present: "api",
    failed: "The address did not load",
    missing: "No such address",
    full: ["2001:db8::77b2", "api-x1"],
  },
  {
    name: "public endpoint",
    page: (n) => <PublicEndpointDetail name={n} />,
    list: "listPublicEndpoints",
    present: "docs",
    failed: "The public endpoint did not load",
    missing: "No such public endpoint",
    full: ["docs-demo.example.com", "plain HTTP on its first interface, eth0"],
  },
  {
    name: "port forward",
    page: (n) => <PortForwardDetail name={n} />,
    list: "listPortForwards",
    present: "api-ssh",
    failed: "The port forward did not load",
    missing: "No such port forward",
    full: ["198.51.100.3:23817", "ssh -p 23817 user@198.51.100.3", "node-1", "hangar get pf api-ssh -p demo"],
  },
];

describe.each(details)("a $name's page", (c) => {
  it("shows skeletons while it loads", async () => {
    show(c.page(c.present), { [c.list]: () => never() });
    expect((await screen.findAllByLabelText("Loading")).length).toBeGreaterThan(0);
  });

  it("shows the API's error in place", async () => {
    show(c.page(c.present), { [c.list]: unavailable });
    expect(await screen.findByText(c.failed)).toBeTruthy();
  });

  it("says when the project has none of the name", async () => {
    show(c.page("nope"));
    expect(await screen.findByText(c.missing)).toBeTruthy();
  });

  it("shows the object and its equivalent CLI", async () => {
    show(c.page(c.present));
    for (const text of c.full) expect((await screen.findAllByText(text, { exact: false })).length).toBeGreaterThan(0);
    expect(screen.getByRole("tab", { name: "CLI" })).toBeTruthy();
  });
});

describe("what comes from the project's machines", () => {
  it("shows skeletons for counts and interfaces while the machines load", async () => {
    show(<NetworkDetail name="default" />, { listMachines: () => never() });
    await screen.findByText("Summary");
    expect(screen.queryByText(/machines?$/)).toBeNull();
    expect(screen.queryByText("No machine has an interface on this network.")).toBeNull();
    expect(screen.getAllByLabelText("Loading").length).toBeGreaterThan(0);
  });

  it("says the machines did not load, rather than none, on a network's page", async () => {
    show(<NetworkDetail name="sandbox" />, { listMachines: unavailable });
    expect(await screen.findByText("The project's machines did not load")).toBeTruthy();
    expect(screen.queryByText("No machine has an interface on this network.")).toBeNull();
    expect(screen.getByText("unknown")).toBeTruthy();
  });

  it("says the machines did not load on the lists, and shows no count", async () => {
    show(<NetworkList />, { listMachines: unavailable });
    expect(await screen.findByText(/The project's machines did not load, so which interfaces/)).toBeTruthy();
    expect(screen.getAllByText("unknown").length).toBeGreaterThan(0);
    cleanup();
    show(<SecurityGroupList />, { listMachines: unavailable });
    expect(await screen.findByText(/The project's machines did not load, so which interfaces/)).toBeTruthy();
    expect(screen.queryByText(/^0$/)).toBeNull();
  });

  it("shows a group's members as loading, then as loaded", async () => {
    show(<SecurityGroupDetail name="web" />);
    await screen.findByText("Inbound");
    expect(await screen.findAllByText("1 member")).toHaveLength(1);
  });
});

describe("counts per network", () => {
  const machines: Overrides = {
    listContainers: () => ({ items: containersNamed("api", "runner", "worker") }),
    listMachines: () => ({
      items: [machineOf("api", "default", ["default", "web"]), machineOf("runner", "sandbox", ["default"]), machineOf("worker", "sandbox", ["default"])],
    }),
  };

  it("counts each network's machines alone", async () => {
    show(<NetworkList />, machines);
    expect(await screen.findAllByText("2 machines")).toHaveLength(1);
    expect(screen.getAllByText("1 machine")).toHaveLength(1);
  });

  it("counts each built-in group's members on its own network alone", async () => {
    show(<SecurityGroupList />, machines);
    expect(await screen.findAllByText("2 members")).toHaveLength(1);
    expect(screen.getAllByText("1 member")).toHaveLength(2);
  });
});

describe("a group whose network the project lacks", () => {
  it("is listed under that network, stated as missing, with no built-in row for it", async () => {
    show(<SecurityGroupList />, {
      listSecurityGroups: () => ({ items: [{ metadata: { name: "orphan" }, spec: { network: "gone", ingress: [] } }] }),
    });
    expect((await screen.findAllByText("orphan")).length).toBe(2);
    expect(screen.getAllByText("network gone does not exist").length).toBe(2);
    expect(screen.getAllByText("built in")).toHaveLength(4); // default and sandbox only
  });

  it("is stated as missing on the group's page", async () => {
    show(<SecurityGroupDetail name="orphan" />, {
      listSecurityGroups: () => ({ items: [{ metadata: { name: "orphan" }, spec: { network: "gone" } }] }),
    });
    expect(await screen.findByText("network gone does not exist")).toBeTruthy();
  });
});

describe("a port forward's page", () => {
  it("warns when no group of the target's interface lets its traffic in, naming the groups to edit", async () => {
    show(<PortForwardDetail name="api-dns" />);
    expect(await screen.findByText("No security group of api · eth0 lets in UDP port 53")).toBeTruthy();
    expect(screen.getByText("Edit rules of web")).toBeTruthy();
    expect(screen.getAllByText("No security group lets it in").length).toBeGreaterThan(0);
    expect(screen.getByText("nc -u 198.51.100.3 29405")).toBeTruthy();
  });

  it("says a node has no shared IPv4 address yet, with the port drawn and no command", async () => {
    show(<PortForwardDetail name="api-nossh" />);
    expect((await screen.findAllByText(/^Node node-2 has no shared IPv4 address yet/)).length).toBeGreaterThan(0);
    expect(screen.getAllByText("port 18466, no address yet").length).toBeGreaterThan(0);
    expect(screen.queryByText(/^ssh /)).toBeNull();
  });

  it("says a forward being deleted waits for its node to close the port, and offers no change", async () => {
    show(<PortForwardDetail name="old" />);
    expect(await screen.findByText("old is being deleted")).toBeTruthy();
    expect(screen.getAllByText("Its port is released once node node-1 has closed it.").length).toBeGreaterThan(0);
    for (const b of screen.getAllByRole("button", { name: /Retarget|Delete/ })) expect((b as HTMLButtonElement).disabled).toBe(true);
    expect(screen.queryByText("No security group lets it in")).toBeNull();
  });

  it("says a Ready forward's machine is not running, with its state, and leaves the light as it is", async () => {
    const starting = machineOf("api", "default", ["default", "web"]);
    starting.status!.phase = MachinePhase.CREATING;
    show(<PortForwardDetail name="api-ssh" />, { listMachines: () => ({ items: [starting] }) });
    // The head and the Connect card.
    expect(await screen.findAllByText(/is not running · Starting/)).toHaveLength(2);
    expect(screen.getAllByText("Ready").length).toBeGreaterThan(0);
    cleanup();
    show(<PortForwardList />, { listMachines: () => ({ items: [starting] }) });
    // api-ssh and api-dns are Ready, each on the phone list and the table; api-nossh is not Ready and old is being deleted.
    expect(await screen.findAllByText(/is not running · Starting/)).toHaveLength(4);
  });

  it("says nothing of the machine while it runs", async () => {
    show(<PortForwardDetail name="api-ssh" />);
    await screen.findByText("Summary");
    await screen.findAllByText("api-x1");
    expect(screen.queryByText(/is not running/)).toBeNull();
  });

  it("states the plan's limit under the list once it is reached", async () => {
    show(<PortForwardList />, { getProject: () => ({ project: { metadata: { name: "demo" }, status: { limits: { portForwards: 4 }, usage: { portForwards: 4 } } } }) });
    expect(await screen.findByText(/as many as the plan allows, 4/)).toBeTruthy();
  });
});

describe("a public endpoint a container made", () => {
  it("names the container and offers no change or delete", async () => {
    show(<PublicEndpointDetail name="api-http" />);
    const made = await screen.findByText("Made by");
    expect(within(made.parentElement!).getByText("api")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Delete|Make public/ })).toBeNull();
  });
});
