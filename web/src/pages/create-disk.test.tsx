// @vitest-environment jsdom

import { Code, ConnectError } from "@connectrpc/connect";
import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import type { TestPermissionsRequest } from "@hangar/api/hangar/iam/v1alpha1/account_pb";
import type { ApplyDiskRequest, Disk } from "@hangar/api/hangar/storage/v1alpha1/disk_pb";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { diskYaml } from "@/components/storage";
import { newDisk } from "@/lib/storage";
import { CreateDisk } from "@/pages/create-disk";
import { deferred, never, withApi } from "@/test/harness";
import { disk, image, storageApi } from "@/test/storage";

const navigate = vi.fn();
vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  useNavigate: () => navigate,
  Link: (await import("@/test/fake-link")).FakeLink,
}));

beforeEach(() => navigate.mockReset());
afterEach(cleanup);

const verified = image("debian-13", { size: "3Gi", min: "10Gi", verified: [ConditionStatus.TRUE, "Verified", ""] });
const large = image("ubuntu-2404", { size: "30Gi", verified: [ConditionStatus.TRUE, "Verified", ""] });
const faulty = image("alpine-3", { verified: [ConditionStatus.FALSE, "HashMismatch", "The file does not match its sha256."] });
const images = async () => ({ items: [verified, faulty, large] });

const submitButton = () => screen.getByRole("button", { name: /Create disk|Loading names|Creating/ }) as HTMLButtonElement;
const type = (label: string, value: string) => fireEvent.change(screen.getByLabelText(label), { target: { value } });
const manifest = () => screen.getByText(/apiVersion: storage\.hangarlab\.cloud/).textContent ?? "";

describe("New disk", () => {
  it("makes a blank data disk by default, labelled data, with one create-only apply, then opens it in place of this page", async () => {
    const applied: ApplyDiskRequest[] = [];
    const a = storageApi({ disks: async () => ({ items: [disk("scratch")] }), images, apply: (req) => (applied.push(req), { disk: req.disk, created: true }) });
    render(<CreateDisk />, { wrapper: withApi(a) });
    expect((await screen.findByRole("radio", { name: "Data disk" })).getAttribute("aria-checked")).toBe("true");
    expect(screen.queryByRole("radiogroup", { name: "Image" })).toBeNull();
    expect(screen.getByText(/It is blank, with no filesystem: format and mount it in the VM, such as with mkfs\.ext4 \/dev\/vdb\./)).toBeTruthy();
    type("Disk name", "data");
    type("Size, GiB", "12");
    await waitFor(() => expect(submitButton().disabled).toBe(false));
    expect(manifest()).toContain("hangar/intended-use: data");
    expect(screen.getAllByText(/hangar apply --create-only -p demo -f data\.yaml/).length).toBeGreaterThan(0);
    fireEvent.click(submitButton());
    await waitFor(() => expect(navigate).toHaveBeenCalledWith({ to: "/projects/$project/disks/$name", params: { project: "demo", name: "data" }, replace: true }));
    expect(a.calls.filter((c) => c === "GetDisk" || c === "ApplyDisk")).toEqual(["ApplyDisk"]);
    expect(applied).toHaveLength(1);
    expect(applied[0].createOnly).toBe(true);
    const sent = applied[0].disk!;
    expect(sent.metadata?.name).toBe("data");
    expect(sent.metadata?.project).toBe("demo");
    expect(sent.metadata?.labels).toEqual({ "hangar/intended-use": "data" });
    expect(sent.spec?.size).toBe("12Gi");
    expect(sent.spec?.source?.source.case).toBeUndefined();
  });

  it("makes a boot disk of the image chosen, labelled boot, and offers only usable images, each with its smallest disk", async () => {
    const applied: ApplyDiskRequest[] = [];
    const a = storageApi({ images, apply: (req) => (applied.push(req), { disk: req.disk, created: true }) });
    render(<CreateDisk />, { wrapper: withApi(a) });
    fireEvent.click(await screen.findByRole("radio", { name: "Boot disk" }));
    await screen.findByRole("radio", { name: "debian-13" });
    expect(screen.queryByRole("radio", { name: "alpine-3" })).toBeNull();
    expect(screen.getByText("10 GiB or more")).toBeTruthy();
    expect(screen.getByText("30 GiB or more")).toBeTruthy();
    expect(screen.queryByText(/mkfs\.ext4/)).toBeNull();
    fireEvent.click(screen.getByRole("radio", { name: "ubuntu-2404" }));
    // The size follows the image's smallest disk until one is typed.
    expect((screen.getByLabelText("Size, GiB") as HTMLInputElement).value).toBe("30");
    type("Disk name", "root");
    await waitFor(() => expect(submitButton().disabled).toBe(false));
    expect(manifest()).toContain("hangar/intended-use: boot");
    fireEvent.click(submitButton());
    await waitFor(() => expect(applied).toHaveLength(1));
    const sent = applied[0].disk!;
    expect(sent.metadata?.labels).toEqual({ "hangar/intended-use": "boot" });
    expect(sent.spec?.size).toBe("30Gi");
    expect(sent.spec?.source?.source).toEqual(expect.objectContaining({ case: "image", value: expect.objectContaining({ name: "ubuntu-2404" }) }));
  });

  it("opens on a boot disk of the image its address names", async () => {
    render(<CreateDisk image="ubuntu-2404" />, { wrapper: withApi(storageApi({ images })) });
    expect((await screen.findByRole("radio", { name: "Boot disk" })).getAttribute("aria-checked")).toBe("true");
    expect((await screen.findByRole("radio", { name: "ubuntu-2404" })).getAttribute("aria-checked")).toBe("true");
    expect(screen.getByRole("radio", { name: "debian-13" }).getAttribute("aria-checked")).toBe("false");
  });

  it("refuses a size below the image's smallest disk, and sends nothing", async () => {
    const a = storageApi({ images });
    render(<CreateDisk image="debian-13" />, { wrapper: withApi(a) });
    await screen.findByRole("radio", { name: "debian-13" });
    type("Disk name", "data");
    type("Size, GiB", "5");
    expect((await screen.findAllByText("debian-13 needs 10 GiB; 5 GiB is less.")).length).toBeGreaterThan(0);
    expect(submitButton().disabled).toBe(true);
    fireEvent.submit(submitButton().closest("form")!);
    expect(a.calls).not.toContain("ApplyDisk");
  });

  it("refuses a name the list holds, and sends nothing while the list loads", async () => {
    const list = deferred<{ items: Disk[] }>();
    const a = storageApi({ disks: () => list.promise });
    render(<CreateDisk />, { wrapper: withApi(a) });
    await screen.findByLabelText("Disk name");
    type("Disk name", "scratch");
    await screen.findByText("The project's disks are still loading.");
    expect(submitButton().textContent).toBe("Loading names…");
    expect(submitButton().disabled).toBe(true);
    list.resolve({ items: [disk("scratch")] });
    await screen.findByText("A disk named scratch exists in this project.");
    expect(submitButton().disabled).toBe(true);
    fireEvent.submit(submitButton().closest("form")!);
    expect(a.calls).not.toContain("ApplyDisk");
  });

  it("sends nothing when the list failed, as which names are taken is not known", async () => {
    const a = storageApi({ disks: () => Promise.reject(new ConnectError("the apiserver is unavailable", Code.Unavailable)) });
    render(<CreateDisk />, { wrapper: withApi(a) });
    await screen.findByLabelText("Disk name");
    type("Disk name", "data");
    await screen.findByText("The list of the project's disks did not load, so which names are taken is not known.");
    expect(submitButton().disabled).toBe(true);
  });

  it("says a name taken since the list loaded exists, as the create-only apply's refusal", async () => {
    const applied: ApplyDiskRequest[] = [];
    const a = storageApi({ get: (name) => disk(name), apply: (req) => (applied.push(req), { disk: req.disk, created: true }) });
    render(<CreateDisk />, { wrapper: withApi(a) });
    await screen.findByLabelText("Disk name");
    type("Disk name", "data");
    await waitFor(() => expect(submitButton().disabled).toBe(false));
    fireEvent.click(submitButton());
    await screen.findByText("Creating the disk failed");
    expect(screen.getByText("A disk named data exists in this project.")).toBeTruthy();
    expect(a.requests.filter((r) => r.method === "ApplyDisk").map((r) => (r.request as ApplyDiskRequest).createOnly)).toEqual([true]);
    expect(applied).toHaveLength(0);
    expect(navigate).not.toHaveBeenCalled();
  });

  it("shows the API's refusal of the apply above the button", async () => {
    const a = storageApi({
      apply: () => {
        throw new ConnectError("quota exceeded: the disk limit is 100Gi", Code.ResourceExhausted);
      },
    });
    render(<CreateDisk />, { wrapper: withApi(a) });
    await screen.findByLabelText("Disk name");
    type("Disk name", "data");
    await waitFor(() => expect(submitButton().disabled).toBe(false));
    fireEvent.click(submitButton());
    const shown = await screen.findByText("quota exceeded: the disk limit is 100Gi");
    expect(shown.compareDocumentPosition(submitButton()) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(navigate).not.toHaveBeenCalled();
  });

  it("asks password managers not to fill the name, and Cancel goes back to the list", async () => {
    render(<CreateDisk />, { wrapper: withApi(storageApi({})) });
    const name = await screen.findByLabelText("Disk name");
    expect(name.getAttribute("autocomplete")).toBe("off");
    expect(name.hasAttribute("data-1p-ignore")).toBe(true);
    expect(screen.getByRole("link", { name: "Cancel" }).getAttribute("href")).toBe("/projects/demo/disks");
  });
});

describe("New disk, by permission", () => {
  it("asks for storage.disks.apply at the project", async () => {
    const a = storageApi({});
    render(<CreateDisk />, { wrapper: withApi(a) });
    await screen.findByLabelText("Disk name");
    const asked = a.requests.find((r) => r.method === "TestPermissions")!.request as TestPermissionsRequest;
    expect(asked.scope?.target).toEqual(expect.objectContaining({ case: "project", value: "demo" }));
    expect(asked.permissions).toEqual(["storage.disks.apply"]);
  });

  it("states that the caller's roles do not include creating disks, and shows no form", async () => {
    render(<CreateDisk />, { wrapper: withApi(storageApi({ perms: async () => ["storage.disks.delete"] })) });
    await screen.findByText("This account's roles do not include creating disks in this project.");
    expect(screen.queryByLabelText("Disk name")).toBeNull();
  });

  it("shows nothing while the answer is unknown", async () => {
    const a = storageApi({ perms: never });
    render(<CreateDisk />, { wrapper: withApi(a) });
    await waitFor(() => expect(a.calls).toContain("TestPermissions"));
    expect(screen.queryByLabelText("Disk name")).toBeNull();
    expect(screen.queryByText(/roles do not include/)).toBeNull();
  });

  it("shows the failed answer, and no form, when TestPermissions fails", async () => {
    render(<CreateDisk />, { wrapper: withApi(storageApi({ perms: () => Promise.reject(new ConnectError("the apiserver is unavailable", Code.Unavailable)) })) });
    await screen.findByText(/the apiserver is unavailable/);
    expect(screen.queryByLabelText("Disk name")).toBeNull();
    expect(screen.queryByText(/roles do not include/)).toBeNull();
  });
});

describe("diskYaml", () => {
  it("keeps the intended-use label and drops the platform's own", () => {
    const d = newDisk("demo", "data", "boot", "20", "debian-13");
    d.metadata!.labels["storage.hangarlab.cloud/node"] = "node-a";
    const y = diskYaml(d);
    expect(y).toContain("hangar/intended-use: boot");
    expect(y).not.toContain("hangarlab.cloud/node");
  });
});
