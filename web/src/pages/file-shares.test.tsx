// @vitest-environment jsdom

import { Code, ConnectError } from "@connectrpc/connect";
import type { ApplyFileShareRequest, DeleteFileShareRequest } from "@hangar/api/hangar/storage/v1alpha1/file_share_pb";
import { FileSharePhase } from "@hangar/api/hangar/storage/v1alpha1/file_share_pb";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { VmFileSharesCard } from "@/components/file-shares";
import { vmView } from "@/lib/machines";
import { CreateFileShare } from "@/pages/create-file-share";
import { FileShareDetail } from "@/pages/file-share-detail";
import { FileShareList } from "@/pages/file-share-list";
import { never, polyfillDialog, withApi } from "@/test/harness";
import { container, machineOf, settledPermissions, share, sharesApi, vm, type Fake } from "@/test/file-shares";

const navigate = vi.fn();
vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  useNavigate: () => navigate,
  Link: (await import("@/test/fake-link")).FakeLink,
}));

beforeAll(polyfillDialog);
beforeEach(() => navigate.mockReset());
afterEach(cleanup);

// cache is on default and data; etl mounts it from data, and four ingest
// containers too, web from default. scratch is on default and served by no
// one, as every share is until its server runs.
const cache = share("cache", { networks: ["default", "data"], size: "100Gi", phase: FileSharePhase.IN_USE, addresses: { default: "10.0.0.8", data: "10.1.0.10" } });
const scratch = share("scratch", { networks: ["default"], size: "10Gi" });
const containers = [
  container("web", [], [{ share: "cache", target: "/cache" }]),
  container("etl", ["data"], [{ share: "cache", target: "/cache" }]),
  ...["ingest-1", "ingest-2", "ingest-3", "ingest-4"].map((n) => container(n, ["data"], [{ share: "cache", target: "/cache" }])),
  container("probe", ["ops"]),
];
const down = new ConnectError("the apiserver is unavailable", Code.Unavailable);

/** Renders a page over the fake API and waits until its TestPermissions answer came. */
async function show(page: React.ReactNode, f: Fake) {
  const api = sharesApi(f);
  render(page, { wrapper: withApi(api) });
  await settledPermissions(api, waitFor);
  return api;
}

/** The element whose own text, children's included, is exactly text: copy with names in the monospace face spans several. */
const text = (t: string) => (_: string, el: Element | null) => el?.textContent === t && [...(el?.children ?? [])].every((c) => c.textContent !== t);

/** The rows of the Used by table. */
const usedByRows = () => screen.getByRole("columnheader", { name: "Path" }).closest("table")!.querySelectorAll("tbody tr");
/** The rows of Other machines able to use the share, each with its snippet's field name. */
const otherRows = () => screen.queryAllByText(/^spec\.(volumes|fileShares)$/);

describe("FileShareList", () => {
  it("lists each share's networks with its address on each, or that it is not served yet", async () => {
    await show(<FileShareList />, { shares: [cache, scratch], containers });
    const row = (await screen.findByRole("link", { name: "scratch" })).closest("tr")!;
    expect(within(row).getByText("Not served yet")).toBeTruthy();
    expect(within(row).getByText("Pending")).toBeTruthy();
    const cacheRow = screen.getByRole("link", { name: "cache" }).closest("tr")!;
    expect(within(cacheRow).getByText("10.1.0.10")).toBeTruthy();
  });

  it("names two machines using a share, and the rest behind +N more", async () => {
    await show(<FileShareList />, { shares: [cache], containers });
    const row = (await screen.findByRole("link", { name: "cache" })).closest("tr")!;
    await within(row).findByText("6 machines");
    expect(within(row).getByText("etl, ingest-1")).toBeTruthy();
    expect(within(row).getByRole("button", { name: /\+4 more/ })).toBeTruthy();
  });

  it("says the machines did not load, never None, when they failed", async () => {
    await show(<FileShareList />, { shares: [cache, scratch], containers, machinesFail: down });
    const row = (await screen.findByRole("link", { name: "scratch" })).closest("tr")!;
    await within(row).findByText("Not loaded");
    expect(within(row).queryByText("None")).toBeNull();
    expect(screen.getByText(/The project's machines did not load/)).toBeTruthy();
  });

  it("offers New file share only to whom may create one", async () => {
    await show(<FileShareList />, { shares: [cache], perms: () => [] });
    await screen.findByRole("link", { name: "cache" });
    expect(screen.queryByRole("link", { name: /New file share/ })).toBeNull();
  });
});

describe("FileShareDetail", () => {
  it("gives each network's address and which machines use it from where, five rows until Show all", async () => {
    await show(<FileShareDetail name="cache" />, { shares: [cache, scratch], containers });
    await screen.findByText("Show all 6");
    // etl mounts from data, at the share's address there.
    const etl = screen.getAllByRole("link", { name: "etl" }).find((l) => l.closest("tr"))!.closest("tr")!;
    expect(within(etl).getByText(/data/)).toBeTruthy();
    expect(within(etl).getByText(/10\.1\.0\.10/)).toBeTruthy();
    expect(usedByRows()).toHaveLength(5);
    fireEvent.click(screen.getByText("Show all 6"));
    expect(usedByRows()).toHaveLength(6);
    // probe is on ops alone, so cannot use it.
    expect(await screen.findByText(/not on/)).toBeTruthy();
    expect(screen.getByRole("button", { name: /Add network ops to this share/ })).toBeTruthy();
  });

  it("lists five of the machines able to use it until Show all", async () => {
    const idle = ["a1", "a2", "a3", "a4", "a5", "a6", "a7"].map((n) => container(n));
    await show(<FileShareDetail name="scratch" />, { shares: [scratch], containers: idle });
    await screen.findByText("Show all 7");
    expect(otherRows()).toHaveLength(5);
    fireEvent.click(screen.getByText("Show all 7"));
    expect(otherRows()).toHaveLength(7);
  });

  it("says a share no server serves yet has no address and no mount command", async () => {
    await show(<FileShareDetail name="scratch" />, { shares: [scratch], containers });
    await screen.findByRole("heading", { name: "scratch" });
    expect(screen.getAllByText("Not served yet").length).toBeGreaterThan(0);
    expect(screen.getByText(/Not served yet: the share has no address on any network/)).toBeTruthy();
    expect(screen.queryByText(/mount -t nfs4/)).toBeNull();
  });

  it("gives a VM's mount commands with its client identity set first", async () => {
    await show(<FileShareDetail name="cache" />, { shares: [cache], containers });
    expect(await screen.findAllByText("echo MACHINE_ID > /sys/module/nfs/parameters/nfs4_unique_id")).toHaveLength(2);
    expect(screen.getByText("mount -t nfs4 -o nosuid,nodev,timeo=50 10.1.0.10:/ /mnt/cache")).toBeTruthy();
  });

  it("shows a held back container as held back, with the API's reason", async () => {
    const held = container("thumbs", [], [{ share: "scratch", target: "/s" }], 'failed_precondition: file share "scratch" is on no node yet');
    await show(<FileShareDetail name="scratch" />, { shares: [scratch], containers: [held] });
    expect(await screen.findByText('1 container held back: file share "scratch" is on no node yet')).toBeTruthy();
    const row = screen.getAllByRole("link", { name: "thumbs" }).find((l) => l.closest("tr"))!.closest("tr")!;
    expect(within(row).getByText("Held back")).toBeTruthy();
  });

  it("refuses to delete a share a live machine declares, saying which", async () => {
    const del = vi.fn<(r: DeleteFileShareRequest) => void>();
    await show(<FileShareDetail name="cache" />, { shares: [cache], containers, del });
    fireEvent.click(await screen.findByRole("button", { name: /Delete/ }));
    const dialog = await screen.findByRole("dialog");
    await within(dialog).findByText(/still declares? it/);
    fireEvent.change(within(dialog).getByLabelText("Name, to confirm"), { target: { value: "cache" } });
    expect((within(dialog).getByRole("button", { name: /^Delete$/ }) as HTMLButtonElement).disabled).toBe(true);
    expect(del).not.toHaveBeenCalled();
  });

  it("deletes a share only a machine being deleted declares, saying it goes once that machine is gone", async () => {
    const del = vi.fn<(r: DeleteFileShareRequest) => void>();
    const leaving = container("old", [], [{ share: "scratch", target: "/s" }], undefined, true);
    // A status.users entry alone, which nothing declares now, does not hold it either.
    const listed = share("scratch", { networks: ["default"], size: "10Gi", users: [{ kind: "Container", name: "gone", network: "default", path: "/s" }] });
    await show(<FileShareDetail name="scratch" />, { shares: [listed], containers: [leaving], del });
    fireEvent.click(await screen.findByRole("button", { name: /Delete/ }));
    const dialog = await screen.findByRole("dialog");
    expect(await within(dialog).findByText(/which is being deleted/)).toBeTruthy();
    expect(within(dialog).queryByText(/The API refuses/)).toBeNull();
    fireEvent.change(within(dialog).getByLabelText("Name, to confirm"), { target: { value: "scratch" } });
    await waitFor(() => expect((within(dialog).getByRole("button", { name: /^Delete$/ }) as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(within(dialog).getByRole("button", { name: /^Delete$/ }));
    await waitFor(() => expect(del).toHaveBeenCalledWith(expect.objectContaining({ project: "demo", name: "scratch" })));
  });

  it("keeps Delete off while the machines did not load, saying so", async () => {
    await show(<FileShareDetail name="scratch" />, { shares: [scratch], containers, machinesFail: down });
    fireEvent.click(await screen.findByRole("button", { name: /Delete/ }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/which machines declare this share is not known/)).toBeTruthy();
    fireEvent.change(within(dialog).getByLabelText("Name, to confirm"), { target: { value: "scratch" } });
    expect((within(dialog).getByRole("button", { name: /^Delete$/ }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("refuses to remove a network machines mount the share from, on its row and above the button", async () => {
    await show(<FileShareDetail name="cache" />, { shares: [cache], containers });
    await screen.findByText("Show all 6");
    fireEvent.click(screen.getByRole("button", { name: /Edit networks/ }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getAllByRole("checkbox")[1]!);
    expect(within(dialog).getByText(text("Removing data is refused"))).toBeTruthy();
    expect(within(dialog).getByText(text("Removing data is refused while the 5 machines using cache from data declare it."))).toBeTruthy();
    expect((within(dialog).getByRole("button", { name: /Remove data/ }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("removes no network while who mounts from it is not known", async () => {
    await show(<FileShareDetail name="cache" />, { shares: [cache], containers, machinesFail: down });
    fireEvent.click(await screen.findByRole("button", { name: /Edit networks/ }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getAllByRole("checkbox")[1]!);
    expect(within(dialog).getByText("Which machines mount from the networks removed is not known")).toBeTruthy();
    expect((within(dialog).getByRole("button", { name: /Remove data/ }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("adds a network at the version the dialog opened on", async () => {
    const apply = vi.fn<(r: ApplyFileShareRequest) => void>();
    await show(<FileShareDetail name="cache" />, { shares: [cache], containers, apply });
    fireEvent.click(await screen.findByRole("button", { name: /Add network ops to this share/ }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: /Add ops/ }));
    await waitFor(() => expect(apply).toHaveBeenCalled());
    const req = apply.mock.calls[0]![0];
    expect(req.createOnly).toBe(false);
    expect(req.fileShare?.spec?.networks).toEqual(["default", "data", "ops"]);
    expect(req.fileShare?.metadata?.resourceVersion).toBe(5n);
  });

  it("shows no control to whom may change no share, and their equivalents neither", async () => {
    await show(<FileShareDetail name="cache" />, { shares: [cache], containers, perms: () => [] });
    await screen.findByText("Show all 6");
    for (const b of [/Edit networks/, /Grow/, /Delete/, /Add network ops/]) expect(screen.queryByRole("button", { name: b })).toBeNull();
    expect(screen.queryByText(/hangar delete fileshare/)).toBeNull();
  });
});

describe("CreateFileShare", () => {
  it("states that the caller's roles do not include creating shares", async () => {
    await show(<CreateFileShare />, { perms: () => [] });
    expect(await screen.findByText("This account's roles do not include creating file shares in this project.")).toBeTruthy();
  });

  it("shows nothing while whether the caller may is unknown", async () => {
    const api = sharesApi({ perms: never });
    render(<CreateFileShare />, { wrapper: withApi(api) });
    await waitFor(() => expect(api.calls).toContain("TestPermissions"));
    expect(screen.queryByLabelText("File share name")).toBeNull();
    expect(screen.queryByText(/roles do not include/)).toBeNull();
  });

  it("creates the share on the networks chosen with one create-only apply, then opens it", async () => {
    const apply = vi.fn<(r: ApplyFileShareRequest) => void>();
    await show(<CreateFileShare networks={["data"]} />, { shares: [cache], containers, apply });
    fireEvent.change(await screen.findByLabelText("File share name"), { target: { value: "media" } });
    // Each network card counts its machines.
    const data = await screen.findByRole("checkbox", { name: "data" });
    await within(data).findByText("5 machines");
    fireEvent.click(await screen.findByRole("checkbox", { name: "ops" }));
    const submit = screen.getByRole("button", { name: "Create file share" }) as HTMLButtonElement;
    await waitFor(() => expect(submit.disabled).toBe(false));
    fireEvent.click(submit);
    await waitFor(() => expect(apply).toHaveBeenCalled());
    const req = apply.mock.calls[0]![0];
    expect(req.createOnly).toBe(true);
    expect(req.fileShare?.spec?.networks).toEqual(["data", "ops"]);
    expect(req.fileShare?.spec?.size).toBe("50Gi");
    await waitFor(() => expect(navigate).toHaveBeenCalledWith(expect.objectContaining({ params: { project: "demo", name: "media" }, replace: true })));
  });

  it("refuses a name the project's shares hold, and no network", async () => {
    await show(<CreateFileShare />, { shares: [cache] });
    fireEvent.change(await screen.findByLabelText("File share name"), { target: { value: "cache" } });
    fireEvent.click(await screen.findByRole("checkbox", { name: "default" }));
    expect(await screen.findByText(/A file share named cache exists in this project\./)).toBeTruthy();
    expect(screen.getByText(/No network is chosen\./)).toBeTruthy();
  });
});

describe("VmFileSharesCard", () => {
  it("gives the mount commands from the VM's own network, its client identity first", async () => {
    const db = vm("db", ["data"], [{ name: "cache", readOnly: true }]);
    const m = vmView(db, machineOf("VirtualMachine", db));
    render(<VmFileSharesCard m={m} />, { wrapper: withApi(sharesApi({ shares: [cache], vms: [db] })) });
    expect(await screen.findByText("10.1.0.10")).toBeTruthy();
    expect(screen.getByText("echo muid-db > /sys/module/nfs/parameters/nfs4_unique_id")).toBeTruthy();
    expect(screen.getByText("mount -t nfs4 -o ro,nosuid,nodev,timeo=50 10.1.0.10:/ /mnt/cache")).toBeTruthy();
  });

  it("says a share is not served yet, and why it is not mounted", async () => {
    const db = vm("db", [], [{ name: "scratch" }], "the machine is on no node yet");
    const m = vmView(db, machineOf("VirtualMachine", db));
    render(<VmFileSharesCard m={m} />, { wrapper: withApi(sharesApi({ shares: [scratch], vms: [db] })) });
    expect(await screen.findByText(/no address: not served yet/)).toBeTruthy();
    expect(screen.getByText("Not mounted: the machine is on no node yet")).toBeTruthy();
    expect(screen.queryByText(/mount -t nfs4/)).toBeNull();
  });
});
