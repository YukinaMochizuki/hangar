// @vitest-environment jsdom

import type { ApplyCustomImageRequest } from "@hangar/api/hangar/storage/v1alpha1/custom_image_pb";
import { DiskAttachmentRole, DiskPhase, type ApplyDiskRequest } from "@hangar/api/hangar/storage/v1alpha1/disk_pb";
import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { customImageState } from "@/lib/custom-images";
import { CreateCustomImage } from "@/pages/create-custom-image";
import { CreateDisk } from "@/pages/create-disk";
import { CustomImageDetail } from "@/pages/custom-image-detail";
import { ImageList } from "@/pages/image-list";
import { withApi } from "@/test/harness";
import { customImage, disk, image, storageApi } from "@/test/storage";

const navigate = vi.fn();
vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  useNavigate: () => navigate,
  Link: (await import("@/test/fake-link")).FakeLink,
}));
vi.mock("@/components/shell", async (orig) => ({
  ...(await orig<typeof import("@/components/shell")>()),
  useProjectName: () => "demo",
  Topbar: () => null,
}));

beforeEach(() => navigate.mockReset());
afterEach(cleanup);

const ubuntu = image("ubuntu-24-04", { size: "4Gi", verified: [ConditionStatus.TRUE, "Verified", ""] });
const boot = (name: string, vm: string) =>
  disk(`${name}`, {
    image: "ubuntu-24-04",
    machine: `vm-${vm}`,
    rootOf: vm,
    attachment: { kind: "VirtualMachine", name: vm, as: DiskAttachmentRole.BOOT, device: "vda" },
  });
// web is stopped, api runs; scratch was never placed; data is on no machine.
const disks = async () => ({
  items: [boot("web-root", "web"), boot("api-root", "api"), disk("scratch", { phase: DiskPhase.PENDING }), disk("data", { size: "50Gi" })],
});
const vms = { web: true, api: false };

const radio = (name: string) => screen.getByRole("radio", { name }) as HTMLButtonElement;
const capture = () => screen.getByRole("button", { name: /Capture image|Loading names|Creating/ }) as HTMLButtonElement;

describe("customImageState", () => {
  it("reads Ready, a failed capture with the node's message, and anything else as capturing", () => {
    expect(customImageState(customImage("a")).label).toBe("Ready");
    expect(customImageState(customImage("a", { state: "failed" }))).toEqual(
      expect.objectContaining({ label: "Failed", tone: "fail", message: "zfs receive: out of space", capturing: false }),
    );
    expect(customImageState(customImage("a", { state: "capturing" }))).toEqual(expect.objectContaining({ label: "Capturing", capturing: true }));
  });
});

describe("New image", () => {
  it("lists the disks that can be captured, folds away the rest with why, and sends one create-only apply of the disk chosen", async () => {
    const applied: ApplyCustomImageRequest[] = [];
    const a = storageApi({ disks, images: async () => ({ items: [ubuntu] }), vms, applyCustomImage: (req) => (applied.push(req), { customImage: req.customImage, created: true }) });
    render(<CreateCustomImage />, { wrapper: withApi(a) });

    const list = await screen.findByRole("radiogroup", { name: "Source disk" });
    await waitFor(() => expect(within(list).getAllByRole("radio").map((r) => r.getAttribute("aria-label"))).toEqual(["web-root", "data"]));
    expect(screen.getByText("2 of 4 can be captured now")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: /Show 2 that cannot be captured now/ }));
    expect(within(list).getAllByRole("radio").map((r) => r.getAttribute("aria-label"))).toEqual(["web-root", "data", "api-root", "scratch"]);
    expect(radio("api-root").disabled).toBe(true);
    expect(radio("api-root").title).toBe("api has it and is not stopped. Stop api first.");
    expect(radio("scratch").title).toMatch(/not been made on a node/);

    fireEvent.change(screen.getByLabelText("Filter disks"), { target: { value: "web" } });
    expect(within(list).getAllByRole("radio").map((r) => r.getAttribute("aria-label"))).toEqual(["web-root"]);

    fireEvent.click(radio("web-root"));
    // The image's description follows the disk's image until edited.
    expect((screen.getByLabelText("OS") as HTMLInputElement).value).toBe("debian");
    expect(screen.getByText(/hangar stop vm web -p demo --wait/)).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Image name"), { target: { value: "golden" } });
    await waitFor(() => expect(capture().disabled).toBe(false));
    fireEvent.click(capture());

    await waitFor(() => expect(navigate).toHaveBeenCalledWith({ to: "/projects/$project/custom-images/$name", params: { project: "demo", name: "golden" }, replace: true }));
    expect(applied).toHaveLength(1);
    expect(applied[0].createOnly).toBe(true);
    const sent = applied[0].customImage!;
    expect(sent.metadata?.project).toBe("demo");
    expect(sent.spec?.source?.source).toEqual({ case: "disk", value: expect.objectContaining({ name: "web-root" }) });
    expect(sent.spec?.defaultUser).toBe("debian");
  });

  it("keeps a disk its address names in sight and refuses it, sending nothing, while its machine runs", async () => {
    const applied: ApplyCustomImageRequest[] = [];
    const a = storageApi({ disks, vms, applyCustomImage: (req) => (applied.push(req), { customImage: req.customImage, created: true }) });
    render(<CreateCustomImage disk="api-root" />, { wrapper: withApi(a) });
    await waitFor(() => expect(radio("api-root").getAttribute("aria-checked")).toBe("true"));
    fireEvent.change(screen.getByLabelText("Image name"), { target: { value: "golden" } });
    await screen.findByText("api-root: api has it and is not stopped. Stop api first.");
    expect(capture().disabled).toBe(true);
    expect(applied).toHaveLength(0);
  });
});

describe("Images", () => {
  it("shows the project's custom images above the platform's, each state as its node reports it", async () => {
    const a = storageApi({
      images: async () => ({ items: [ubuntu] }),
      customImages: async () => ({ items: [customImage("golden", { used: "3Gi" }), customImage("next", { state: "capturing" }), customImage("bad", { state: "failed" })] }),
    });
    render(<ImageList />, { wrapper: withApi(a) });
    const table = (await screen.findAllByRole("table"))[0];
    await within(table).findByText("golden");
    const rows = within(table).getAllByRole("row").slice(1).map((r) => r.textContent);
    expect(rows[0]).toMatch(/bad.*Failed.*Capture failed/);
    expect(rows[1]).toMatch(/golden.*Ready.*web-root.*20 GiB.*3 GiB/);
    expect(rows[2]).toMatch(/next.*Capturing/);
    expect(screen.getAllByRole("link", { name: /New image/ }).length).toBeGreaterThan(0);
  });

  it("says what a custom image is for when the project has none, and offers New image only to whoever may capture", async () => {
    render(<ImageList />, { wrapper: withApi(storageApi({ perms: async () => ["storage.disks.apply"] })) });
    await screen.findByText("No custom images");
    expect(screen.queryByRole("link", { name: /New image/ })).toBeNull();
  });

  it("offers a disk from a Ready custom image alone, on its page", async () => {
    const a = storageApi({ customImages: async () => ({ items: [customImage("next", { state: "capturing" })] }) });
    render(<CustomImageDetail name="next" />, { wrapper: withApi(a) });
    await screen.findByText("Capturing next");
    await waitFor(() => expect((screen.getByRole("button", { name: /New disk from it/ }) as HTMLButtonElement).disabled).toBe(true));
  });
});

describe("New disk from a custom image", () => {
  it("offers the project's Ready images apart from the platform's, and names the project in the source it sends", async () => {
    const applied: ApplyDiskRequest[] = [];
    const a = storageApi({
      images: async () => ({ items: [ubuntu] }),
      customImages: async () => ({ items: [customImage("golden"), customImage("next", { state: "capturing" })] }),
      apply: (req) => (applied.push(req), { disk: req.disk, created: true }),
    });
    render(<CreateDisk image={{ name: "golden", custom: true }} />, { wrapper: withApi(a) });
    const mine = await screen.findByRole("radiogroup", { name: "This project's images" });
    expect(within(mine).getAllByRole("radio").map((r) => r.getAttribute("aria-label"))).toEqual(["golden"]);
    expect(within(mine).getByRole("radio", { name: "golden" }).getAttribute("aria-checked")).toBe("true");
    expect(screen.getByRole("radiogroup", { name: "Platform images" })).toBeTruthy();
    // The size starts at the image's smallest disk, the source disk's 20 GiB.
    expect((screen.getByLabelText("Size, GiB") as HTMLInputElement).value).toBe("20");
    fireEvent.change(screen.getByLabelText("Disk name"), { target: { value: "copy" } });
    const submit = () => screen.getByRole("button", { name: /Create disk/ }) as HTMLButtonElement;
    await waitFor(() => expect(submit().disabled).toBe(false));
    fireEvent.click(submit());
    await waitFor(() => expect(applied).toHaveLength(1));
    expect(applied[0].disk?.spec?.source?.source).toEqual({ case: "image", value: expect.objectContaining({ name: "golden", project: "demo" }) });
  });
});
