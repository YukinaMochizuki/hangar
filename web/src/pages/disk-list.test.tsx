// @vitest-environment jsdom

import { Code, ConnectError } from "@connectrpc/connect";
import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import type { TestPermissionsRequest } from "@hangar/api/hangar/iam/v1alpha1/account_pb";
import { DiskPhase, type ApplyDiskRequest, type Disk } from "@hangar/api/hangar/storage/v1alpha1/disk_pb";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { DiskList } from "@/pages/disk-list";
import { deferred, never, polyfillDialog, withApi } from "@/test/harness";
import { disk, image, storageApi } from "@/test/storage";

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

describe("New disk", () => {
  const open = async () => {
    // An empty list offers New disk in its header and its empty state.
    fireEvent.click((await screen.findAllByRole("button", { name: "New disk" }))[0]);
    return screen.getByRole("dialog", { name: "New disk" });
  };
  const submitButton = (d: HTMLElement) => within(d).getByRole("button", { name: /Create disk|Loading names|Creating/ }) as HTMLButtonElement;

  it("creates a disk of an image with one create-only apply, then opens the disk's page", async () => {
    const applied: ApplyDiskRequest[] = [];
    const a = storageApi({
      disks: async () => ({ items: [disk("scratch")] }),
      images: async () => ({ items: [image("debian-13", { size: "3Gi", min: "10Gi", verified: [ConditionStatus.TRUE, "Verified", ""] })] }),
      apply: (req) => (applied.push(req), { disk: req.disk, created: true }),
    });
    render(<DiskList />, { wrapper: withApi(a) });
    const d = await open();
    fireEvent.change(within(d).getByLabelText("Name"), { target: { value: "data" } });
    fireEvent.change(within(d).getByLabelText("Size, GiB"), { target: { value: "12" } });
    fireEvent.change(within(d).getByLabelText("Holds"), { target: { value: "debian-13" } });
    await waitFor(() => expect(submitButton(d).disabled).toBe(false));
    fireEvent.click(submitButton(d));
    await waitFor(() => expect(navigate).toHaveBeenCalledWith({ to: "/projects/$project/disks/$name", params: { project: "demo", name: "data" } }));
    expect(a.calls.filter((c) => c === "GetDisk" || c === "ApplyDisk")).toEqual(["ApplyDisk"]);
    expect(applied).toHaveLength(1);
    expect(applied[0].createOnly).toBe(true);
    const sent = applied[0].disk!;
    expect(sent.metadata?.name).toBe("data");
    expect(sent.metadata?.project).toBe("demo");
    expect(sent.spec?.size).toBe("12Gi");
    expect(sent.spec?.source?.source).toEqual(expect.objectContaining({ case: "image", value: expect.objectContaining({ name: "debian-13" }) }));
    expect(within(d).getAllByText(/hangar apply --create-only -p demo -f data\.yaml/).length).toBeGreaterThan(0);
  });

  it("refuses a size below the image's smallest disk, and sends nothing", async () => {
    const a = storageApi({ images: async () => ({ items: [image("debian-13", { min: "10Gi" })] }) });
    render(<DiskList />, { wrapper: withApi(a) });
    const d = await open();
    fireEvent.change(within(d).getByLabelText("Name"), { target: { value: "data" } });
    fireEvent.change(within(d).getByLabelText("Holds"), { target: { value: "debian-13" } });
    fireEvent.change(within(d).getByLabelText("Size, GiB"), { target: { value: "5" } });
    await within(d).findByText("debian-13 needs 10 GiB; 5 GiB is less.");
    expect(submitButton(d).disabled).toBe(true);
    fireEvent.submit(submitButton(d).closest("form")!);
    expect(a.calls).not.toContain("ApplyDisk");
  });

  it("refuses a name the list holds, and sends nothing while the list loads", async () => {
    const list = deferred<{ items: Disk[] }>();
    const a = storageApi({ disks: () => list.promise });
    render(<DiskList />, { wrapper: withApi(a) });
    // While the list loads, the page shows skeletons and the button.
    const d = await open();
    fireEvent.change(within(d).getByLabelText("Name"), { target: { value: "scratch" } });
    await within(d).findByText("The project's disks are still loading.");
    expect(submitButton(d).disabled).toBe(true);
    list.resolve({ items: [disk("scratch")] });
    await within(d).findByText("A disk named scratch exists in this project.");
    expect(submitButton(d).disabled).toBe(true);
    expect(a.calls).not.toContain("ApplyDisk");
  });

  it("says a name taken since the list loaded exists, as the create-only apply's refusal", async () => {
    const applied: ApplyDiskRequest[] = [];
    const a = storageApi({ get: (name) => disk(name), apply: (req) => (applied.push(req), { disk: req.disk, created: true }) });
    render(<DiskList />, { wrapper: withApi(a) });
    const d = await open();
    fireEvent.change(within(d).getByLabelText("Name"), { target: { value: "data" } });
    await waitFor(() => expect(submitButton(d).disabled).toBe(false));
    fireEvent.click(submitButton(d));
    await within(d).findByText("Creating the disk failed");
    expect(within(d).getByText("A disk named data exists in this project.")).toBeTruthy();
    expect(a.requests.filter((r) => r.method === "ApplyDisk").map((r) => (r.request as ApplyDiskRequest).createOnly)).toEqual([true]);
    expect(applied).toHaveLength(0);
    expect(navigate).not.toHaveBeenCalled();
  });

  it("shows the API's refusal of the apply in the dialog", async () => {
    const a = storageApi({
      apply: () => {
        throw new ConnectError("quota exceeded: the disk limit is 100Gi", Code.ResourceExhausted);
      },
    });
    render(<DiskList />, { wrapper: withApi(a) });
    const d = await open();
    fireEvent.change(within(d).getByLabelText("Name"), { target: { value: "data" } });
    await waitFor(() => expect(submitButton(d).disabled).toBe(false));
    fireEvent.click(submitButton(d));
    await within(d).findByText("quota exceeded: the disk limit is 100Gi");
    expect(navigate).not.toHaveBeenCalled();
  });
});

describe("New disk, by permission", () => {
  const disks = async () => ({ items: [disk("scratch")] });

  it("shows New disk to a caller who holds storage.disks.apply, asked at the project", async () => {
    const a = storageApi({ disks, perms: async () => ["storage.disks.apply"] });
    render(<DiskList />, { wrapper: withApi(a) });
    await screen.findByRole("button", { name: "New disk" });
    const asked = a.requests.find((r) => r.method === "TestPermissions")!.request as TestPermissionsRequest;
    expect(asked.scope?.target).toEqual(expect.objectContaining({ case: "project", value: "demo" }));
    expect(asked.permissions).toEqual(["storage.disks.apply"]);
  });

  it("hides New disk from a caller who does not hold it, in the list and the empty state", async () => {
    const a = storageApi({ perms: async () => ["storage.disks.delete"] });
    render(<DiskList />, { wrapper: withApi(a) });
    await screen.findByText("No disks");
    await waitFor(() => expect(a.calls).toContain("TestPermissions"));
    expect(screen.queryByRole("button", { name: "New disk" })).toBeNull();
  });

  it("hides New disk and says nothing while the answer is unknown", async () => {
    const a = storageApi({ disks, perms: never });
    render(<DiskList />, { wrapper: withApi(a) });
    await within(await screen.findByRole("table")).findByText("scratch");
    expect(screen.queryByRole("button", { name: "New disk" })).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("hides New disk and says nothing when TestPermissions fails", async () => {
    const a = storageApi({ disks, perms: () => Promise.reject(new ConnectError("the apiserver is unavailable", Code.Unavailable)) });
    render(<DiskList />, { wrapper: withApi(a) });
    await within(await screen.findByRole("table")).findByText("scratch");
    await waitFor(() => expect(a.calls).toContain("TestPermissions"));
    expect(screen.queryByRole("button", { name: "New disk" })).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.queryByText(/unavailable/)).toBeNull();
  });
});
