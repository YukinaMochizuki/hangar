// @vitest-environment jsdom

// Which controls each network page shows, by AccountService.TestPermissions:
// each page asks once, at its project, for the permissions its controls
// need; a control shows only when its permission is known to be held, and
// while the answer is pending, or when it failed, it is hidden and nothing
// is said about it.

import { Code, ConnectError } from "@connectrpc/connect";
import type { TestPermissionsRequest } from "@hangar/api/hangar/iam/v1alpha1/account_pb";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { AddressDetail, AddressList } from "@/pages/addresses";
import { NetworkDetail, NetworkList } from "@/pages/networks";
import { PublicEndpointDetail, PublicEndpointList } from "@/pages/public-endpoints";
import { SecurityGroupDetail, SecurityGroupList } from "@/pages/security-groups";
import { never, polyfillDialog, withApi } from "@/test/harness";
import { networkApi, type Overrides } from "@/test/network-api";

vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  useNavigate: () => vi.fn(),
  Link: (await import("@/test/fake-link")).FakeLink,
}));

beforeAll(polyfillDialog);
afterEach(cleanup);

function show(page: ReactNode, o: Overrides = {}) {
  const api = networkApi(o);
  render(page, { wrapper: withApi(api) });
  return api;
}

type Case = {
  name: string;
  page: () => ReactNode;
  /** What the page shows once its data is in, permissions aside. */
  ready: string;
  /** The controls, by the permission each needs. */
  controls: Record<string, string[]>;
};

const cases: Case[] = [
  { name: "networks list", page: () => <NetworkList />, ready: "sandbox", controls: { "network.networks.apply": ["New network"] } },
  {
    name: "network page",
    page: () => <NetworkDetail name="sandbox" />,
    ready: "Summary",
    controls: { "network.networks.delete": ["Delete"], "storage.fileshares.apply": ["New file share"] },
  },
  {
    name: "security groups list",
    page: () => <SecurityGroupList />,
    ready: "web",
    controls: { "network.securitygroups.apply": ["New security group"] },
  },
  {
    name: "security group page",
    page: () => <SecurityGroupDetail name="web" />,
    ready: "Inbound",
    controls: { "network.securitygroups.apply": ["Edit rules"], "network.securitygroups.delete": ["Delete"] },
  },
  { name: "addresses list", page: () => <AddressList />, ready: "spare", controls: { "network.addresses.apply": ["Reserve address"] } },
  {
    name: "bound address page",
    page: () => <AddressDetail name="api" />,
    ready: "Summary",
    controls: { "network.addresses.apply": ["Move", "Unbind"], "network.addresses.delete": ["Delete"] },
  },
  {
    name: "unbound address page",
    page: () => <AddressDetail name="spare" />,
    ready: "Summary",
    controls: { "network.addresses.apply": ["Bind"], "network.addresses.delete": ["Delete"] },
  },
  {
    name: "public endpoints list",
    page: () => <PublicEndpointList />,
    ready: "docs",
    controls: { "network.publicendpoints.apply": ["New public endpoint"] },
  },
  {
    name: "public endpoint page",
    page: () => <PublicEndpointDetail name="docs" />,
    ready: "Summary",
    controls: { "network.publicendpoints.apply": ["Make public"], "network.publicendpoints.delete": ["Delete"] },
  },
];

const buttons = (c: Case) => Object.values(c.controls).flat();
// A control is a button, or a link to a create page.
const shown = (name: string) =>
  screen.queryAllByRole("button", { name: new RegExp(`^${name}$`) }).length + screen.queryAllByRole("link", { name: new RegExp(`^${name}$`) }).length > 0;
const asked = (api: ReturnType<typeof show>) => api.requests.filter((r) => r.method === "TestPermissions").map((r) => r.request as TestPermissionsRequest);

/** Whether the TestPermissions query has its answer or its failure, so what it decides is drawn. */
const answered = (api: ReturnType<typeof show>) => {
  const queries = api.client
    .getQueryCache()
    .findAll()
    .filter((q) => JSON.stringify(q.queryKey).toLowerCase().includes("testpermissions"));
  return queries.length > 0 && queries.every((q) => q.state.status !== "pending");
};

/**
 * Waits until the page is drawn and TestPermissions was asked; unless the
 * answer is held back, also until it has landed, so the assertions after it
 * see what the answer decides rather than the state before it.
 */
async function settle(c: Case, api: ReturnType<typeof show>, opts: { pending?: boolean } = {}) {
  await screen.findAllByText(c.ready);
  await waitFor(() => expect(asked(api).length).toBeGreaterThan(0));
  if (!opts.pending) await waitFor(() => expect(answered(api)).toBe(true));
}

describe.each(cases)("the $name", (c) => {
  it("asks once, at its project, for the permissions its controls need", async () => {
    const api = show(c.page());
    await settle(c, api);
    await waitFor(() => expect(asked(api)).toHaveLength(1));
    const req = asked(api)[0];
    expect(req.scope?.target).toEqual({ case: "project", value: "demo" });
    expect([...req.permissions].sort()).toEqual(Object.keys(c.controls).sort());
  });

  it("shows every control when every permission is held", async () => {
    const api = show(c.page());
    await settle(c, api);
    for (const b of buttons(c)) await waitFor(() => expect(shown(b)).toBe(true));
  });

  it("hides every control when none is held, and says nothing of it", async () => {
    const api = show(c.page(), { permissions: () => ({ permissions: [] }) });
    await settle(c, api);
    for (const b of buttons(c)) expect(shown(b)).toBe(false);
    expect(screen.queryByText(/permission/i)).toBeNull();
  });

  it("hides every control while the answer is pending", async () => {
    const api = show(c.page(), { permissions: () => never() });
    await settle(c, api, { pending: true });
    for (const b of buttons(c)) expect(shown(b)).toBe(false);
  });

  it("hides every control when asking failed, and says nothing of it", async () => {
    const api = show(c.page(), {
      permissions: () => {
        throw new ConnectError("down", Code.Unavailable);
      },
    });
    await settle(c, api);
    for (const b of buttons(c)) expect(shown(b)).toBe(false);
    expect(screen.queryByText(/down|permission/i)).toBeNull();
  });

  if (Object.keys(c.controls).length > 1) {
    it.each(Object.keys(c.controls))("shows only what %s allows when that alone is held", async (perm) => {
      const api = show(c.page(), { permissions: () => ({ permissions: [perm] }) });
      await settle(c, api);
      for (const b of c.controls[perm]) await waitFor(() => expect(shown(b)).toBe(true));
      for (const [other, bs] of Object.entries(c.controls)) if (other !== perm) for (const b of bs) expect(shown(b)).toBe(false);
    });
  }
});

describe("an empty list", () => {
  it("offers no create button without the permission", async () => {
    const api = show(<NetworkList />, { listNetworks: () => ({ items: [] }), permissions: () => ({ permissions: [] }) });
    await screen.findByText("No networks");
    await waitFor(() => expect(asked(api).length).toBeGreaterThan(0));
    await waitFor(() => expect(answered(api)).toBe(true));
    expect(shown("New network")).toBe(false);
  });

  it("offers it with the permission", async () => {
    show(<NetworkList />, { listNetworks: () => ({ items: [] }) });
    await screen.findByText("No networks");
    await waitFor(() => expect(screen.getAllByRole("link", { name: "New network" })).toHaveLength(2));
  });
});
