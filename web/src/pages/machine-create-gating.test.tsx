// @vitest-environment jsdom

import { Code, ConnectError } from "@connectrpc/connect";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { MachineList } from "@/pages/machine-list";
import { Overview } from "@/pages/overview";
import { never, polyfillDialog, withApi, type Api } from "@/test/harness";
import { paletteApi, type PaletteFake } from "@/test/palette-api";

// A Link whose href carries its search too, so a test sees which kind New machine opens on.
vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  useNavigate: () => vi.fn(),
  Link: ({ to, params, search, children, className }: { to: string; params?: Record<string, string>; search?: Record<string, string>; children?: ReactNode; className?: string }) => {
    const path = Object.entries(params ?? {}).reduce((p, [k, v]) => p.replace(`$${k}`, v), to);
    const q = search ? `?${new URLSearchParams(search)}` : "";
    return (
      <a href={path + q} className={className}>
        {children}
      </a>
    );
  },
}));

beforeAll(polyfillDialog);
afterEach(cleanup);

const containers = "compute.containers.apply";
const vms = "compute.virtualmachines.apply";
const links = (name: string) => screen.queryAllByRole("link", { name });

/** Renders the page and waits until TestPermissions was asked and the machines listed. */
async function show(page: ReactNode, f: PaletteFake): Promise<Api> {
  const api = paletteApi(f);
  render(<>{page}</>, { wrapper: withApi(api) });
  await waitFor(() => expect(api.calls).toContain("TestPermissions"));
  await waitFor(() => expect(api.calls).toContain("ListMachines"));
  return api;
}

const settled = (api: Api) =>
  waitFor(() =>
    expect(
      api.client
        .getQueryCache()
        .getAll()
        .filter((q) => JSON.stringify(q.queryKey).includes("TestPermissions"))
        .every((q) => q.state.status !== "pending"),
    ).toBe(true),
  );

describe("New container and New virtual machine on the machine lists", () => {
  it("show each where its permission is held, above the list and in the empty state", async () => {
    await show(<MachineList kind="Container" />, { listContainers: () => ({ items: [] }) });
    await waitFor(() => expect(links("New container")).toHaveLength(2));
    expect(links("New container")[0]!.getAttribute("href")).toBe("/projects/demo/new?kind=container");
    cleanup();
    await show(<MachineList kind="VirtualMachine" />, {});
    await waitFor(() => expect(links("New virtual machine")).toHaveLength(1));
    expect(links("New virtual machine")[0]!.getAttribute("href")).toBe("/projects/demo/new?kind=vm");
  });

  it("hide the one whose permission is not held", async () => {
    const api = await show(<MachineList kind="Container" />, { perms: () => [vms], listContainers: () => ({ items: [] }) });
    await settled(api);
    await screen.findByText("No containers");
    expect(links("New container")).toEqual([]);
  });

  it("hide it while the answer is not known, and say nothing", async () => {
    await show(<MachineList kind="Container" />, { perms: () => never(), listContainers: () => ({ items: [] }) });
    await screen.findByText("No containers");
    expect(links("New container")).toEqual([]);
  });

  it("hide it when the permission check failed, and say nothing of it", async () => {
    const api = await show(<MachineList kind="Container" />, {
      perms: () => Promise.reject(new ConnectError("the apiserver is unavailable", Code.Unavailable)),
      listContainers: () => ({ items: [] }),
    });
    await settled(api);
    await screen.findByText("No containers");
    expect(links("New container")).toEqual([]);
    expect(document.body.textContent).not.toMatch(/permission|unavailable/i);
  });
});

describe("New machine on the overview", () => {
  it("shows in the header and the empty state, opening on containers, where containers may be created", async () => {
    await show(<Overview />, { listContainers: () => ({ items: [] }), listVirtualMachines: () => ({ items: [] }), listMachines: () => ({ items: [] }) });
    await waitFor(() => expect(links("New machine")).toHaveLength(2));
    for (const l of links("New machine")) expect(l.getAttribute("href")).toBe("/projects/demo/new?kind=container");
  });

  it("opens on virtual machines where only those may be created", async () => {
    await show(<Overview />, { perms: () => [vms] });
    await waitFor(() => expect(links("New machine")).toHaveLength(1));
    expect(links("New machine")[0]!.getAttribute("href")).toBe("/projects/demo/new?kind=vm");
  });

  it("is hidden where neither may be created, and while the answer is not known", async () => {
    const api = await show(<Overview />, { perms: () => [] });
    await settled(api);
    await screen.findByRole("heading", { level: 1, name: "Demo" });
    expect(links("New machine")).toEqual([]);
    cleanup();
    await show(<Overview />, { perms: () => never() });
    await screen.findByRole("heading", { level: 1, name: "Demo" });
    expect(links("New machine")).toEqual([]);
  });

  it("asks for both permissions in one call", async () => {
    const api = await show(<Overview />, { perms: () => [containers, vms] });
    await waitFor(() => expect(links("New machine").length).toBeGreaterThan(0));
    const asked = api.requests.filter((r) => r.method === "TestPermissions").map((r) => (r.request as { permissions: string[] }).permissions);
    expect(asked).toContainEqual([containers, vms]);
  });
});
