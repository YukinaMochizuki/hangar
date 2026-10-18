// @vitest-environment jsdom

// What every create page shares: it asks TestPermissions once, at its
// project, for what creating needs; it shows its form only when that is
// held, states a refusal the answer gives, shows nothing while the answer
// is unknown, and the error when asking failed. Its name field asks
// password managers not to fill it.

import { Code, ConnectError } from "@connectrpc/connect";
import type { TestPermissionsRequest } from "@hangar/api/hangar/iam/v1alpha1/account_pb";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { CreateDisk } from "@/pages/create-disk";
import { CreateNetwork } from "@/pages/create-network";
import { never, polyfillDialog, withApi } from "@/test/harness";
import { paletteApi, type PaletteFake } from "@/test/palette-api";

vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  useNavigate: () => vi.fn(),
  Link: (await import("@/test/fake-link")).FakeLink,
}));

beforeAll(polyfillDialog);
afterEach(cleanup);

type Case = { kind: string; page: () => ReactNode; perm: string; field: string; refusal: string; list: string };

const cases: Case[] = [
  { kind: "disk", page: () => <CreateDisk />, perm: "storage.disks.apply", field: "Disk name", refusal: "creating disks", list: "disks" },
  { kind: "network", page: () => <CreateNetwork />, perm: "network.networks.apply", field: "Network name", refusal: "creating networks", list: "networks" },
];

function show(c: Case, f: PaletteFake = {}) {
  const api = paletteApi(f);
  render(<>{c.page()}</>, { wrapper: withApi(api) });
  return api;
}

describe.each(cases)("the New $kind page", (c) => {
  it("asks once, at its project, for what creating needs, and shows the form when it is held", async () => {
    const api = show(c);
    await screen.findByLabelText(c.field);
    const asked = api.requests.filter((r) => r.method === "TestPermissions").map((r) => r.request as TestPermissionsRequest);
    expect(asked).toHaveLength(1);
    expect(asked[0].scope?.target).toEqual({ case: "project", value: "demo" });
    expect(asked[0].permissions).toEqual([c.perm]);
  });

  it("states that the caller's roles do not include it, and shows no form", async () => {
    show(c, { perms: () => [] });
    await screen.findByText(`This account's roles do not include ${c.refusal} in this project.`);
    expect(screen.queryByLabelText(c.field)).toBeNull();
    expect(screen.queryByRole("button", { name: /^Create/ })).toBeNull();
  });

  it("shows nothing while the answer is unknown", async () => {
    const api = show(c, { perms: () => never() });
    await waitFor(() => expect(api.calls).toContain("TestPermissions"));
    expect(screen.queryByLabelText(c.field)).toBeNull();
    expect(screen.queryByText(/roles do not include/)).toBeNull();
  });

  it("shows the failed answer, and no form, when asking failed", async () => {
    show(c, { perms: () => Promise.reject(new ConnectError("the apiserver is unavailable", Code.Unavailable)) });
    await screen.findByText(/the apiserver is unavailable/);
    expect(screen.queryByLabelText(c.field)).toBeNull();
    expect(screen.queryByText(/roles do not include/)).toBeNull();
  });

  it("asks password managers not to fill the name, and Cancel goes back to the list", async () => {
    show(c);
    const name = await screen.findByLabelText(c.field);
    expect(name.getAttribute("autocomplete")).toBe("off");
    expect(name.hasAttribute("data-1p-ignore")).toBe(true);
    expect(screen.getByRole("link", { name: "Cancel" }).getAttribute("href")).toBe(`/projects/demo/${c.list}`);
  });
});
