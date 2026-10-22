// @vitest-environment jsdom

import { Code, ConnectError } from "@connectrpc/connect";
import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import { clone, equals } from "@bufbuild/protobuf";
import { DiskPhase, DiskSchema, type ApplyDiskRequest, type DeleteDiskRequest, type Disk } from "@hangar/api/hangar/storage/v1alpha1/disk_pb";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { DiskDetail } from "@/pages/disk-detail";
import { never, polyfillDialog, withApi } from "@/test/harness";
import { disk, image, storageApi, type Fake } from "@/test/storage";

const navigate = vi.fn();
vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  useNavigate: () => navigate,
  Link: (await import("@/test/fake-link")).FakeLink,
}));

beforeAll(polyfillDialog);
beforeEach(() => navigate.mockReset());
afterEach(cleanup);

const attached = disk("pg-data", { size: "20Gi", phase: DiskPhase.IN_USE, machine: "vm-postgres", version: 7n });
const free = disk("scratch", { size: "10Gi" });

/** An API whose list and Get hold the disks given. */
function api(disks: Disk[], f: Fake = {}) {
  return storageApi({ disks: async () => ({ items: disks }), get: (n) => disks.find((d) => d.metadata?.name === n), ...f });
}

describe("DiskDetail", () => {
  it("shows skeletons while the disks load", async () => {
    render(<DiskDetail name="pg-data" />, { wrapper: withApi(storageApi({ disks: never })) });
    expect((await screen.findAllByLabelText("Loading")).length).toBeGreaterThan(0);
  });

  it("says the disk did not load, with the API's error", async () => {
    const a = storageApi({ disks: () => Promise.reject(new ConnectError("the apiserver is unavailable", Code.Unavailable)) });
    render(<DiskDetail name="pg-data" />, { wrapper: withApi(a) });
    await screen.findByText("The disk did not load");
    expect(screen.getByText("the apiserver is unavailable")).toBeTruthy();
  });

  it("says the project has no disk of the name", async () => {
    render(<DiskDetail name="gone" />, { wrapper: withApi(api([free])) });
    await screen.findByText("No such disk");
  });

  it("shows the disk's source, its size, node and the VM it is attached to", async () => {
    render(<DiskDetail name="pg-data" />, { wrapper: withApi(api([attached])) });
    await screen.findByRole("heading", { name: "pg-data" });
    await waitFor(() => expect(screen.getAllByRole("link", { name: "postgres" }).length).toBeGreaterThan(0));
    for (const l of screen.getAllByRole("link", { name: "postgres" })) expect(l.getAttribute("href")).toBe("/projects/demo/vms/postgres");
    expect(screen.getByText("Source")).toBeTruthy();
    expect(screen.getByText("Blank")).toBeTruthy();
    expect(screen.getByText("node-a")).toBeTruthy();
    expect(screen.getAllByText(/hangar get disk pg-data -p demo/).length).toBeGreaterThan(0);
  });

  it("says what the disk is used as and what it was made as", async () => {
    const d = disk("pg-data", { phase: DiskPhase.IN_USE, machine: "vm-postgres", labels: { "hangar/intended-use": "data" } });
    render(<DiskDetail name="pg-data" />, { wrapper: withApi(api([d])) });
    await screen.findByRole("heading", { name: "pg-data" });
    expect(screen.getByText("Used as")).toBeTruthy();
    expect(screen.getAllByText((_, el) => el?.textContent === "Data disk of postgres · vdb").length).toBeGreaterThan(0);
    // "Data disk" alone is the Made as field; the Used as line holds more.
    expect(screen.getByText("Data disk")).toBeTruthy();
  });

  it("says it was not recorded what a disk made outside the console was made as", async () => {
    render(<DiskDetail name="scratch" />, { wrapper: withApi(api([free])) });
    await screen.findByRole("heading", { name: "scratch" });
    expect(screen.getByText("Not recorded")).toBeTruthy();
    expect(screen.getByText("Not attached")).toBeTruthy();
  });

  it("names the VM a root disk was made for, and links its image", async () => {
    const root = disk("postgres-root", { phase: DiskPhase.IN_USE, machine: "vm-postgres", rootOf: "postgres", image: "debian-13" });
    render(<DiskDetail name="postgres-root" />, { wrapper: withApi(api([root])) });
    await screen.findByText(", root disk; deleted with it");
    expect(screen.getByRole("link", { name: "debian-13" }).getAttribute("href")).toBe("/projects/demo/images/debian-13");
  });

  it("says why its node has not made the disk", async () => {
    const d = disk("build-root", { phase: DiskPhase.PENDING, ready: [ConditionStatus.FALSE, "ImagePullFailed", "fetching the image: 404 Not Found"] });
    render(<DiskDetail name="build-root" />, { wrapper: withApi(api([d])) });
    await screen.findByText("build-root is not ready: ImagePullFailed");
    expect(within(screen.getByRole("alert")).getByText("fetching the image: 404 Not Found")).toBeTruthy();
  });

  it("says why a growth is not granted, next to the size it is growing to", async () => {
    const message = "node node-a has 5Gi of disk free, and growing the disk to 200Gi takes 100Gi more; it keeps 100Gi";
    const d = disk("pg-data", { size: "200Gi", have: "100Gi", phase: DiskPhase.IN_USE, allocated: [ConditionStatus.FALSE, "ResizeDoesNotFit", message] });
    render(<DiskDetail name="pg-data" />, { wrapper: withApi(api([d])) });
    await screen.findByText("pg-data is not growing: ResizeDoesNotFit");
    expect(within(screen.getByRole("alert")).getByText(message)).toBeTruthy();
    expect(screen.getByText("· ResizeDoesNotFit")).toBeTruthy();
  });

  it("says a disk not placed yet is not placed, and no growth is blocked", async () => {
    const d = disk("models", {
      phase: DiskPhase.PENDING,
      allocated: [ConditionStatus.FALSE, "WaitingForFirstMachine", "the disk is made on the node of the first machine it is attached to"],
    });
    render(<DiskDetail name="models" />, { wrapper: withApi(api([d])) });
    await screen.findByText("Not placed yet");
    expect(screen.queryByRole("alert")).toBeNull();
  });
});

describe("Grow and Delete, by permission", () => {
  const heading = () => screen.findByRole("heading", { name: "pg-data" });
  const shown = (name: string) => screen.queryByRole("button", { name }) !== null;

  it("shows Grow for storage.disks.apply and Delete for storage.disks.delete, each on its own", async () => {
    const a = api([attached], { perms: async () => ["storage.disks.apply"] });
    render(<DiskDetail name="pg-data" />, { wrapper: withApi(a) });
    await screen.findByRole("button", { name: "Grow" });
    await waitFor(() => expect(a.calls.filter((c) => c === "TestPermissions")).toHaveLength(1));
    expect(shown("Delete")).toBe(false);
    cleanup();
    const b = api([attached], { perms: async () => ["storage.disks.delete"] });
    render(<DiskDetail name="pg-data" />, { wrapper: withApi(b) });
    await screen.findByRole("button", { name: "Delete" });
    await waitFor(() => expect(b.calls.filter((c) => c === "TestPermissions")).toHaveLength(1));
    expect(shown("Grow")).toBe(false);
  });

  it("hides both and says nothing while the answer is unknown", async () => {
    render(<DiskDetail name="pg-data" />, { wrapper: withApi(api([attached], { perms: never })) });
    await heading();
    expect(shown("Grow")).toBe(false);
    expect(shown("Delete")).toBe(false);
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("hides both and says nothing when TestPermissions fails", async () => {
    const a = api([attached], { perms: () => Promise.reject(new ConnectError("the apiserver is unavailable", Code.Unavailable)) });
    render(<DiskDetail name="pg-data" />, { wrapper: withApi(a) });
    await heading();
    await waitFor(() => expect(a.calls.filter((c) => c === "TestPermissions")).toHaveLength(1));
    expect(shown("Grow")).toBe(false);
    expect(shown("Delete")).toBe(false);
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.queryByText(/unavailable/)).toBeNull();
  });
});

describe("Grow", () => {
  const open = async () => {
    fireEvent.click(await screen.findByRole("button", { name: "Grow" }));
    return screen.getByRole("dialog", { name: /Grow disk/ });
  };
  const size = (d: HTMLElement, v: string) => fireEvent.change(within(d).getByLabelText("Size, GiB"), { target: { value: v } });

  it("says growing restarts the VM the disk is attached to", async () => {
    render(<DiskDetail name="pg-data" />, { wrapper: withApi(api([attached])) });
    await waitFor(() => expect(screen.getAllByRole("link", { name: "postgres" }).length).toBeGreaterThan(0));
    const d = await open();
    expect(within(d).getByText("Growing it restarts postgres.")).toBeTruthy();
    expect(within(d).getByText("Its node holds 20 GiB.")).toBeTruthy();
  });

  it("says nothing of restarts for a disk attached to no machine", async () => {
    render(<DiskDetail name="scratch" />, { wrapper: withApi(api([free])) });
    const d = await open();
    expect(within(d).queryByText(/restart/)).toBeNull();
  });

  it("applies the disk Get returned, newer than the list, with only spec.size changed", async () => {
    const fields = { size: "20Gi", image: "debian-13", labels: { app: "postgres" }, rootOf: "postgres", phase: DiskPhase.IN_USE, machine: "vm-postgres" };
    const listed = disk("postgres-root", { ...fields, version: 7n });
    const got = disk("postgres-root", { ...fields, version: 9n, labels: { app: "postgres", tier: "db" } });
    const applied: ApplyDiskRequest[] = [];
    const a = storageApi({
      disks: async () => ({ items: [listed] }),
      get: () => clone(DiskSchema, got),
      apply: (req) => (applied.push(req), { disk: req.disk, created: false }),
    });
    render(<DiskDetail name="postgres-root" />, { wrapper: withApi(a) });
    const d = await open();
    size(d, "50");
    fireEvent.click(within(d).getByRole("button", { name: "Grow to 50 GiB" }));
    await waitFor(() => expect(applied).toHaveLength(1));
    expect(a.calls.filter((c) => c === "GetDisk" || c === "ApplyDisk")).toEqual(["GetDisk", "ApplyDisk"]);
    const want = clone(DiskSchema, got);
    want.spec!.size = "50Gi";
    expect(equals(DiskSchema, applied[0].disk!, want)).toBe(true);
  });

  it("withdraws a growth not granted, down to what the node holds", async () => {
    const growing = disk("pg-data", { size: "200Gi", have: "100Gi", phase: DiskPhase.IN_USE, allocated: [ConditionStatus.FALSE, "ResizeDoesNotFit", "no room"] });
    const applied: ApplyDiskRequest[] = [];
    const a = api([growing], { apply: (req) => (applied.push(req), { disk: req.disk, created: false }) });
    render(<DiskDetail name="pg-data" />, { wrapper: withApi(a) });
    const d = await open();
    expect(within(d).getByText("spec.size is 200 GiB; its node holds 100 GiB.")).toBeTruthy();
    expect(within(d).getByText("At least 100 GiB.")).toBeTruthy();
    size(d, "99");
    await within(d).findByText("Its node holds 100 GiB; a disk never shrinks below that.");
    expect((within(d).getByRole("button", { name: "Set to 99 GiB" }) as HTMLButtonElement).disabled).toBe(true);
    size(d, "100");
    fireEvent.click(within(d).getByRole("button", { name: "Set to 100 GiB" }));
    await waitFor(() => expect(applied).toHaveLength(1));
    expect(applied[0].disk?.spec?.size).toBe("100Gi");
  });

  it("holds a disk of an image to the image's smallest disk", async () => {
    const d0 = disk("debian-test", { size: "20Gi", phase: DiskPhase.PENDING, image: "debian-13" });
    const a = storageApi({ disks: async () => ({ items: [d0] }), images: async () => ({ items: [image("debian-13", { size: "3Gi", min: "10Gi" })] }) });
    render(<DiskDetail name="debian-test" />, { wrapper: withApi(a) });
    // The image's floor is known once the images have loaded.
    await waitFor(() => expect(a.calls).toContain("ListImages"));
    const d = await open();
    expect(within(d).getByText("It is not placed yet; spec.size is 20 GiB.")).toBeTruthy();
    size(d, "8");
    await within(d).findByText("debian-13 needs 10 GiB; 8 GiB is less.");
    expect(a.calls).not.toContain("ApplyDisk");
  });

  it("refuses the size the disk already has, and sends nothing", async () => {
    const a = api([attached]);
    render(<DiskDetail name="pg-data" />, { wrapper: withApi(a) });
    const d = await open();
    size(d, "20");
    await within(d).findByText("pg-data is already set to 20 GiB.");
    expect(a.calls).not.toContain("ApplyDisk");
  });

  it("applies nothing when the disk read already asks for the size, and says so", async () => {
    // The list shows 20Gi; someone has grown it to 60Gi since.
    const a = api([attached], { get: () => disk("pg-data", { size: "60Gi", phase: DiskPhase.IN_USE, machine: "vm-postgres", version: 8n }) });
    render(<DiskDetail name="pg-data" />, { wrapper: withApi(a) });
    const d = await open();
    size(d, "50");
    fireEvent.click(within(d).getByRole("button", { name: "Grow to 50 GiB" }));
    await within(d).findByText("pg-data is now set to 60 GiB, not 20 GiB as shown; nothing is applied.");
    expect(a.calls).toContain("GetDisk");
    expect(a.calls).not.toContain("ApplyDisk");
  });

  it("shows the API's refusal in the dialog", async () => {
    const a = api([attached], {
      apply: () => {
        throw new ConnectError("quota exceeded: the disk limit is 100Gi", Code.ResourceExhausted);
      },
    });
    render(<DiskDetail name="pg-data" />, { wrapper: withApi(a) });
    const d = await open();
    fireEvent.click(within(d).getByRole("button", { name: /Grow to/ }));
    await within(d).findByText("Changing the size failed");
    expect(within(d).getByText("quota exceeded: the disk limit is 100Gi")).toBeTruthy();
  });
});

describe("Delete", () => {
  const open = async () => {
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));
    return screen.getByRole("dialog", { name: /Delete disk/ });
  };

  it("asks for the name, sends DeleteDisk, then opens the list", async () => {
    const deleted: DeleteDiskRequest[] = [];
    const a = api([free], { del: (req) => void deleted.push(req) });
    render(<DiskDetail name="scratch" />, { wrapper: withApi(a) });
    const d = await open();
    expect(within(d).getByText("Its data is deleted with it.")).toBeTruthy();
    const button = within(d).getByRole("button", { name: "Delete" }) as HTMLButtonElement;
    expect(button.disabled).toBe(true);
    fireEvent.change(within(d).getByRole("textbox"), { target: { value: "scratch" } });
    fireEvent.click(button);
    await waitFor(() => expect(navigate).toHaveBeenCalledWith({ to: "/projects/$project/disks", params: { project: "demo" } }));
    expect(deleted.map((r) => [r.project, r.name])).toEqual([["demo", "scratch"]]);
  });

  it("opens the list even when the page finds the disk gone before every query is asked again", async () => {
    // The disk is gone as soon as it is deleted, and another of the page's
    // queries is slow to answer again: the page says "No such disk" first,
    // which takes away the dialog that deleted it.
    let disks = [free];
    let deleted = false;
    const a = storageApi({
      disks: async () => ({ items: disks }),
      get: (n) => disks.find((d) => d.metadata?.name === n),
      images: () => (deleted ? never() : Promise.resolve({ items: [] })),
      del: () => {
        disks = [];
        deleted = true;
      },
    });
    render(<DiskDetail name="scratch" />, { wrapper: withApi(a) });
    const d = await open();
    fireEvent.change(within(d).getByRole("textbox"), { target: { value: "scratch" } });
    fireEvent.click(within(d).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(navigate).toHaveBeenCalledWith({ to: "/projects/$project/disks", params: { project: "demo" } }));
  });

  it("says an attached disk is refused, and shows the API's refusal in the dialog", async () => {
    const a = api([attached], {
      del: () => {
        throw new ConnectError('disk "pg-data" is attached to machine vm-postgres; detach it first', Code.FailedPrecondition);
      },
    });
    render(<DiskDetail name="pg-data" />, { wrapper: withApi(a) });
    await waitFor(() => expect(screen.getAllByRole("link", { name: "postgres" }).length).toBeGreaterThan(0));
    const d = await open();
    expect(within(d).getByText("pg-data is attached to postgres, which is not being deleted.")).toBeTruthy();
    fireEvent.change(within(d).getByRole("textbox"), { target: { value: "pg-data" } });
    fireEvent.click(within(d).getByRole("button", { name: "Delete" }));
    await within(d).findByText("Deleting failed");
    expect(within(d).getByText('disk "pg-data" is attached to machine vm-postgres; detach it first')).toBeTruthy();
    expect(navigate).not.toHaveBeenCalled();
  });

  it("states no refusal when the attached machine is being deleted, and deletes", async () => {
    const deleted: DeleteDiskRequest[] = [];
    const a = api([attached], { machineDeleting: true, del: (req) => void deleted.push(req) });
    render(<DiskDetail name="pg-data" />, { wrapper: withApi(a) });
    await waitFor(() => expect(screen.getAllByRole("link", { name: "postgres" }).length).toBeGreaterThan(0));
    const d = await open();
    expect(within(d).getByText("Its data is deleted with it, once postgres, which is being deleted, is gone.")).toBeTruthy();
    expect(within(d).queryByText(/refuses/)).toBeNull();
    fireEvent.change(within(d).getByRole("textbox"), { target: { value: "pg-data" } });
    fireEvent.click(within(d).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(deleted).toHaveLength(1));
  });
});
