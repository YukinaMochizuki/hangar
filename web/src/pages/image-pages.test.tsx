// @vitest-environment jsdom

import { Code, ConnectError } from "@connectrpc/connect";
import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import type { ApplyDiskRequest } from "@hangar/api/hangar/storage/v1alpha1/disk_pb";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { ImageDetail } from "@/pages/image-detail";
import { ImageList } from "@/pages/image-list";
import { never, polyfillDialog, withApi } from "@/test/harness";
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

const verified = image("debian-13", { size: "3Gi", min: "10Gi", verified: [ConditionStatus.TRUE, "Verified", ""] });
const fetching = image("fedora-43");
const faulty = image("alpine-3", { verified: [ConditionStatus.FALSE, "HashMismatch", "The file does not match its sha256."] });
const large = image("ubuntu-2404", { size: "12Gi", min: "10Gi", verified: [ConditionStatus.TRUE, "Verified", ""] });
const all = () => storageApi({ images: async () => ({ items: [verified, fetching, faulty, large] }) });

describe("ImageList", () => {
  it("shows skeletons while the images load", async () => {
    render(<ImageList />, { wrapper: withApi(storageApi({ images: never })) });
    expect((await screen.findAllByLabelText("Loading")).length).toBeGreaterThan(0);
  });

  it("says the images did not load, with the API's error", async () => {
    render(<ImageList />, { wrapper: withApi(storageApi({ images: () => Promise.reject(new ConnectError("the apiserver is unavailable", Code.Unavailable)) })) });
    await screen.findByText("The images did not load");
    expect(screen.getByText("the apiserver is unavailable")).toBeTruthy();
  });

  it("says the catalogue is empty", async () => {
    render(<ImageList />, { wrapper: withApi(storageApi({})) });
    await screen.findByText("No images");
  });

  it("shows each image's state and smallest disk, and offers no action", async () => {
    render(<ImageList />, { wrapper: withApi(all()) });
    const t = await screen.findByRole("table");
    const row = (name: string) => within(t).getAllByRole("row").find((r) => r.textContent?.includes(name))!;
    await within(t).findByText("debian-13");
    expect(within(row("debian-13")).getByText("Verified")).toBeTruthy();
    expect(within(row("debian-13")).getByText("10 GiB")).toBeTruthy();
    // The smallest disk is the larger of min_disk_size and the image's own size.
    expect(within(row("ubuntu-2404")).getByText("12 GiB")).toBeTruthy();
    expect(within(row("fedora-43")).getByText("Not fetched yet")).toBeTruthy();
    expect(within(row("fedora-43")).getByText("not known yet")).toBeTruthy();
    expect(within(row("alpine-3")).getByText("HashMismatch")).toBeTruthy();
    expect(within(row("debian-13")).getByRole("link", { name: "debian-13" }).getAttribute("href")).toBe("/projects/demo/images/debian-13");
    expect(screen.queryByRole("button", { name: /New|Delete|Create/ })).toBeNull();
  });
});

describe("ImageDetail", () => {
  it("shows skeletons while the images load", async () => {
    render(<ImageDetail name="debian-13" />, { wrapper: withApi(storageApi({ images: never })) });
    expect((await screen.findAllByLabelText("Loading")).length).toBeGreaterThan(0);
  });

  it("says the image did not load, with the API's error", async () => {
    render(<ImageDetail name="debian-13" />, {
      wrapper: withApi(storageApi({ images: () => Promise.reject(new ConnectError("the apiserver is unavailable", Code.Unavailable)) })),
    });
    await screen.findByText("The image did not load");
  });

  it("says the platform has no image of the name", async () => {
    render(<ImageDetail name="gone" />, { wrapper: withApi(all()) });
    await screen.findByText("No such image");
  });

  it("shows the image and this project's disks made from it", async () => {
    const a = storageApi({
      images: async () => ({ items: [verified] }),
      disks: async () => ({ items: [disk("debian-test", { image: "debian-13" }), disk("scratch")] }),
    });
    render(<ImageDetail name="debian-13" />, { wrapper: withApi(a) });
    await screen.findByRole("link", { name: "debian-test" });
    expect(screen.queryByRole("link", { name: "scratch" })).toBeNull();
    expect(screen.getByText("https://images.example.com/debian-13.img")).toBeTruthy();
    expect(screen.getByText("ab".repeat(32))).toBeTruthy();
  });

  it("says an image is at fault, and offers no disk of it", async () => {
    render(<ImageDetail name="alpine-3" />, { wrapper: withApi(all()) });
    await screen.findByText("alpine-3 is at fault: HashMismatch");
    expect(((await screen.findByRole("button", { name: "New disk from it" })) as HTMLButtonElement).disabled).toBe(true);
  });

  it("creates a disk of the image with one create-only apply, the image as its source", async () => {
    const applied: ApplyDiskRequest[] = [];
    const a = storageApi({
      images: async () => ({ items: [verified] }),
      apply: (req) => (applied.push(req), { disk: req.disk, created: true }),
    });
    render(<ImageDetail name="debian-13" />, { wrapper: withApi(a) });
    fireEvent.click(await screen.findByRole("button", { name: "New disk from it" }));
    const d = screen.getByRole("dialog", { name: "New disk" });
    expect((within(d).getByLabelText("Holds") as HTMLSelectElement).value).toBe("debian-13");
    fireEvent.change(within(d).getByLabelText("Name"), { target: { value: "data" } });
    const submit = within(d).getByRole("button", { name: /Create disk|Loading names/ }) as HTMLButtonElement;
    await waitFor(() => expect(submit.disabled).toBe(false));
    fireEvent.click(submit);
    await waitFor(() => expect(applied).toHaveLength(1));
    expect(a.calls.filter((c) => c === "GetDisk" || c === "ApplyDisk")).toEqual(["ApplyDisk"]);
    expect(applied[0].createOnly).toBe(true);
    expect(applied[0].disk?.spec?.source?.source).toEqual(expect.objectContaining({ case: "image", value: expect.objectContaining({ name: "debian-13" }) }));
    expect(applied[0].disk?.spec?.size).toBe("20Gi");
  });

  it("hides New disk from it from a caller who does not hold storage.disks.apply", async () => {
    const a = storageApi({ images: async () => ({ items: [verified] }), perms: async () => [] });
    render(<ImageDetail name="debian-13" />, { wrapper: withApi(a) });
    await screen.findByRole("heading", { name: "debian-13" });
    await waitFor(() => expect(a.calls).toContain("TestPermissions"));
    expect(screen.queryByRole("button", { name: "New disk from it" })).toBeNull();
  });

  it("hides New disk from it and says nothing while the answer is unknown", async () => {
    render(<ImageDetail name="debian-13" />, { wrapper: withApi(storageApi({ images: async () => ({ items: [verified] }), perms: never })) });
    await screen.findByRole("heading", { name: "debian-13" });
    expect(screen.queryByRole("button", { name: "New disk from it" })).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
  });
});
