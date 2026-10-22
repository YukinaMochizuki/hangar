// @vitest-environment jsdom

// The custom domain pages (docs/decisions/custom-domains.md): the tenant's
// list and a domain's page loading, failed, empty and full; which controls
// TestPermissions at the tenant allows; and every change as the RPC it
// sends: create-only for New, Edit projects at the version the dialog
// opened on, and Delete, held back while endpoints serve under the domain.

import { Code } from "@connectrpc/connect";
import type { ApplyCustomDomainRequest, DeleteCustomDomainRequest } from "@hangar/api/hangar/network/v1alpha1/custom_domain_pb";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { CreateCustomDomain } from "@/pages/create-custom-domain";
import { CustomDomainDetail, CustomDomainList } from "@/pages/custom-domains";
import { domains, domainsApi, refuse, type DomainOverrides, type DomainsApi } from "@/test/domains-api";
import { never, polyfillDialog, withApi } from "@/test/harness";

const navigate = vi.fn();
vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  useNavigate: () => navigate,
  Link: ({ children, className }: { children: ReactNode; className?: string }) => <a className={className}>{children}</a>,
}));

beforeAll(polyfillDialog);
beforeEach(() => navigate.mockReset());
afterEach(cleanup);

function show(page: ReactNode, o: DomainOverrides = {}): DomainsApi {
  const api = domainsApi(o);
  render(page, { wrapper: withApi(api, { kind: "tenant", name: "acme" }) });
  return api;
}

const type = (label: string | RegExp, value: string) => fireEvent.change(screen.getByLabelText(label), { target: { value } });
const click = (name: string | RegExp) => fireEvent.click(screen.getAllByRole("button", { name }).at(-1)!);
const button = (name: string | RegExp) => screen.getAllByRole("button", { name }).at(-1) as HTMLButtonElement;
const writes = (api: DomainsApi) => api.requests.filter((r) => !r.method.startsWith("List") && !r.method.startsWith("Get") && r.method !== "TestPermissions");
const unavailable = refuse(Code.Unavailable, "connection refused");
const none = () => ({ permissions: [] });

describe("the custom domains list", () => {
  it("shows skeletons while it loads", async () => {
    show(<CustomDomainList />, { listCustomDomains: () => never() });
    expect(await screen.findByLabelText("Loading")).toBeTruthy();
  });

  it("shows the API's error in place, and asks again on Retry", async () => {
    const api = show(<CustomDomainList />, { listCustomDomains: unavailable });
    expect(await screen.findByText("The custom domains did not load")).toBeTruthy();
    const before = api.calls.filter((m) => m === "ListCustomDomains").length;
    click("Retry");
    await vi.waitFor(() => expect(api.calls.filter((m) => m === "ListCustomDomains").length).toBeGreaterThan(before));
  });

  it("says when the tenant has none", async () => {
    show(<CustomDomainList />, { listCustomDomains: () => ({ items: [] }) });
    expect(await screen.findByText("No custom domains")).toBeTruthy();
  });

  it("lists the tenant's domains with their state, projects and endpoints, and no other tenant's", async () => {
    show(<CustomDomainList />);
    const table = within(await screen.findByRole("table"));
    for (const t of ["example.com", "example.org", "shop.example.net", "Verified", "Waiting for DNS", "Held by another tenant", "All projects", "Shop", "Demo"]) {
      expect(table.getByText(t)).toBeTruthy();
    }
    expect(table.queryByText("zeta.example")).toBeNull();
    // web has two hostnames under example.com, shop one under example.org.
    await waitFor(() => expect(table.getAllByText("1")).toHaveLength(2));
    expect(table.getByText("0")).toBeTruthy();
  });

  it("reads every page of the list, as the API may hand back a short page with more after it", async () => {
    const all = domains();
    show(<CustomDomainList />, {
      listCustomDomains: (req) => (req.pageToken ? { items: all.slice(1) } : { items: all.slice(0, 1), nextPageToken: "more" }),
    });
    const table = within(await screen.findByRole("table"));
    expect(await table.findByText("shop.example.net")).toBeTruthy();
    expect(table.getByText("example.com")).toBeTruthy();
  });

  it("offers New custom domain and row menus only where the tenant's answer holds their permissions", async () => {
    show(<CustomDomainList />);
    expect((await screen.findAllByText(/New custom domain/)).length).toBeGreaterThan(0);
    expect(screen.getAllByRole("button", { name: "Actions for example.com" }).length).toBeGreaterThan(0);
  });

  it("offers neither to someone whose roles do not hold them, such as a project's viewer", async () => {
    const api = show(<CustomDomainList />, { permissions: none });
    await screen.findByRole("table");
    await waitFor(() => expect(api.calls).toContain("TestPermissions"));
    expect(screen.queryByText(/New custom domain/)).toBeNull();
    expect(screen.queryByRole("button", { name: /^Actions for/ })).toBeNull();
    const asked = api.requests.find((r) => r.method === "TestPermissions")!.request as { scope: { target: { case: string; value: string } } };
    expect(asked.scope.target).toMatchObject({ case: "tenant", value: "acme" });
  });
});

describe("a custom domain's page", () => {
  it("shows skeletons while the list loads, and the error when it failed", async () => {
    show(<CustomDomainDetail name="example-org" />, { listCustomDomains: () => never() });
    expect((await screen.findAllByLabelText("Loading")).length).toBeGreaterThan(0);
    cleanup();
    show(<CustomDomainDetail name="example-org" />, { listCustomDomains: unavailable });
    expect(await screen.findByText("The custom domain did not load")).toBeTruthy();
  });

  it("says when the tenant has no such domain", async () => {
    show(<CustomDomainDetail name="nope" />);
    expect(await screen.findByText("No such custom domain")).toBeTruthy();
    expect(screen.getByText(/This tenant has no custom domain named/)).toBeTruthy();
  });

  it("states why a domain waits, its records with every name and value copyable, and the endpoints under it", async () => {
    show(<CustomDomainDetail name="example-org" />);
    expect((await screen.findAllByText("Waiting for DNS")).length).toBeGreaterThan(0);
    expect(screen.getByText("_acme-challenge.example.org must be a CNAME of t-example-org.acme.example.net.")).toBeTruthy();
    // Two records, each its name and value, in the phone list and the table.
    expect(screen.getAllByRole("button", { name: "Copy" }).length).toBeGreaterThanOrEqual(8);
    expect(await screen.findByText("shop.example.org")).toBeTruthy();
  });

  it("says the records are DNS only, as a proxy in front would hide the challenge record", async () => {
    show(<CustomDomainDetail name="example-org" />);
    expect(await screen.findByText(/Each record is DNS only, with no proxy in front \(in Cloudflare, the grey cloud\)/)).toBeTruthy();
  });

  it("offers no change to someone whose roles hold no write at the tenant", async () => {
    const api = show(<CustomDomainDetail name="example-com" />, { permissions: none });
    await screen.findAllByText("Verified");
    await waitFor(() => expect(api.calls).toContain("TestPermissions"));
    for (const name of [/Edit projects/, /Delete/, /Edit labels/]) expect(screen.queryByRole("button", { name })).toBeNull();
    // Its CLI lists only what the caller may run.
    expect(screen.getByText("hangar get cdom example-com")).toBeTruthy();
    expect(screen.queryByText(/hangar (apply|delete)/)).toBeNull();
  });

  it("holds Delete back, saying which endpoints serve under the domain", async () => {
    const api = show(<CustomDomainDetail name="example-com" />);
    await screen.findAllByText("Verified");
    click("Delete");
    type("Name, to confirm", "example-com");
    expect(await screen.findByText("1 endpoint has hostnames under this domain: web.")).toBeTruthy();
    expect(button("Delete").disabled).toBe(true);
    expect(writes(api)).toHaveLength(0);
  });

  it("deletes a domain no endpoint uses with the API's Delete, then goes to the list", async () => {
    const api = show(<CustomDomainDetail name="shop-example-net" />);
    await screen.findAllByText("Held by another tenant");
    click("Delete");
    type("Name, to confirm", "shop-example-net");
    await waitFor(() => expect(button("Delete").disabled).toBe(false));
    click("Delete");
    await waitFor(() => expect(writes(api).map((w) => w.method)).toEqual(["DeleteCustomDomain"]));
    expect((writes(api)[0].request as DeleteCustomDomainRequest).name).toBe("shop-example-net");
    await waitFor(() => expect(navigate).toHaveBeenCalledWith({ to: "/tenants/$tenant/domains", params: { tenant: "acme" } }));
  });

  it("applies Edit projects over the domain as read, its projects alone changed, at the version read", async () => {
    const api = show(<CustomDomainDetail name="example-org" />);
    await screen.findAllByText("Waiting for DNS");
    click("Edit projects");
    fireEvent.click(screen.getByRole("checkbox", { name: /^Demo/ }));
    expect(screen.getByText("Lets Demo")).toBeTruthy();
    click("Apply");
    await waitFor(() => expect(writes(api).map((w) => w.method)).toEqual(["ApplyCustomDomain"]));
    const sent = (writes(api)[0].request as ApplyCustomDomainRequest).customDomain!;
    expect(sent.spec).toMatchObject({ tenant: "acme", domain: "example.org", projects: ["demo", "shop"] });
    expect(sent.metadata?.resourceVersion).toBe(10n);
    expect(sent.status?.challengeTarget).toBe("t-example-org.acme.example.net");
  });

  it("will not leave out a project whose endpoint serves under the domain", async () => {
    const api = show(<CustomDomainDetail name="example-com" />);
    await screen.findAllByText("Verified");
    click("Edit projects");
    fireEvent.click(screen.getByRole("radio", { name: "Chosen projects" }));
    fireEvent.click(screen.getByRole("checkbox", { name: /^Shop/ }));
    expect(await screen.findByText("web of Demo serves at example.com; web of Demo serves at www.example.com, under this domain.")).toBeTruthy();
    expect(button("Apply").disabled).toBe(true);
    expect(writes(api)).toHaveLength(0);
  });

  it("says the domain changed after the dialog opened, writes nothing over it, and starts again from it on Reload", async () => {
    const newer = domains();
    newer[1].metadata!.resourceVersion = 11n;
    newer[1].spec!.projects = ["shop", "demo"];
    const api = show(<CustomDomainDetail name="example-org" />, { server: { domains: newer } });
    await screen.findAllByText("Waiting for DNS");
    click("Edit projects");
    fireEvent.click(screen.getByRole("radio", { name: "All projects" }));
    click("Apply");
    expect(await screen.findByText("example-org changed since this dialog opened.")).toBeTruthy();
    expect(api.server.domains[1].spec?.projects).toEqual(["shop", "demo"]);
    click("Reload");
    await waitFor(() => expect((screen.getByRole("checkbox", { name: /^Demo/ }) as HTMLInputElement).checked).toBe(true));
    expect(screen.queryByText(/changed since this dialog opened/)).toBeNull();
  });
});

describe("New custom domain", () => {
  it("suggests the name from the domain and sends one create-only Apply, then opens the domain in the page's place", async () => {
    const api = show(<CreateCustomDomain />);
    await screen.findByLabelText("Domain");
    type("Domain", "Shop.Example.dev");
    expect((screen.getByLabelText("Custom domain name") as HTMLInputElement).value).toBe("shop-example-dev");
    fireEvent.click(screen.getByRole("radio", { name: "Chosen projects" }));
    fireEvent.click(screen.getByRole("checkbox", { name: /^Shop/ }));
    await waitFor(() => expect(button("Create custom domain").disabled).toBe(false));
    click("Create custom domain");
    await waitFor(() => expect(writes(api).map((w) => w.method)).toEqual(["ApplyCustomDomain"]));
    const req = writes(api)[0].request as ApplyCustomDomainRequest;
    expect(req.createOnly).toBe(true);
    expect(req.customDomain?.metadata?.name).toBe("shop-example-dev");
    expect(req.customDomain?.spec).toMatchObject({ tenant: "acme", domain: "shop.example.dev", projects: ["shop"] });
    await waitFor(() =>
      expect(navigate).toHaveBeenCalledWith({ to: "/tenants/$tenant/domains/$name", params: { tenant: "acme", name: "shop-example-dev" }, replace: true }),
    );
  });

  it("says the API's refusal of a name another tenant holds, which no list here shows", async () => {
    show(<CreateCustomDomain />, { applyCustomDomain: refuse(Code.AlreadyExists, "shop-example-dev already exists") });
    await screen.findByLabelText("Domain");
    type("Domain", "shop.example.dev");
    await waitFor(() => expect(button("Create custom domain").disabled).toBe(false));
    click("Create custom domain");
    expect(await screen.findByText("A custom domain named shop-example-dev exists. Names are unique on the platform.")).toBeTruthy();
    expect(navigate).not.toHaveBeenCalled();
  });

  it("refuses a domain the tenant has already", async () => {
    show(<CreateCustomDomain />);
    await screen.findByLabelText("Domain");
    type("Domain", "example.com");
    expect(await screen.findByText(/The tenant has example.com already\./)).toBeTruthy();
    expect(button("Create custom domain").disabled).toBe(true);
  });

  it("states the refusal once the tenant's answer lacks the permission", async () => {
    show(<CreateCustomDomain />, { permissions: none });
    expect(await screen.findByText("This account's roles do not include creating custom domains in this tenant.")).toBeTruthy();
    expect(screen.queryByLabelText("Domain")).toBeNull();
  });
});
