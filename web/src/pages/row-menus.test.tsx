// @vitest-environment jsdom

// Every list's row menu, in its desktop table and its phone list, holds
// exactly its object page's actions: the head's, with Edit labels (which
// the page draws beside the labels) before Delete. Both are gated by the
// same answer of TestPermissions, so an action the answer does not hold is
// in neither, and a row the caller may do nothing to has no menu. An object
// being deleted offers nothing that changes it.

import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import type { ObjectMeta } from "@hangar/api/hangar/meta/v1/meta_pb";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { AddressDetail, AddressList } from "@/pages/addresses";
import { DiskDetail } from "@/pages/disk-detail";
import { DiskList } from "@/pages/disk-list";
import { FileShareDetail } from "@/pages/file-share-detail";
import { FileShareList } from "@/pages/file-share-list";
import { ImageDetail } from "@/pages/image-detail";
import { ImageList } from "@/pages/image-list";
import { MachineDetail } from "@/pages/machine-detail";
import { MachineList } from "@/pages/machine-list";
import { NetworkDetail, NetworkList } from "@/pages/networks";
import { PublicEndpointDetail, PublicEndpointList } from "@/pages/public-endpoints";
import { SecretDetail, SecretList } from "@/pages/secrets";
import { SecurityGroupDetail, SecurityGroupList } from "@/pages/security-groups";
import { ServiceTokenDetail, ServiceTokenList } from "@/pages/service-tokens";
import { credentialsApi, secret, token } from "@/test/credentials";
import { container, share, sharesApi, vm } from "@/test/file-shares";
import { polyfillDialog, withApi, type Api } from "@/test/harness";
import { networkApi, networks } from "@/test/network-api";
import { disk, image, storageApi } from "@/test/storage";

vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  useNavigate: () => vi.fn(),
  Link: (await import("@/test/fake-link")).FakeLink,
}));

beforeAll(() => {
  polyfillDialog();
  // A machine's page asks whether the person wants less motion.
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {} })) as unknown as typeof window.matchMedia;
});
afterEach(cleanup);

/** Which permissions the caller holds; each case's API answers those of what it is asked. */
type Held = (p: string) => boolean;

type Case = {
  kind: string;
  api: (held: Held) => Api;
  list: () => ReactNode;
  page: () => ReactNode;
  /** The row whose menu is opened. */
  row: string;
  /** Permissions that hold some of the kind's actions and not others, and the actions they leave out. */
  partial: { held: Held; hidden: string[] };
  /** The list has one table alone, with no phone list. */
  tableOnly?: boolean;
};

const all: Held = () => true;
const filter = (held: Held) => async () => PERMS.filter(held);
// The permissions any of these pages asks for.
const PERMS = [
  "storage.disks.apply",
  "storage.disks.delete",
  "storage.images.apply",
  "storage.fileshares.apply",
  "storage.fileshares.delete",
  "network.networks.apply",
  "network.networks.delete",
  "network.securitygroups.apply",
  "network.securitygroups.delete",
  "network.addresses.apply",
  "network.addresses.delete",
  "network.publicendpoints.apply",
  "network.publicendpoints.delete",
  "secrets.secrets.apply",
  "secrets.secrets.delete",
  "iam.servicetokens.apply",
  "iam.servicetokens.delete",
  "compute.containers.apply",
  "compute.containers.delete",
  "compute.virtualmachines.apply",
  "compute.virtualmachines.delete",
];
const net = (held: Held) => networkApi({ permissions: (req) => ({ permissions: req.permissions.filter(held) }) });
const only =
  (...ps: string[]): Held =>
  (p) =>
    ps.includes(p);

const cases: Case[] = [
  {
    kind: "disks",
    api: (held) => storageApi({ disks: async () => ({ items: [disk("pg")] }), get: () => disk("pg"), perms: filter(held) }),
    list: () => <DiskList />,
    page: () => <DiskDetail name="pg" />,
    row: "pg",
    partial: { held: only("storage.disks.apply"), hidden: ["Delete"] },
  },
  {
    kind: "file shares",
    api: (held) => sharesApi({ shares: [share("media", { networks: ["default"] })], perms: filter(held) }),
    list: () => <FileShareList />,
    page: () => <FileShareDetail name="media" />,
    row: "media",
    partial: { held: only("storage.fileshares.delete"), hidden: ["Edit networks", "Grow", "Edit labels"] },
    tableOnly: true,
  },
  {
    kind: "networks",
    api: net,
    list: () => <NetworkList />,
    page: () => <NetworkDetail name="sandbox" />,
    row: "sandbox",
    partial: { held: only("network.networks.apply"), hidden: ["Delete"] },
  },
  {
    kind: "security groups",
    api: net,
    list: () => <SecurityGroupList />,
    page: () => <SecurityGroupDetail name="web" />,
    row: "web",
    partial: { held: only("network.securitygroups.delete"), hidden: ["Edit rules", "Edit labels"] },
  },
  {
    kind: "addresses",
    api: net,
    list: () => <AddressList />,
    page: () => <AddressDetail name="api" />,
    row: "api",
    partial: { held: only("network.addresses.apply"), hidden: ["Delete"] },
  },
  {
    kind: "public endpoints",
    api: net,
    list: () => <PublicEndpointList />,
    page: () => <PublicEndpointDetail name="docs" />,
    row: "docs",
    partial: { held: only("network.publicendpoints.delete"), hidden: ["Make public", "Edit labels"] },
  },
  {
    kind: "secrets",
    api: (held) => credentialsApi({ secrets: async () => ({ items: [secret("hub")] }), perms: filter(held) }),
    list: () => <SecretList />,
    page: () => <SecretDetail name="hub" />,
    row: "hub",
    partial: { held: only("secrets.secrets.apply"), hidden: ["Delete"] },
  },
  {
    kind: "service tokens",
    api: (held) => credentialsApi({ tokens: async () => ({ items: [token("ci")] }), perms: filter(held) }),
    list: () => <ServiceTokenList />,
    page: () => <ServiceTokenDetail name="ci" />,
    row: "ci",
    partial: { held: only("iam.servicetokens.delete"), hidden: ["Rotate", "Edit labels"] },
  },
  {
    kind: "containers",
    api: (held) => sharesApi({ containers: [container("web", ["default"])], perms: filter(held) }),
    list: () => <MachineList kind="Container" />,
    page: () => <MachineDetail kind="Container" name="web" />,
    row: "web",
    partial: { held: only("compute.containers.apply"), hidden: ["Delete"] },
  },
  {
    kind: "virtual machines",
    api: (held) => sharesApi({ vms: [vm("db", ["default"])], perms: filter(held) }),
    list: () => <MachineList kind="VirtualMachine" />,
    page: () => <MachineDetail kind="VirtualMachine" name="db" />,
    row: "db",
    partial: { held: only("compute.virtualmachines.delete"), hidden: ["Edit", "Start", "Edit labels"] },
  },
  {
    kind: "images",
    api: (held) => storageApi({ images: async () => ({ items: [image("debian-13")] }), perms: filter(held) }),
    list: () => <ImageList />,
    page: () => <ImageDetail name="debian-13" />,
    row: "debian-13",
    partial: { held: only("storage.disks.apply"), hidden: ["Edit labels"] },
  },
];

const names = (els: HTMLElement[]) => els.map((e) => e.textContent?.trim() ?? "");

/** Waits until the API has answered every TestPermissions it was asked, so what the answers decide is drawn. */
async function answered(api: Api) {
  await waitFor(() => {
    const asked = api.client
      .getQueryCache()
      .findAll()
      .filter((q) => JSON.stringify(q.queryKey).includes("TestPermissions"));
    expect(asked.length).toBeGreaterThan(0);
    expect(asked.every((q) => q.state.status !== "pending")).toBe(true);
  });
}

/** The page's actions: its head's, and Edit labels where it draws it beside the labels, in the menu's order. */
async function pageActions(c: Case, held: Held): Promise<string[]> {
  const api = c.api(held);
  render(c.page(), { wrapper: withApi(api) });
  await screen.findAllByText(c.row);
  await answered(api);
  const group = screen.queryByRole("group", { name: "Actions" });
  const head = group ? names([...group.querySelectorAll<HTMLElement>("button, a")]) : [];
  const labels = screen.queryAllByRole("button", { name: "Edit labels" }).length > 0;
  cleanup();
  if (!labels) return head;
  const at = head.indexOf("Delete");
  return at < 0 ? [...head, "Edit labels"] : [...head.slice(0, at), "Edit labels", ...head.slice(at)];
}

/**
 * The row's menu, opened in the desktop table and in the phone list, each
 * of which must have one and the same; undefined when the row has none.
 */
async function menuActions(c: Case, held: Held): Promise<string[] | undefined> {
  const api = c.api(held);
  render(c.list(), { wrapper: withApi(api) });
  await screen.findAllByText(c.row);
  await answered(api);
  const buttons = screen.queryAllByRole("button", { name: `Actions for ${c.row}` });
  if (buttons.length === 0) {
    cleanup();
    return undefined;
  }
  // Both layouts are in the document; CSS shows one.
  const table = buttons.filter((b) => b.closest("table"));
  const phone = buttons.filter((b) => !b.closest("table"));
  expect(table).toHaveLength(1);
  expect(phone).toHaveLength(c.tableOnly ? 0 : 1);
  const menus = [...table, ...phone].map((b) => {
    fireEvent.click(b);
    const items = names(within(screen.getByRole("menu")).getAllByRole("menuitem"));
    fireEvent.keyDown(document, { key: "Escape" });
    return items;
  });
  cleanup();
  if (menus.length === 2) expect(menus[1]).toEqual(menus[0]);
  return menus[0];
}

describe.each(cases)("the $kind list", (c) => {
  it("holds in each row's menu exactly the actions of the object's page", async () => {
    const page = await pageActions(c, all);
    expect(page.length).toBeGreaterThan(0);
    expect(await menuActions(c, all)).toEqual(page);
  });

  it("leaves out of both an action whose permission is not held", async () => {
    const page = await pageActions(c, c.partial.held);
    const menu = await menuActions(c, c.partial.held);
    expect(menu).toEqual(page);
    for (const h of c.partial.hidden) expect(menu).not.toContain(h);
    expect(menu?.length).toBeGreaterThan(0);
  });

  it("draws no menu on a row the caller may do nothing to", async () => {
    expect(await menuActions(c, () => false)).toBeUndefined();
  });
});

describe("an object being deleted", () => {
  const deleting = <T extends { metadata?: ObjectMeta }>(o: T): T => {
    o.metadata!.deleteTime = timestampFromDate(new Date());
    return o;
  };

  it("offers no Edit labels on its page, and draws the head's actions off", async () => {
    const pg = deleting(disk("pg"));
    render(<DiskDetail name="pg" />, { wrapper: withApi(storageApi({ disks: async () => ({ items: [pg] }), perms: filter(all) })) });
    const grow = await screen.findByRole("button", { name: "Grow" });
    expect((grow as HTMLButtonElement).disabled).toBe(true);
    expect(screen.queryByRole("button", { name: "Edit labels" })).toBeNull();
  });

  it("offers no Edit labels on a network's page, nor anything in its row's menu", async () => {
    const api = () =>
      networkApi({
        listNetworks: () => ({ items: networks().map((n) => (n.metadata?.name === "sandbox" ? deleting(n) : n)) }),
      });
    render(<NetworkDetail name="sandbox" />, { wrapper: withApi(api()) });
    expect(((await screen.findByRole("button", { name: "Delete" })) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.queryByRole("button", { name: "Edit labels" })).toBeNull();
    cleanup();
    render(<NetworkList />, { wrapper: withApi(api()) });
    fireEvent.click((await screen.findAllByRole("button", { name: "Actions for sandbox" }))[0]);
    expect(within(screen.getByRole("menu")).queryAllByRole("menuitem")).toHaveLength(0);
  });
});
