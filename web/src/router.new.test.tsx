// @vitest-environment jsdom

// Creating from a list's address and from the palette. A kind created on a
// page has it at /new/<kind>, and its list's ?new=1 opens that page in place
// of the list's entry. A kind still created in a dialog opens it on ?new=1,
// and closing the dialog takes the parameter away, so neither going back
// nor reloading opens it again. These run the console's real routes over
// memory history.

import { TransportProvider } from "@connectrpc/connect-query";
import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeAll, describe, expect, it } from "vitest";
import { PrefsProvider } from "@/lib/prefs";
import { routeTree } from "@/router";
import { Code, ConnectError } from "@connectrpc/connect";
import { never, polyfillDialog, type Api } from "@/test/harness";
import { paletteApi, type PaletteFake } from "@/test/palette-api";

beforeAll(() => {
  polyfillDialog();
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {} })) as unknown as typeof window.matchMedia;
  HTMLElement.prototype.scrollIntoView ??= () => {};
});
afterEach(cleanup);

function app(entries: string[], f: PaletteFake = {}) {
  const api: Api = paletteApi(f);
  const history = createMemoryHistory({ initialEntries: entries, initialIndex: entries.length - 1 });
  const router = createRouter({ routeTree, history });
  render(
    <TransportProvider transport={api.transport}>
      <QueryClientProvider client={api.client}>
        <PrefsProvider>
          <RouterProvider router={router} />
        </PrefsProvider>
      </QueryClientProvider>
    </TransportProvider>,
  );
  return { api, router, history };
}

const dialog = (name: string) => screen.queryByRole("dialog", { name });

/** Records whether the dialog of this name was ever opened, however briefly, from now on. */
function watchOpened(name: string) {
  let opened = false;
  const seen = (records: MutationRecord[]) => {
    for (const r of records) {
      const t = r.target;
      if (t instanceof HTMLDialogElement && t.getAttribute("aria-label") === name && r.oldValue === null) opened = true;
    }
  };
  const mo = new MutationObserver(seen);
  mo.observe(document.body, { subtree: true, attributes: true, attributeOldValue: true, attributeFilter: ["open"] });
  return {
    opened: () => {
      seen(mo.takeRecords());
      return opened;
    },
    stop: () => mo.disconnect(),
  };
}

const type = (el: Element, value: string) => fireEvent.change(el, { target: { value } });
/** The dialog's first text field: every create form asks for the name first. */
const nameField = (d: HTMLElement) => within(d).getAllByRole("textbox")[0]!;

describe("a list route's ?new=1", () => {
  it.each([
    ["security-groups", "New security group"],
    ["public-endpoints", "New public endpoint"],
    ["secrets", "New secret"],
    ["service-tokens", "New service token"],
    ["members", "Add a member to Demo"],
  ])("on %s opens %s, and closing it clears the parameter so back and reload do not reopen it", async (page, title) => {
    const path = `/projects/demo/${page}`;
    const { router, history } = app([path, `${path}?new=1`]);
    const d = await screen.findByRole("dialog", { name: title });
    fireEvent.click(within(d).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(router.state.location.href).toBe(path));
    expect(dialog(title)).toBeNull();
    // The entry with ?new=1 was replaced: going back leaves the page without the dialog.
    history.back();
    await waitFor(() => expect(router.state.location.href).toBe(path));
    expect(history.length).toBe(2);
    expect(dialog(title)).toBeNull();
    // Reloading is the address as it now is, which asks for nothing.
    cleanup();
    app([router.state.location.href]);
    await screen.findByRole("heading", { level: 1 });
    expect(dialog(title)).toBeNull();
  });

  const refusedCases: [string, string][] = [
    ["security-groups", "New security group"],
    ["public-endpoints", "New public endpoint"],
    ["secrets", "New secret"],
    ["service-tokens", "New service token"],
    ["members", "Add a member to Demo"],
  ];

  it.each(refusedCases)("on %s, for a caller whose roles do not include creating there, never opens %s and replaces ?new=1 away", async (page, title) => {
    const path = `/projects/demo/${page}`;
    const watch = watchOpened(title);
    const { router, history } = app([path, `${path}?new=1`], { perms: () => [] });
    await waitFor(() => expect(router.state.location.href).toBe(path));
    expect(history.length).toBe(2);
    expect(watch.opened()).toBe(false);
    watch.stop();
  });

  it.each(refusedCases)("on %s, when the permission check failed, never opens %s and replaces ?new=1 away", async (page, title) => {
    const path = `/projects/demo/${page}`;
    const watch = watchOpened(title);
    const { router } = app([path, `${path}?new=1`], { perms: () => Promise.reject(new ConnectError("the apiserver is unavailable", Code.Unavailable)) });
    await waitFor(() => expect(router.state.location.href).toBe(path));
    expect(watch.opened()).toBe(false);
    watch.stop();
  });

  it("keeps ?new=1, and opens nothing, while the permission answer has not come", async () => {
    const watch = watchOpened("New security group");
    const { api, router } = app(["/projects/demo/security-groups?new=1"], { perms: () => never() });
    await waitFor(() => expect(api.calls).toContain("TestPermissions"));
    await screen.findAllByText("ssh-admin");
    expect(router.state.location.href).toBe("/projects/demo/security-groups?new=1");
    expect(watch.opened()).toBe(false);
    watch.stop();
  });

  it("on members, never opens Add member when the members list is refused, though the role allows adding, and replaces ?new=1 away", async () => {
    const title = "Add a member to Demo";
    const watch = watchOpened(title);
    const { router } = app(["/projects/demo/members", "/projects/demo/members?new=1"], {
      listMembers: () => Promise.reject(new ConnectError("listing members is not allowed", Code.PermissionDenied)),
    });
    await waitFor(() => expect(router.state.location.href).toBe("/projects/demo/members"));
    expect(watch.opened()).toBe(false);
    watch.stop();
  });

  // What each create form needs filled to send, and where a create lands: the object, or the list.
  const creates: [string, string, (d: HTMLElement) => void, string][] = [
    ["security-groups", "New security group", (d) => type(nameField(d), "fresh"), "/projects/demo/security-groups/fresh"],
    ["public-endpoints", "New public endpoint", (d) => type(nameField(d), "fresh"), "/projects/demo/public-endpoints/fresh"],
    [
      "secrets",
      "New secret",
      (d) => {
        const [name, registry, user] = within(d).getAllByRole("textbox");
        type(name!, "fresh");
        type(registry!, "registry.example.com");
        type(user!, "ci");
        type(d.querySelector("input[type=password]")!, "pw");
      },
      "/projects/demo/secrets/fresh",
    ],
    ["service-tokens", "New service token", (d) => type(nameField(d), "fresh"), "/projects/demo/service-tokens"],
    ["members", "Add a member to Demo", (d) => type(nameField(d), "new@example.com"), "/projects/demo/members"],
  ];

  it.each(creates)("on %s, creating from ?new=1 replaces it away first, so Back from where it lands does not reopen %s", async (page, title, fill, lands) => {
    const path = `/projects/demo/${page}`;
    const { router, history } = app([path, `${path}?new=1`]);
    const d = await screen.findByRole("dialog", { name: title });
    fill(d);
    const submit = () => within(d).getAllByRole("button").find((b) => /^(Create|Reserve|Add member)/.test(b.textContent?.trim() ?? "")) as HTMLButtonElement;
    await waitFor(() => expect(submit().disabled).toBe(false));
    fireEvent.click(submit());
    if (page === "service-tokens") {
      // The secret is shown once; closing it is the end of creating.
      const once = await screen.findByRole("dialog", { name: "Token fresh created" });
      fireEvent.click(within(once).getByRole("button", { name: "Close" }));
    }
    await waitFor(() => expect(router.state.location.href).toBe(lands));
    expect(dialog(title)).toBeNull();
    // The entries are the list, the list again (?new=1 replaced), and the object when it lands there.
    expect(history.length).toBe(lands === path ? 2 : 3);
    const before = history.location.state.key;
    history.back();
    await waitFor(() => expect(router.state.location.state.key).not.toBe(before));
    expect(router.state.location.href).toBe(path);
    await screen.findByRole("heading", { level: 1 });
    expect(dialog(title)).toBeNull();
  });
});

const field = (name: string) => screen.getByRole("textbox", { name });
const submitOf = () => screen.getAllByRole("button").find((b) => /^(Create|Reserve)/.test(b.textContent?.trim() ?? "")) as HTMLButtonElement;

// Each kind created on a page: its list, its page's path and title, what the
// page needs filled to send, and where a create lands.
const pages: [string, string, string, () => void, string][] = [
  ["disks", "disk", "New disk", () => type(field("Disk name"), "fresh"), "/projects/demo/disks/fresh"],
  ["networks", "network", "New network", () => type(field("Network name"), "fresh"), "/projects/demo/networks/fresh"],
  ["addresses", "address", "Reserve address", () => type(field("Address name"), "fresh"), "/projects/demo/addresses/fresh"],
];

describe("a create page", () => {
  it.each(pages)("is what %s?new=1 opens, in place of the list's entry", async (list, kind, title) => {
    const path = `/projects/demo/${list}`;
    const { router, history } = app([path, `${path}?new=1`]);
    await screen.findByRole("heading", { level: 1, name: title });
    expect(router.state.location.href).toBe(`/projects/demo/new/${kind}`);
    expect(history.length).toBe(2);
    history.back();
    await waitFor(() => expect(router.state.location.href).toBe(path));
  });

  it.each(pages)("of %s opens the object made in its place, so Back returns to the list", async (list, kind, title, fill, lands) => {
    const path = `/projects/demo/${list}`;
    const { router, history } = app([path, `/projects/demo/new/${kind}`]);
    await screen.findByRole("heading", { level: 1, name: title });
    fill();
    await waitFor(() => expect(submitOf().disabled).toBe(false));
    fireEvent.click(submitOf());
    if (kind === "service-token") {
      // The secret is shown once; closing it opens the token.
      const once = await screen.findByRole("dialog", { name: "Token fresh created" });
      fireEvent.click(within(once).getByRole("button", { name: "Close" }));
    }
    await waitFor(() => expect(router.state.location.href).toBe(lands));
    expect(history.length).toBe(2);
    history.back();
    await waitFor(() => expect(router.state.location.href).toBe(path));
  });

  it.each(pages)("of %s goes back to the list on Cancel", async (list, kind, title) => {
    const { router } = app([`/projects/demo/new/${kind}`]);
    await screen.findByRole("heading", { level: 1, name: title });
    fireEvent.click(screen.getByRole("link", { name: "Cancel" }));
    await waitFor(() => expect(router.state.location.href).toBe(`/projects/demo/${list}`));
  });

  it.each(pages)("of %s, for a caller whose roles do not include creating there, states it and shows no form", async (_list, kind, title) => {
    app([`/projects/demo/new/${kind}`], { perms: () => [] });
    await screen.findByText(/^This account's roles do not include .* in this project\.$/);
    expect(screen.getByRole("heading", { level: 1, name: title })).toBeTruthy();
    expect(screen.queryByRole("textbox")).toBeNull();
  });

  it("is what the palette's New disk opens, from another page", async () => {
    const { router } = app(["/projects/demo/networks"]);
    await screen.findByRole("heading", { level: 1, name: "Networks" });
    fireEvent.keyDown(window, { key: "k", ctrlKey: true });
    const box = await screen.findByRole("combobox", { name: "Search or jump to" });
    fireEvent.change(box, { target: { value: "new disk" } });
    await waitFor(() => expect(screen.getAllByRole("option").map((o) => o.textContent)).toContain("New disk"));
    fireEvent.keyDown(box, { key: "Enter" });
    await screen.findByRole("heading", { level: 1, name: "New disk" });
    expect(router.state.location.href).toBe("/projects/demo/new/disk");
  });

  it("leaves an object named new its own page", async () => {
    const { router } = app(["/projects/demo/disks/new"], { listDisks: () => ({ items: [{ metadata: { name: "new", project: "demo" }, spec: { size: "10Gi" }, status: { size: "10Gi" } }] }) });
    await screen.findByRole("heading", { level: 1, name: "new" });
    expect(router.state.location.href).toBe("/projects/demo/disks/new");
  });
});
