// @vitest-environment jsdom

import { Code, ConnectError } from "@connectrpc/connect";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { CommandPalette } from "@/components/command-palette";
import { Topbar, type Place } from "@/components/shell";
import { useSecrets, useServiceTokens, useTokenRoles } from "@/lib/credentials";
import { useMachines } from "@/lib/machines";
import { useAddresses, useNetworks, usePublicEndpoints, useSecurityGroups } from "@/lib/network";
import { useProjects } from "@/lib/projects";
import { useDisks, useImages } from "@/lib/storage";
import { never, polyfillDialog, withApi, type Api } from "@/test/harness";
import { lists, paletteApi, type PaletteFake } from "@/test/palette-api";

const navigate = vi.fn();
vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  useNavigate: () => navigate,
  Link: (await import("@/test/fake-link")).FakeLink,
}));

beforeAll(polyfillDialog);
beforeEach(() => navigate.mockReset());
afterEach(cleanup);

const demo: Place = { kind: "project", name: "demo" };

/** The palette behind a button, as the top bar holds it: open while it is open, unmounted once closed. */
function Host({ place = demo }: { place?: Place }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button onClick={() => setOpen(true)}>Open palette</button>
      {open && <CommandPalette place={place} onClose={() => setOpen(false)} />}
    </>
  );
}

function setup(f: PaletteFake = {}, place: Place = demo): Api {
  const api = paletteApi(f);
  render(<Host place={place} />, { wrapper: withApi(api, place) });
  const opener = screen.getByRole("button", { name: "Open palette" });
  opener.focus();
  fireEvent.click(opener);
  return api;
}

const box = () => screen.getByRole("combobox");
const type = (q: string) => fireEvent.change(box(), { target: { value: q } });
const key = (k: string, o: object = {}) => fireEvent.keyDown(box(), { key: k, ...o });
/** An option's title: the first text in it, before its ID and its fact. */
const titleOf = (o: HTMLElement) => o.querySelector("span")?.textContent ?? "";
const titles = () => screen.queryAllByRole("option").map(titleOf);
const option = (title: string) => screen.getAllByRole("option").find((o) => titleOf(o) === title)!;
const findOption = (title: string) => waitFor(() => expect(titles()).toContain(title)).then(() => option(title));
const active = () => screen.getAllByRole("option").find((o) => o.getAttribute("aria-selected") === "true");

describe("CommandPalette", () => {
  it("offers the actions and the pages before anything is typed, and no object", async () => {
    setup();
    await findOption("New container");
    expect(titles()).toEqual(expect.arrayContaining(["New virtual machine", "Add member", "Switch project…", "Overview", "Disks", "Service tokens"]));
    expect(titles()).not.toContain("web");
  });

  it.each([
    ["web", "web", { to: "/projects/demo/containers/web" }],
    ["postgres", "postgres", { to: "/projects/demo/vms/postgres" }],
    ["pg-data", "pg-data", { to: "/projects/demo/disks/pg-data" }],
    ["debian", "debian-13", { to: "/projects/demo/images/debian-13" }],
    ["backend", "backend", { to: "/projects/demo/networks/backend" }],
    ["ssh", "ssh-admin", { to: "/projects/demo/security-groups/ssh-admin" }],
    ["front", "front-v6", { to: "/projects/demo/addresses/front-v6" }],
    ["site", "site-https", { to: "/projects/demo/public-endpoints/site-https" }],
    ["registry", "registry-login", { to: "/projects/demo/secrets/registry-login" }],
    ["ci-", "ci-deploy", { to: "/projects/demo/service-tokens/ci-deploy" }],
    ["grace@example", "grace@example.com", { to: "/projects/demo/members" }],
    ["ada@", "ada", { to: "/projects/demo/members" }],
    ["Staging", "Staging", { to: "/projects/$project", params: { project: "web-staging" } }],
    ["lab-1", "Edge lab", { to: "/projects/$project", params: { project: "lab-1" } }],
    ["zeta", "Zeta Research", { to: "/tenants/$tenant", params: { tenant: "zeta" } }],
    ["secr", "Secrets", { to: "/projects/demo/secrets" }],
  ])("finds %s and opens %s once the palette has closed", async (q, title, to) => {
    setup();
    type(q);
    const o = await findOption(title);
    let openWhenCalled: boolean | undefined;
    navigate.mockImplementation(() => {
      openWhenCalled = screen.queryByRole("combobox", { name: "Search or jump to" }) !== null;
    });
    fireEvent.click(o);
    expect(navigate).toHaveBeenCalledWith(to);
    expect(openWhenCalled).toBe(false);
  });

  it("says a fact beside each object", async () => {
    setup();
    const fact = async (q: string, title: string, text: string) => {
      type(q);
      expect((await findOption(title)).textContent).toContain(text);
    };
    await fact("web", "web", "Running");
    await fact("api-web", "api-web", "Stopped");
    await fact("pg-data", "pg-data", "100 GiB");
    await fact("backend", "backend", "10.1.0.0/24");
    await fact("ssh", "ssh-admin", "3 rules");
    await fact("front", "front-v6", "2001:db8::10");
    await fact("site", "site-https", "site.example.com");
    await fact("registry", "registry-login", "1 key");
    await fact("ci-", "ci-deploy", "Editor");
    await fact("ada", "ada", "Owner");
    await fact("demo", "Demo", "Acme · current");
  });

  it("ranks an exact name above a prefix, and a prefix above a match inside", async () => {
    setup();
    type("web");
    await findOption("api-web");
    await findOption("Staging");
    expect(titles()).toEqual(["web", "web-worker", "api-web", "Staging"]);
    expect(active()).toBe(option("web"));
  });

  it("moves through the results with the arrows, wrapping, and opens the active one with Enter", async () => {
    setup();
    await findOption("New container");
    expect(active()).toBe(option("New container"));
    key("ArrowDown");
    expect(active()).toBe(option("New virtual machine"));
    expect(box().getAttribute("aria-activedescendant")).toBe(option("New virtual machine").id);
    key("ArrowUp");
    key("ArrowUp");
    expect(active()).toBe(option("Service tokens"));
    key("Enter");
    expect(navigate).toHaveBeenCalledWith({ to: "/projects/demo/service-tokens" });
    expect(screen.queryByRole("combobox", { name: "Search or jump to" })).toBeNull();
  });

  it("makes the option under the pointer active", async () => {
    setup();
    fireEvent.mouseMove(await findOption("Disks"));
    expect(active()).toBe(option("Disks"));
  });

  it("is a combobox over a listbox of options, inside a modal dialog", async () => {
    setup();
    await findOption("Disks");
    const list = screen.getByRole("listbox");
    expect(box().getAttribute("aria-controls")).toBe(list.id);
    expect(box().getAttribute("aria-expanded")).toBe("true");
    expect(within(list).getAllByRole("option").length).toBe(screen.getAllByRole("option").length);
    expect(screen.getByRole("dialog", { name: "Search" })).toBeTruthy();
  });

  it("closes with Escape and gives focus back to what opened it", async () => {
    setup();
    await findOption("Disks");
    expect(document.activeElement).toBe(box());
    key("Escape");
    expect(screen.queryByRole("combobox", { name: "Search or jump to" })).toBeNull();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Open palette" }));
  });

  it("cycles Tab and Shift+Tab through what can be focused inside, and never out", async () => {
    // jsdom lays nothing out; every element is taken as drawn, Cancel included.
    const rects = vi.spyOn(HTMLElement.prototype, "getClientRects").mockReturnValue([{}] as unknown as DOMRectList);
    try {
      setup();
      await findOption("Disks");
      const cancel = screen.getByRole("button", { name: "Cancel" });
      const tab = (shiftKey = false) => fireEvent.keyDown(document.activeElement!, { key: "Tab", shiftKey });
      expect(document.activeElement).toBe(box());
      expect(tab()).toBe(false);
      expect(document.activeElement).toBe(cancel);
      // From the last, Tab wraps to the first.
      tab();
      expect(document.activeElement).toBe(box());
      // From the first, Shift+Tab wraps to the last.
      tab(true);
      expect(document.activeElement).toBe(cancel);
      tab(true);
      expect(document.activeElement).toBe(box());
    } finally {
      rects.mockRestore();
    }
  });

  it("opens nothing with an Enter that ends an IME composition", async () => {
    setup();
    await findOption("New container");
    key("Enter", { isComposing: true });
    key("Enter", { keyCode: 229 });
    expect(navigate).not.toHaveBeenCalled();
    expect(screen.getByRole("combobox", { name: "Search or jump to" })).toBeTruthy();
    key("Enter");
    expect(navigate).toHaveBeenCalledTimes(1);
  });

  it("shows what a list holds when only its refetch failed, with nothing said", async () => {
    let fail = false;
    const api = paletteApi({
      listDisks: () => (fail ? Promise.reject(new ConnectError("the apiserver is unavailable", Code.Unavailable)) : { items: [{ metadata: { name: "pg-data", project: "demo" }, spec: { size: "20Gi" }, status: { size: "20Gi" } }] }),
    });
    render(<Host />, { wrapper: withApi(api) });
    fireEvent.click(screen.getByRole("button", { name: "Open palette" }));
    type("pg-data");
    await findOption("pg-data");
    fail = true;
    await api.client.refetchQueries({ predicate: (q) => JSON.stringify(q.queryKey).includes("ListDisks") });
    await waitFor(() =>
      expect(api.client.getQueryCache().getAll().some((q) => JSON.stringify(q.queryKey).includes("ListDisks") && q.state.status === "error")).toBe(true),
    );
    expect(titles()).toContain("pg-data");
    expect(screen.queryByText("The disks did not load:")).toBeNull();
  });

  it("says a token's role only once the bindings came, and keeps it when their refetch failed", async () => {
    let bindings: () => Promise<ReturnType<typeof lists.listRoleBindings>> = () => never();
    render(<Host />, { wrapper: withApi(paletteApi({ listRoleBindings: () => bindings() })) });
    fireEvent.click(screen.getByRole("button", { name: "Open palette" }));
    type("ci-");
    expect((await findOption("ci-deploy")).textContent).not.toContain("Editor");
    cleanup();
    bindings = async () => lists.listRoleBindings();
    const api = paletteApi({ listRoleBindings: () => bindings() });
    render(<Host />, { wrapper: withApi(api) });
    fireEvent.click(screen.getByRole("button", { name: "Open palette" }));
    type("ci-");
    await waitFor(() => expect(option("ci-deploy")?.textContent).toContain("Editor"));
    bindings = () => Promise.reject(new ConnectError("the apiserver is unavailable", Code.Unavailable));
    await api.client.refetchQueries({ predicate: (q) => JSON.stringify(q.queryKey).includes("ListRoleBindings") });
    await waitFor(() =>
      expect(api.client.getQueryCache().getAll().some((q) => JSON.stringify(q.queryKey).includes("ListRoleBindings") && q.state.status === "error")).toBe(true),
    );
    expect(option("ci-deploy").textContent).toContain("Editor");
  });

  it("starts the create flows: a create page, or the list page with ?new=1 for a dialog", async () => {
    setup();
    fireEvent.click(await findOption("New container"));
    expect(navigate).toHaveBeenLastCalledWith({ to: "/projects/$project/new", params: { project: "demo" }, search: { kind: "container" } });
    const dialog = (to: string) => ({ to, search: { new: 1 } });
    for (const [title, call] of [
      ["New virtual machine", { to: "/projects/$project/new", params: { project: "demo" }, search: { kind: "vm" } }],
      ["New disk", { to: "/projects/demo/new/disk" }],
      ["New network", { to: "/projects/demo/new/network" }],
      ["New security group", dialog("/projects/demo/security-groups")],
      ["Reserve address", dialog("/projects/demo/addresses")],
      ["New public endpoint", dialog("/projects/demo/public-endpoints")],
      ["New secret", dialog("/projects/demo/secrets")],
      ["New service token", dialog("/projects/demo/service-tokens")],
      ["Add member", dialog("/projects/demo/members")],
    ] as const) {
      fireEvent.click(screen.getByRole("button", { name: "Open palette" }));
      fireEvent.click(await findOption(title));
      expect(navigate).toHaveBeenLastCalledWith(call);
    }
  });

  it("shows only the actions whose permission TestPermissions says is held", async () => {
    setup({ perms: () => ["storage.disks.apply"] });
    await findOption("New disk");
    expect(titles().filter((t) => /^(New|Add|Reserve) /.test(t))).toEqual(["New disk"]);
    expect(titles()).toContain("Switch project…");
  });

  it("hides every action while the permissions are not known, and says nothing of it", async () => {
    const api = setup({ perms: () => never() });
    await findOption("Disks");
    await waitFor(() => expect(api.calls).toContain("TestPermissions"));
    expect(titles().filter((t) => /^(New|Add|Reserve) /.test(t))).toEqual([]);
    expect(titles()).toContain("Switch project…");
  });

  it("hides every action when the permission check failed, and says nothing of it", async () => {
    const api = setup({ perms: () => Promise.reject(new ConnectError("the apiserver is unavailable", Code.Unavailable)) });
    await findOption("Disks");
    await waitFor(() =>
      expect(api.client.getQueryCache().getAll().filter((q) => JSON.stringify(q.queryKey).includes("TestPermissions") && q.state.status === "error").length).toBeGreaterThan(0),
    );
    expect(titles().filter((t) => /^(New|Add|Reserve) /.test(t))).toEqual([]);
    expect(screen.getByRole("dialog").textContent).not.toMatch(/permission|unavailable/i);
  });

  it("offers New project where the caller may create one in a tenant, and opens the form", async () => {
    setup({ perms: (scope) => (scope === "tenant:acme" ? ["tenancy.projects.apply"] : []) });
    fireEvent.click(await findOption("New project"));
    expect(await screen.findByRole("dialog", { name: "New project" })).toBeTruthy();
    expect(screen.queryByRole("combobox", { name: "Search or jump to" })).toBeNull();
  });

  it("hands Switch project over to the picker", async () => {
    setup();
    fireEvent.click(await findOption("Switch project…"));
    expect(await screen.findByRole("dialog", { name: "Projects and tenants" })).toBeTruthy();
  });

  it("states a list still loading, and does not call what it lacks no match", async () => {
    setup({ listDisks: () => never() });
    expect(await screen.findByText("The disks are loading.")).toBeTruthy();
    type("pg-data");
    expect(await screen.findByText("Nothing that has loaded matches “pg-data”.")).toBeTruthy();
    expect(screen.queryByText(/^Nothing in/)).toBeNull();
  });

  it("states a list that failed with the API's error, and still finds what the others hold", async () => {
    setup({ listSecrets: () => Promise.reject(new ConnectError("the apiserver is unavailable", Code.Unavailable)) });
    expect(await screen.findByText("The secrets did not load:")).toBeTruthy();
    expect(screen.getByText(/the apiserver is unavailable/)).toBeTruthy();
    type("web");
    await findOption("web");
  });

  it("says nothing matches once every list has answered", async () => {
    setup();
    await findOption("Disks");
    type("zzz");
    expect(await screen.findByText("Nothing in Demo, its members, the projects and tenants, or the pages matches “zzz”.")).toBeTruthy();
    await waitFor(() => expect(screen.queryByText(/are loading/)).toBeNull());
  });

  it("fetches no list a page has already fetched, and fetches once what was not", async () => {
    function Page() {
      useMachines("demo");
      useDisks("demo");
      useImages();
      useNetworks("demo");
      useSecurityGroups("demo");
      useAddresses("demo");
      usePublicEndpoints("demo");
      useSecrets("demo");
      useServiceTokens("demo");
      useTokenRoles("demo");
      useProjects();
      return null;
    }
    const api = paletteApi();
    render(
      <>
        <Page />
        <Host />
      </>,
      { wrapper: withApi(api) },
    );
    await waitFor(() => expect(api.client.getQueryCache().getAll().every((q) => q.state.status === "success")).toBe(true));
    const before = [...api.calls];
    expect(before).not.toContain("ListMembers");
    fireEvent.click(screen.getByRole("button", { name: "Open palette" }));
    type("a");
    await findOption("ada");
    await findOption("pg-data");
    const lists = (calls: string[]) => calls.filter((c) => c.startsWith("List") && c !== "ListMembers").sort();
    expect(lists(api.calls)).toEqual(lists(before));
    expect(api.calls.filter((c) => c === "ListMembers")).toEqual(["ListMembers"]);
  });

  it("in a tenant, finds its members, the projects and its pages, and asks for no project's objects", async () => {
    const acme: Place = { kind: "tenant", name: "acme" };
    const api = setup({}, acme);
    await findOption("Members");
    expect(titles()).toEqual(expect.arrayContaining(["Switch project…", "Overview", "Members"]));
    type("olga");
    fireEvent.click(await findOption("olga"));
    expect(navigate).toHaveBeenCalledWith({ to: "/tenants/acme/members" });
    expect(api.calls.filter((c) => ["ListDisks", "ListContainers", "ListSecrets"].includes(c))).toEqual([]);
  });
});

describe("Topbar", () => {
  it("opens the palette with Ctrl+K or ⌘K, and with its search control", async () => {
    render(<Topbar crumbs={["Disks"]} />, { wrapper: withApi(paletteApi()) });
    fireEvent.keyDown(window, { key: "k", ctrlKey: true });
    expect(await screen.findByRole("combobox")).toBeTruthy();
    key("Escape");
    fireEvent.keyDown(window, { key: "K", metaKey: true });
    expect(await screen.findByRole("combobox")).toBeTruthy();
    key("Escape");
    const control = screen.getByRole("button", { name: /Search or jump to/ });
    control.focus();
    fireEvent.click(control);
    expect(await screen.findByRole("combobox")).toBeTruthy();
    key("Escape");
    expect(document.activeElement).toBe(control);
    fireEvent.click(screen.getByRole("button", { name: "Search" }));
    expect(await screen.findByRole("combobox")).toBeTruthy();
  });

  it("leaves a plain K alone", () => {
    render(<Topbar crumbs={["Disks"]} />, { wrapper: withApi(paletteApi()) });
    fireEvent.keyDown(window, { key: "k" });
    expect(screen.queryByRole("combobox", { name: "Search or jump to" })).toBeNull();
  });
});
