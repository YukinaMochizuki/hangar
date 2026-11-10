// @vitest-environment jsdom

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { FileShareEntrySchema, FileShareEntryType, type ListFilesRequest } from "@hangar/api/hangar/storage/v1alpha1/file_share_files_pb";
import { FileSharePhase } from "@hangar/api/hangar/storage/v1alpha1/file_share_pb";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { decodePath, displayName, encodePath, formatSize } from "@/lib/share-files";
import { FileShareDetail } from "@/pages/file-share-detail";
import { FileShareFiles } from "@/pages/file-share-files";
import { polyfillDialog, withApi } from "@/test/harness";
import { project, settledPermissions, share, sharesApi, type Fake } from "@/test/file-shares";

const navigate = vi.fn();
vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  useNavigate: () => navigate,
  Link: (await import("@/test/fake-link")).FakeLink,
}));

beforeAll(polyfillDialog);
beforeEach(() => navigate.mockReset());
afterEach(cleanup);

const enc = new TextEncoder();
const at = timestampFromDate(new Date("2026-11-09T03:00:00Z"));
const entry = (name: string | Uint8Array, type: FileShareEntryType, size = 0) =>
  create(FileShareEntrySchema, { name: typeof name === "string" ? enc.encode(name) : name, type, sizeBytes: BigInt(size), modifyTime: at });

describe("names and paths", () => {
  it("shows UTF-8 as it is and every other byte, and control characters, as \\xNN", () => {
    expect(displayName(enc.encode("照片.jpg"))).toEqual({ text: "照片.jpg", utf8: true });
    expect(displayName(Uint8Array.from([0x61, 0xe9, 0x62]))).toEqual({ text: "a\\xE9b", utf8: false });
    expect(displayName(Uint8Array.from([0x61, 0x0a, 0x62]))).toEqual({ text: "a\\x0Ab", utf8: true });
    // An overlong encoding and a surrogate are not UTF-8.
    expect(displayName(Uint8Array.from([0xc0, 0xaf])).utf8).toBe(false);
    expect(displayName(Uint8Array.from([0xed, 0xa0, 0x80])).utf8).toBe(false);
    // A C1 control and a right-to-left override are valid UTF-8, shown escaped.
    expect(displayName(Uint8Array.from([0x61, 0xc2, 0x85]))).toEqual({ text: "a\\xC2\\x85", utf8: true });
    expect(displayName(enc.encode("invoice\u202Efdp.exe")).text).toBe("invoice\\xE2\\x80\\xAEfdp.exe");
  });

  it("carries any path through the URL and back", () => {
    const path = [enc.encode("a b"), Uint8Array.from([0xff, 0x25]), enc.encode("照片"), enc.encode("2026")];
    const s = encodePath(path);
    expect(s).toMatch(/^[A-Za-z0-9\-._~%/]+$/);
    expect(bytes(decodePath(s))).toEqual(bytes(path));
    expect(decodePath("")).toEqual([]);
    for (const bad of ["a//b", "%G1", "%1G", "% 1", "%4", "é"]) expect(decodePath(bad)).toBeUndefined();
  });

  it("writes sizes in binary units", () => {
    expect(formatSize(0n)).toBe("0 B");
    expect(formatSize(1536n)).toBe("1.5 KiB");
    expect(formatSize(64n << 20n)).toBe("64 MiB");
  });
});

/** Renders the files page of share "media" at path, encoded. */
function showFiles(f: Fake, path?: string) {
  const api = sharesApi({ shares: [share("media", { phase: FileSharePhase.AVAILABLE })], ...f });
  render(<FileShareFiles project={project} name="media" path={path} />, { wrapper: withApi(api) });
  return api;
}

/** Bytes as numbers, which compare whichever realm made them. */
const bytes = (p: Uint8Array[] | undefined) => p?.map((n) => Array.from(n));

const asked = (api: ReturnType<typeof sharesApi>) => api.requests.filter((c) => c.method === "ListFiles").map((c) => c.request as ListFilesRequest);

describe("FileShareFiles", () => {
  it("lists a directory, its names as bytes, and pages on", async () => {
    const api = showFiles(
      {
        files: (req) =>
          req.pageToken === ""
            ? {
                entries: [entry("2026", FileShareEntryType.DIRECTORY), entry(Uint8Array.from([0x6f, 0x6c, 0x64, 0xe9]), FileShareEntryType.REGULAR, 2048), entry("current", FileShareEntryType.SYMLINK)],
                nextPageToken: "tok",
              }
            : { entries: [entry("zz.txt", FileShareEntryType.REGULAR, 1)] },
      },
      "backups",
    );
    expect(await screen.findByText("old\\xE9")).toBeTruthy();
    expect(screen.getByText("not UTF-8")).toBeTruthy();
    expect(screen.getByText("2 KiB")).toBeTruthy();
    expect(screen.getByText("not followed")).toBeTruthy();
    expect(asked(api)[0]).toMatchObject({ project, name: "media", pageSize: 500, pageToken: "" });
    expect(bytes(asked(api)[0].path)).toEqual([Array.from(enc.encode("backups"))]);

    fireEvent.click(screen.getByRole("button", { name: "Load more" }));
    expect(await screen.findByText("zz.txt")).toBeTruthy();
    expect(screen.getByText("old\\xE9")).toBeTruthy();
    expect(asked(api)[1].pageToken).toBe("tok");
    expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
  });

  it("opens a directory, and goes back up the path", async () => {
    showFiles({ files: () => ({ entries: [entry("sub dir", FileShareEntryType.DIRECTORY), entry("f", FileShareEntryType.REGULAR)] }) }, "a/b");
    fireEvent.click(await screen.findByRole("button", { name: /sub dir/ }));
    expect(navigate).toHaveBeenLastCalledWith(expect.objectContaining({ search: { path: "a/b/sub%20dir" } }));
    // A file is no link.
    expect(screen.queryByRole("button", { name: /^f$/ })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "a" }));
    expect(navigate).toHaveBeenLastCalledWith(expect.objectContaining({ search: { path: "a" } }));
    fireEvent.click(screen.getByRole("button", { name: /media/ }));
    expect(navigate).toHaveBeenLastCalledWith(expect.objectContaining({ search: {} }));
  });

  it("says when a directory is empty", async () => {
    showFiles({});
    expect(await screen.findByText("The share is empty.")).toBeTruthy();
  });

  for (const [code, title] of [
    [Code.Unavailable, "The share's server is not running"],
    [Code.Unimplemented, "This share's server cannot list files yet"],
    [Code.NotFound, "No such directory"],
    [Code.FailedPrecondition, "This directory cannot be listed"],
    [Code.ResourceExhausted, "This directory is too large to list here"],
    [Code.Internal, "The directory did not load"],
  ] as const) {
    it(`tells a listing refused with ${Code[code]}`, async () => {
      showFiles({
        files: () => {
          throw new ConnectError("refused", code);
        },
      });
      expect(await screen.findByText(title)).toBeTruthy();
    });
  }

  it("tells a share that is gone, or going, from a directory that is", async () => {
    let api = sharesApi({ shares: [] });
    render(<FileShareFiles project={project} name="media" />, { wrapper: withApi(api) });
    expect(await screen.findByText("No such file share")).toBeTruthy();
    expect(asked(api)).toEqual([]);
    cleanup();

    const going = share("media", { phase: FileSharePhase.AVAILABLE });
    going.metadata!.deleteTime = at;
    api = sharesApi({ shares: [going] });
    render(<FileShareFiles project={project} name="media" />, { wrapper: withApi(api) });
    expect(await screen.findByText("This share is being deleted")).toBeTruthy();
    expect(asked(api)).toEqual([]);
  });

  it("asks nothing for a path no link writes", async () => {
    const api = showFiles({}, "a%ZZ");
    expect(await screen.findByText("No such directory")).toBeTruthy();
    expect(asked(api)).toEqual([]);
  });
});

describe("the share's page", () => {
  it("offers Browse files to those who may browse, and to no one else", async () => {
    const media = share("media", { phase: FileSharePhase.AVAILABLE });
    let api = sharesApi({ shares: [media] });
    render(<FileShareDetail name="media" />, { wrapper: withApi(api) });
    await settledPermissions(api, waitFor);
    const link = await screen.findByRole("link", { name: /Browse files/ });
    expect(link.getAttribute("href")).toBe(`/projects/${project}/file-shares/media/files`);
    cleanup();

    api = sharesApi({ shares: [media], perms: () => ["storage.fileshares.get", "storage.fileshares.apply"] });
    render(<FileShareDetail name="media" />, { wrapper: withApi(api) });
    await settledPermissions(api, waitFor);
    await screen.findByRole("button", { name: /Grow/ });
    expect(screen.queryByRole("link", { name: /Browse files/ })).toBeNull();
  });
});
