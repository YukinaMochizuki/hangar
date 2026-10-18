// @vitest-environment jsdom

import { Code, ConnectError } from "@connectrpc/connect";
import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import type { TestPermissionsRequest } from "@hangar/api/hangar/iam/v1alpha1/account_pb";
import { DiskPhase, type Disk } from "@hangar/api/hangar/storage/v1alpha1/disk_pb";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { DiskList } from "@/pages/disk-list";
import { never, polyfillDialog, withApi } from "@/test/harness";
import { disk, storageApi } from "@/test/storage";

const navigate = vi.fn();
vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  useNavigate: () => navigate,
  Link: (await import("@/test/fake-link")).FakeLink,
}));

beforeAll(polyfillDialog);
beforeEach(() => navigate.mockReset());
afterEach(cleanup);

const table = () => screen.getByRole("table");

describe("DiskList", () => {
  it("shows skeletons while the disks load", async () => {
    render(<DiskList />, { wrapper: withApi(storageApi({ disks: never })) });
    expect((await screen.findAllByLabelText("Loading")).length).toBeGreaterThan(0);
  });

  it("says the list failed with the API's error, and asks again on Retry", async () => {
    let disks = () => Promise.reject<{ items: Disk[] }>(new ConnectError("the apiserver is unavailable", Code.Unavailable));
    const a = storageApi({ disks: () => disks() });
    render(<DiskList />, { wrapper: withApi(a) });
    await screen.findByText("The disks did not load");
    expect(screen.getByText("the apiserver is unavailable")).toBeTruthy();
    disks = async () => ({ items: [disk("scratch")] });
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await within(await screen.findByRole("table")).findByText("scratch");
    expect(a.calls.filter((c) => c === "ListDisks").length).toBe(2);
  });

  it("says the project has no disks", async () => {
    render(<DiskList />, { wrapper: withApi(storageApi({})) });
    await screen.findByText("No disks");
  });

  it("links an attached disk to the VM its machine runs for, and marks a root disk", async () => {
    const disks = [
      disk("pg-data", { phase: DiskPhase.IN_USE, machine: "vm-postgres" }),
      disk("postgres-root", { phase: DiskPhase.IN_USE, machine: "vm-postgres", rootOf: "postgres", image: "debian-13" }),
      disk("scratch"),
    ];
    render(<DiskList />, { wrapper: withApi(storageApi({ disks: async () => ({ items: disks }) })) });
    const t = await screen.findByRole("table");
    // The Machine's name is known before its owner: wait for the owner.
    await waitFor(() => expect(within(t).getAllByRole("link", { name: "postgres" })).toHaveLength(2));
    for (const l of within(t).getAllByRole("link", { name: "postgres" })) expect(l.getAttribute("href")).toBe("/projects/demo/vms/postgres");
    const rows = within(t).getAllByRole("row");
    expect(within(rows.find((r) => r.textContent?.includes("postgres-root"))!).getByText(/root disk/)).toBeTruthy();
    expect(within(rows.find((r) => r.textContent?.includes("pg-data"))!).queryByText(/root disk/)).toBeNull();
    expect(within(rows.find((r) => r.textContent?.includes("scratch"))!).getByText("none")).toBeTruthy();
  });

  it("shows a growth not yet granted and why, and why a node has not made a disk", async () => {
    const disks = [
      disk("pg-data", { size: "200Gi", have: "100Gi", phase: DiskPhase.IN_USE, machine: "vm-postgres", allocated: [ConditionStatus.FALSE, "ResizeDoesNotFit", "no room"] }),
      disk("build-root", { phase: DiskPhase.PENDING, image: "ubuntu", ready: [ConditionStatus.FALSE, "ImagePullFailed", "404 Not Found"] }),
    ];
    render(<DiskList />, { wrapper: withApi(storageApi({ disks: async () => ({ items: disks }) })) });
    await within(await screen.findByRole("table")).findByText("growing to 200 GiB");
    expect(within(table()).getByText("100 GiB")).toBeTruthy();
    expect(within(table()).getByText("ImagePullFailed")).toBeTruthy();
    expect(within(table()).getByText("· ResizeDoesNotFit")).toBeTruthy();
    expect(within(table()).getByText("not placed yet")).toBeTruthy();
  });

  it("filters by state", async () => {
    const disks = [disk("pg-data", { phase: DiskPhase.IN_USE, machine: "vm-postgres" }), disk("scratch"), disk("models", { phase: DiskPhase.PENDING })];
    render(<DiskList />, { wrapper: withApi(storageApi({ disks: async () => ({ items: disks }) })) });
    await within(await screen.findByRole("table")).findByText("scratch");
    fireEvent.click(screen.getByRole("radio", { name: /Available/ }));
    expect(within(table()).queryByText("pg-data")).toBeNull();
    expect(within(table()).getByText("scratch")).toBeTruthy();
    expect(screen.getByText("1 of 3")).toBeTruthy();
  });
});

describe("New disk, by permission", () => {
  const disks = async () => ({ items: [disk("scratch")] });

  it("links New disk to its page for a caller who holds storage.disks.apply, asked at the project", async () => {
    const a = storageApi({ disks, perms: async () => ["storage.disks.apply"] });
    render(<DiskList />, { wrapper: withApi(a) });
    const links = await screen.findAllByRole("link", { name: "New disk" });
    for (const l of links) expect(l.getAttribute("href")).toBe("/projects/demo/new/disk");
    const asked = a.requests.find((r) => r.method === "TestPermissions")!.request as TestPermissionsRequest;
    expect(asked.scope?.target).toEqual(expect.objectContaining({ case: "project", value: "demo" }));
    expect(asked.permissions).toEqual(["storage.disks.apply"]);
  });

  it("hides New disk from a caller who does not hold it, in the list and the empty state", async () => {
    const a = storageApi({ perms: async () => ["storage.disks.delete"] });
    render(<DiskList />, { wrapper: withApi(a) });
    await screen.findByText("No disks");
    await waitFor(() => expect(a.calls).toContain("TestPermissions"));
    expect(screen.queryByRole("link", { name: "New disk" })).toBeNull();
  });

  it("hides New disk and says nothing while the answer is unknown", async () => {
    const a = storageApi({ disks, perms: never });
    render(<DiskList />, { wrapper: withApi(a) });
    await within(await screen.findByRole("table")).findByText("scratch");
    expect(screen.queryByRole("link", { name: "New disk" })).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("hides New disk and says nothing when TestPermissions fails", async () => {
    const a = storageApi({ disks, perms: () => Promise.reject(new ConnectError("the apiserver is unavailable", Code.Unavailable)) });
    render(<DiskList />, { wrapper: withApi(a) });
    await within(await screen.findByRole("table")).findByText("scratch");
    await waitFor(() => expect(a.calls).toContain("TestPermissions"));
    expect(screen.queryByRole("link", { name: "New disk" })).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.queryByText(/unavailable/)).toBeNull();
  });
});
