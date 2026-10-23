import { afterEach, describe, expect, it, vi } from "vitest";
import { forgetUpload, pauseUpload, sha256Hex, sniffFormat, startUpload, subscribeUploads, uploadState, type UploadState } from "@/lib/upload";

/** A PUT the fake server took: the range it was asked to write. */
type Put = { range: string };

/**
 * server answers the upload route as the apiserver does: a chunk at the
 * node's count whose sha256 matches moves the count on, one elsewhere is
 * 409 with the count, and `fail` can refuse a call by its number, from 1.
 */
function server(fail: (n: number) => { status: number; body?: object } | undefined = () => undefined) {
  let received = 0;
  let calls = 0;
  const puts: Put[] = [];
  const fetch = vi.fn(async (url: string, init: RequestInit) => {
    calls++;
    const headers = init.headers as Record<string, string>;
    const body = init.body as ArrayBuffer;
    puts.push({ range: headers["Content-Range"] });
    expect(url).toBe("/v1/projects/demo/customimages/nixos:upload");
    expect(init.method).toBe("PUT");
    expect(init.credentials).toBe("same-origin");
    const refused = fail(calls);
    if (refused) return new Response(JSON.stringify(refused.body ?? {}), { status: refused.status });
    const [, a, b] = /^bytes (\d+)-(\d+)\/(\d+)$/.exec(headers["Content-Range"])!.map(Number);
    if (a !== received) return new Response(JSON.stringify({ code: "aborted", message: "wrong offset", received_bytes: received }), { status: 409 });
    if ((await sha256Hex(body)) !== headers["X-Hangar-Chunk-SHA256"]) return new Response("{}", { status: 400 });
    expect(body.byteLength).toBe(b - a + 1);
    received = b + 1;
    return new Response(JSON.stringify({ received_bytes: received }), { status: 200 });
  });
  vi.stubGlobal("fetch", fetch);
  return { puts, received: () => received, fetch };
}

const file = (n: number) => new File([new Uint8Array(n).map((_, i) => i % 251)], "nixos.qcow2");

/** phases records every state the upload of nixos goes through. */
function phases(): UploadState[] {
  const seen: UploadState[] = [];
  const stop = subscribeUploads(() => {
    const s = uploadState("demo", "nixos");
    if (s && s !== seen.at(-1)) seen.push(s);
  });
  stops.push(stop);
  return seen;
}
const stops: (() => void)[] = [];

/** settled waits until the upload of nixos is no longer running, and returns its state. */
async function settled(): Promise<UploadState> {
  await vi.waitFor(() => {
    const s = uploadState("demo", "nixos");
    expect(s && s.phase !== "uploading" && s.phase !== "retrying").toBe(true);
  });
  return uploadState("demo", "nixos")!;
}

afterEach(() => {
  forgetUpload("demo", "nixos");
  stops.splice(0).forEach((s) => s());
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe("startUpload", () => {
  it("sends the file in order, each chunk with its range and its own sha256", async () => {
    const s = server();
    startUpload("demo", "nixos", file(10), 0, 4);
    expect((await settled()).phase).toBe("done");
    expect(s.puts.map((p) => p.range)).toEqual(["bytes 0-3/10", "bytes 4-7/10", "bytes 8-9/10"]);
    expect(s.received()).toBe(10);
  });

  it("goes on from the node's count when a 409 tells it", async () => {
    const s = server();
    // Started at 4, as from a status read when the node held more; the node holds 0.
    startUpload("demo", "nixos", file(10), 4, 4);
    expect((await settled()).phase).toBe("done");
    expect(s.puts.map((p) => p.range)).toEqual(["bytes 4-7/10", "bytes 0-3/10", "bytes 4-7/10", "bytes 8-9/10"]);
    expect(s.received()).toBe(10);
  });

  it("halves its chunk when the server answers 413", async () => {
    const s = server((n) => (n === 1 ? { status: 413 } : undefined));
    startUpload("demo", "nixos", file(3 << 20), 0, 4 << 20);
    expect((await settled()).phase).toBe("done");
    expect(s.puts.map((p) => p.range)).toEqual([`bytes 0-${(3 << 20) - 1}/${3 << 20}`, `bytes 0-${(2 << 20) - 1}/${3 << 20}`, `bytes ${2 << 20}-${(3 << 20) - 1}/${3 << 20}`]);
  });

  it("retries a chunk that failed for a reason that may pass, after a wait, and says so meanwhile", async () => {
    vi.useFakeTimers({ toFake: ["setTimeout"] });
    const s = server((n) => (n === 1 ? { status: 503, body: { message: "node unavailable" } } : undefined));
    const seen = phases();
    startUpload("demo", "nixos", file(4), 0, 4);
    await vi.waitFor(() => expect(seen.at(-1)?.phase).toBe("retrying"));
    expect(seen.at(-1)).toEqual(expect.objectContaining({ error: "node unavailable", inSeconds: 1 }));
    expect(s.received()).toBe(0);
    await vi.advanceTimersByTimeAsync(1000);
    await vi.waitFor(() => expect(seen.at(-1)?.phase).toBe("done"));
    expect(s.received()).toBe(4);
  });

  it("stops for good on a refusal no retry changes, sending nothing more", async () => {
    const s = server(() => ({ status: 412, body: { message: "the image is not awaiting an upload" } }));
    startUpload("demo", "nixos", file(8), 0, 4);
    expect(await settled()).toEqual(expect.objectContaining({ phase: "failed", error: "the image is not awaiting an upload" }));
    expect(s.fetch).toHaveBeenCalledTimes(1);
  });

  it("pauses at the node's count, and sends nothing more until resumed", async () => {
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    const s = server();
    const inner = s.fetch.getMockImplementation()!;
    // The second chunk waits, so the pause comes while it is in flight.
    s.fetch.mockImplementation(async (url, init) => {
      if (s.fetch.mock.calls.length === 2) await gate;
      return inner(url, init);
    });
    startUpload("demo", "nixos", file(12), 0, 4);
    await vi.waitFor(() => expect(s.fetch).toHaveBeenCalledTimes(2));
    pauseUpload("demo", "nixos");
    release();
    await new Promise((r) => setTimeout(r, 20));
    expect(uploadState("demo", "nixos")).toEqual(expect.objectContaining({ phase: "paused", received: 4 }));
    expect(s.fetch).toHaveBeenCalledTimes(2);
  });
});

describe("sniffFormat", () => {
  it("reads qcow2 by its magic, raw by whole sectors, and refuses a vmdk", async () => {
    expect(await sniffFormat(new File([new Uint8Array([0x51, 0x46, 0x49, 0xfb, 0, 0, 0, 3])], "a"))).toBe("qcow2");
    expect(await sniffFormat(new File([new Uint8Array(1024)], "a"))).toBe("raw");
    expect(await sniffFormat(new File([new Uint8Array(1000)], "a"))).toBe("unknown");
    expect(await sniffFormat(new File([new TextEncoder().encode("KDMV"), new Uint8Array(508)], "a"))).toBe("unknown");
  });
});
