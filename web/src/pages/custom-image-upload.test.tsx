// @vitest-environment jsdom

import type { ApplyCustomImageRequest } from "@hangar/api/hangar/storage/v1alpha1/custom_image_pb";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { customImageState } from "@/lib/custom-images";
import { forgetUpload } from "@/lib/upload";
import { CreateCustomImage, nameOfFile } from "@/pages/create-custom-image";
import { CustomImageDetail, uploadStops } from "@/pages/custom-image-detail";
import { ImageList } from "@/pages/image-list";
import { withApi } from "@/test/harness";
import { storageApi, uploadedImage } from "@/test/storage";

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

// The taxi line asks whether motion is reduced, which jsdom cannot answer.
beforeAll(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {} })) as unknown as typeof window.matchMedia;
});

/** The PUTs to the upload route, by their Content-Range; each answered as the node takes it. */
let puts: string[] = [];
beforeEach(() => {
  navigate.mockReset();
  puts = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (_url: string, init: RequestInit) => {
      const range = (init.headers as Record<string, string>)["Content-Range"];
      puts.push(range);
      const end = Number(/-(\d+)\//.exec(range)![1]) + 1;
      return new Response(JSON.stringify({ received_bytes: end }), { status: 200 });
    }),
  );
});
afterEach(() => {
  cleanup();
  for (const n of ["nixos", "nixos-24-11"]) forgetUpload("demo", n);
  vi.unstubAllGlobals();
});

const qcow2 = (n: number, name = "nixos-24.11.qcow2") => {
  const b = new Uint8Array(n);
  b.set([0x51, 0x46, 0x49, 0xfb]);
  return new File([b], name);
};

describe("an uploaded image's state", () => {
  it("is awaiting its file, then converting, or failed with the step it failed at", () => {
    expect(customImageState(uploadedImage("a", 100, { awaiting: 37 }))).toEqual(expect.objectContaining({ step: "awaiting-upload", label: "Awaiting upload" }));
    expect(customImageState(uploadedImage("a", 100, "converting"))).toEqual(expect.objectContaining({ step: "converting", label: "Converting" }));
    expect(customImageState(uploadedImage("a", 100, { failed: "InvalidImage", uploaded: true }))).toEqual(
      expect.objectContaining({ step: "failed", reason: "Not a usable image", message: "the converter refused it" }),
    );
    const at = (ci: ReturnType<typeof uploadedImage>) => uploadStops(ci, customImageState(ci).step, customImageState(ci).reason).map((s) => `${s.label}:${s.state}`);
    expect(at(uploadedImage("a", 100, { awaiting: 37 }))).toEqual(["Upload:current", "Convert:todo", "Ready:todo"]);
    expect(at(uploadedImage("a", 100, "converting"))).toEqual(["Upload:done", "Convert:current", "Ready:todo"]);
    expect(at(uploadedImage("a", 100, { failed: "ConvertFailed", uploaded: true }))).toEqual(["Upload:done", "Convert:failed", "Ready:todo"]);
    expect(at(uploadedImage("a", 100, { failed: "UploadExpired", uploaded: false }))).toEqual(["Upload:failed", "Convert:todo", "Ready:todo"]);
    expect(uploadStops(uploadedImage("a", 100, { awaiting: 37 }), "awaiting-upload")[0].note).toBe("37% of 100 B");
  });

  it("names an image after its file", () => {
    expect(nameOfFile("nixos-24.11-x86_64.qcow2")).toBe("nixos-24-11-x86-64");
    expect(nameOfFile("Arch Cloud.img")).toBe("arch-cloud");
  });
});

describe("New image, uploading a file", () => {
  it("makes an upload image of the file's size with one create-only apply, starts sending it, and opens its page", async () => {
    const applied: ApplyCustomImageRequest[] = [];
    const a = storageApi({ applyCustomImage: (req) => (applied.push(req), { customImage: req.customImage, created: true }) });
    render(<CreateCustomImage mode="upload" />, { wrapper: withApi(a) });
    expect((await screen.findByRole("radio", { name: "Upload a file" })).getAttribute("aria-checked")).toBe("true");
    fireEvent.change(screen.getByLabelText("Image file"), { target: { files: [qcow2(1024)] } });
    await screen.findByText("qcow2");
    // The name follows the file's until typed.
    expect((screen.getByLabelText("Image name") as HTMLInputElement).value).toBe("nixos-24-11");
    const submit = () => screen.getByRole("button", { name: /Create and upload/ }) as HTMLButtonElement;
    await waitFor(() => expect(submit().disabled).toBe(false));
    fireEvent.click(submit());
    await waitFor(() => expect(navigate).toHaveBeenCalledWith({ to: "/projects/$project/custom-images/$name", params: { project: "demo", name: "nixos-24-11" }, replace: true }));
    expect(applied).toHaveLength(1);
    expect(applied[0].createOnly).toBe(true);
    expect(applied[0].customImage?.spec?.source?.source).toEqual({ case: "upload", value: expect.objectContaining({ size: 1024n }) });
    await waitFor(() => expect(puts).toEqual(["bytes 0-1023/1024"]));
  });

  it("refuses a file that is neither qcow2 nor raw, and sends nothing", async () => {
    const applied: ApplyCustomImageRequest[] = [];
    render(<CreateCustomImage mode="upload" />, { wrapper: withApi(storageApi({ applyCustomImage: (req) => (applied.push(req), { created: true }) })) });
    fireEvent.change(await screen.findByLabelText("Image file"), { target: { files: [new File([new Uint8Array(1000)], "notes.txt")] } });
    await screen.findByText(/neither qcow2 nor a raw disk/);
    fireEvent.change(screen.getByLabelText("Image name"), { target: { value: "notes" } });
    expect((screen.getByRole("button", { name: /Create and upload/ }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText("notes.txt is not a qcow2 or raw image.")).toBeTruthy();
    expect(applied).toHaveLength(0);
  });
});

describe("An uploaded image's page", () => {
  it("with no upload in this tab, takes the same file again and goes on from the node's count, refusing one of another size", async () => {
    const a = storageApi({ customImages: async () => ({ items: [uploadedImage("nixos", 4096, { awaiting: 1024 })] }) });
    render(<CustomImageDetail name="nixos" />, { wrapper: withApi(a) });
    await screen.findByText("No upload runs in this tab");
    expect(screen.getByText("25%")).toBeTruthy();
    const input = screen.getByLabelText("Image file to resume");
    fireEvent.change(input, { target: { files: [qcow2(4000)] } });
    await screen.findByText(/this image was made for a file of 4 KiB/);
    expect(puts).toEqual([]);
    fireEvent.change(input, { target: { files: [qcow2(4096)] } });
    await waitFor(() => expect(puts).toEqual(["bytes 1024-4095/4096"]));
  });

  it("shows the taxi line at Convert while converting, and offers no disk until Ready", async () => {
    const a = storageApi({ customImages: async () => ({ items: [uploadedImage("nixos", 4096, "converting")] }) });
    render(<CustomImageDetail name="nixos" />, { wrapper: withApi(a) });
    await screen.findByText("Conversion");
    expect(screen.queryByText("No upload runs in this tab")).toBeNull();
    await waitFor(() => expect((screen.getByRole("button", { name: /New disk from it/ }) as HTMLButtonElement).disabled).toBe(true));
  });
});

describe("Images", () => {
  it("says how much of an upload's file the node holds, and offers Upload image", async () => {
    const a = storageApi({ customImages: async () => ({ items: [uploadedImage("nixos", 400, { awaiting: 150 })] }) });
    render(<ImageList />, { wrapper: withApi(a) });
    const table = (await screen.findAllByRole("table"))[0];
    const row = (await within(table).findByText("nixos")).closest("tr")!;
    expect(row.textContent).toMatch(/Awaiting upload · 37%.*Uploaded file/);
    expect(screen.getAllByRole("link", { name: /Upload image/ })[0].getAttribute("href")).toBe("/projects/demo/new/custom-image?upload=1");
  });
});
