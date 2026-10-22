// @vitest-environment jsdom

// A public endpoint's custom hostnames (docs/decisions/custom-domains.md):
// where its page says each serves or why it waits; the rows of New public
// endpoint and Edit hostnames as the spec.hostnames they send, the latter
// at the version the dialog opened on; and Make private held back while the
// endpoint has custom hostnames, which the API refuses.

import { PublicEndpointAccess, type ApplyPublicEndpointRequest } from "@hangar/api/hangar/network/v1alpha1/public_endpoint_pb";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { CreatePublicEndpoint } from "@/pages/create-public-endpoint";
import { PublicEndpointDetail } from "@/pages/public-endpoints";
import { domainsApi, endpoints, type DomainOverrides, type DomainsApi } from "@/test/domains-api";
import { polyfillDialog, withApi } from "@/test/harness";

vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  useNavigate: () => vi.fn(),
  Link: ({ children, className }: { children: ReactNode; className?: string }) => <a className={className}>{children}</a>,
}));

beforeAll(polyfillDialog);
afterEach(cleanup);

function show(page: ReactNode, project = "demo", o: DomainOverrides = {}): DomainsApi {
  const api = domainsApi(o);
  render(page, { wrapper: withApi(api, project) });
  return api;
}

const type = (label: string | RegExp, value: string) => fireEvent.change(screen.getByLabelText(label), { target: { value } });
const click = (name: string | RegExp) => fireEvent.click(screen.getAllByRole("button", { name }).at(-1)!);
const button = (name: string | RegExp) => screen.getAllByRole("button", { name }).at(-1) as HTMLButtonElement;
const applies = (api: DomainsApi) => api.requests.filter((r) => r.method === "ApplyPublicEndpoint").map((r) => r.request as ApplyPublicEndpointRequest);

describe("an endpoint's Hostnames card", () => {
  it("links each custom hostname the status says serves", async () => {
    show(<PublicEndpointDetail name="web" />);
    expect(await screen.findByText("Hostnames")).toBeTruthy();
    expect(screen.getAllByText("www.example.com").length).toBeGreaterThan(0);
    expect(screen.getAllByText("serving")).toHaveLength(2);
  });

  it("says a hostname waits for its domain to be verified", async () => {
    show(<PublicEndpointDetail name="shop" />, "shop");
    expect(await screen.findByText("Waits for example.org to be verified.")).toBeTruthy();
  });
});

describe("Make private", () => {
  it("is held back, with the fact, while the endpoint has custom hostnames", async () => {
    const api = show(<PublicEndpointDetail name="web" />);
    await screen.findByText("Hostnames");
    click("Make private");
    expect(await screen.findByText("web serves at 2 custom hostnames: example.com, www.example.com.")).toBeTruthy();
    expect(button("Make private").disabled).toBe(true);
    expect(applies(api)).toHaveLength(0);
  });

  it("is not held back for an endpoint without them", async () => {
    const plain = endpoints();
    plain[0].spec!.hostnames = [];
    show(<PublicEndpointDetail name="web" />, "demo", { listPublicEndpoints: () => ({ items: plain }) });
    await screen.findByText("Hostnames");
    click("Make private");
    const dialog = await screen.findByRole("dialog", { name: "Make web private" });
    expect(within(dialog).queryByText(/custom hostname/)).toBeNull();
    expect(button("Make private").disabled).toBe(false);
  });
});

describe("New public endpoint's custom hostnames", () => {
  async function fill() {
    await screen.findByRole("option", { name: "api (container)" });
    await waitFor(() => expect(screen.queryByText(/still loading/)).toBeNull());
    type("Endpoint name", "store");
    fireEvent.click(screen.getByRole("radio", { name: "Public" }));
    // Domains demo may use: example.com (all projects) and shop.example.net (demo only, held by another tenant).
    click("Add hostname");
    type("Hostname 1 label", "store");
    click("Add hostname");
    fireEvent.change(screen.getByLabelText("Hostname 2 domain"), { target: { value: "shop.example.net" } });
  }

  it("sends the rows as spec.hostnames, an empty label being the domain itself, and marks a domain not verified", async () => {
    const api = show(<CreatePublicEndpoint />);
    await fill();
    expect(within(screen.getByLabelText("Hostname 2 domain")).getByRole("option", { name: "shop.example.net (not verified)" })).toBeTruthy();
    expect(screen.getByText("shop.example.net is not verified; shop.example.net serves once it is.")).toBeTruthy();
    // A domain the project may not use is not offered.
    expect(screen.queryByRole("option", { name: /example\.org/ })).toBeNull();
    click("Create public endpoint");
    await waitFor(() => expect(applies(api)).toHaveLength(1));
    expect(applies(api)[0].publicEndpoint?.spec?.hostnames).toEqual(["store.example.com", "shop.example.net"]);
    expect(applies(api)[0].publicEndpoint?.spec?.access).toBe(PublicEndpointAccess.PUBLIC);
  });

  it("sends none for a private endpoint, rows typed while it was public or not", async () => {
    const api = show(<CreatePublicEndpoint />);
    await fill();
    fireEvent.click(screen.getByRole("radio", { name: "Private" }));
    expect(screen.getByText("Public endpoints only.")).toBeTruthy();
    click("Create public endpoint");
    await waitFor(() => expect(applies(api)).toHaveLength(1));
    expect(applies(api)[0].publicEndpoint?.spec?.hostnames).toEqual([]);
  });

  it("refuses a label that is not a DNS label, and a hostname named twice", async () => {
    show(<CreatePublicEndpoint />);
    await fill();
    type("Hostname 1 label", "Not_ok");
    expect(await screen.findByText(/not_ok is not a DNS label/)).toBeTruthy();
    type("Hostname 1 label", "");
    fireEvent.change(screen.getByLabelText("Hostname 1 domain"), { target: { value: "shop.example.net" } });
    expect(await screen.findByText(/shop.example.net is named twice\./)).toBeTruthy();
    expect(button("Create public endpoint").disabled).toBe(true);
  });
});

describe("Edit hostnames", () => {
  it("applies the endpoint as it opened on, its hostnames alone changed, at the version read", async () => {
    const api = show(<PublicEndpointDetail name="web" />);
    await screen.findByText("Hostnames");
    click("Edit hostnames");
    await screen.findByLabelText("Hostname 2 label");
    // The domain itself reads back as an empty label under example.com.
    expect((screen.getByLabelText("Hostname 1 label") as HTMLInputElement).value).toBe("");
    expect((screen.getByLabelText("Hostname 1 domain") as HTMLSelectElement).value).toBe("example.com");
    click("Remove hostname 2");
    expect(screen.getByText("Removes www.example.com")).toBeTruthy();
    click("Apply");
    await waitFor(() => expect(applies(api)).toHaveLength(1));
    const sent = applies(api)[0];
    expect(sent.createOnly).toBe(false);
    expect(sent.publicEndpoint?.spec?.hostnames).toEqual(["example.com"]);
    expect(sent.publicEndpoint?.spec?.port).toBe(8080);
    expect(sent.publicEndpoint?.metadata?.resourceVersion).toBe(20n);
  });

  it("says the endpoint changed after the dialog opened and writes nothing over it", async () => {
    const newer = endpoints();
    newer[0].metadata!.resourceVersion = 21n;
    const api = show(<PublicEndpointDetail name="web" />, "demo", { server: { endpoints: newer } });
    await screen.findByText("Hostnames");
    click("Edit hostnames");
    await screen.findByLabelText("Hostname 2 label");
    click("Remove hostname 2");
    click("Apply");
    expect(await screen.findByText("web changed since this dialog opened.")).toBeTruthy();
    expect(api.server.endpoints[0].spec?.hostnames).toEqual(["example.com", "www.example.com"]);
    expect(button("Apply").disabled).toBe(true);
  });
});
