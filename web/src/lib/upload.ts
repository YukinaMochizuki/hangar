// Uploading a file into a CustomImage of the upload kind, from the browser
// (docs/decisions/custom-images.md). The file goes in order, one chunk per
// PUT to `/v1/projects/{p}/customimages/{name}:upload`, each with its
// Content-Range and its own sha256, which the node checks before the chunk
// counts. The browser never hashes the whole file: Web Crypto hashes only
// whole buffers, and a chunk is one.
//
// Where to start is the image's status.received_bytes; a PUT at the wrong
// offset is answered 409 with the node's own count, and the upload goes on
// from there. The cookie is the credential, so every request is one the
// apiserver's Origin check believes: a PUT, which always carries Origin
// (console-design.md); the route's GET, which may not, is never used.
//
// Uploads run in a store outside React, so leaving a page does not stop
// one; closing the tab does, and the image's page then offers to choose the
// file again and go on from where the node is.

import { useSyncExternalStore } from "react";

/** The chunk size the apiserver takes; a smaller one is used if it answers 413. */
export const uploadChunkBytes = 64 << 20;

/** How long a failed chunk waits before it is sent again, doubling, up to the last. */
const retryDelays = [1000, 2000, 4000, 8000, 16000, 30000];

export type UploadState =
  | { phase: "uploading"; received: number; size: number; bytesPerSecond: number }
  | { phase: "paused"; received: number; size: number }
  | { phase: "retrying"; received: number; size: number; error: string; inSeconds: number }
  | { phase: "done"; size: number }
  | { phase: "failed"; received: number; size: number; error: string };

/** The key an upload is kept under: one per image. */
export const uploadKey = (project: string, name: string) => `${project}/${name}`;

type Entry = { state: UploadState; file: File; chunk: number; abort?: AbortController };

const entries = new Map<string, Entry>();
const listeners = new Set<() => void>();

function set(key: string, e: Entry) {
  entries.set(key, e);
  for (const l of listeners) l();
}

/** subscribeUploads calls l whenever an upload in this tab changes, until the function it returns is called. */
export function subscribeUploads(l: () => void): () => void {
  listeners.add(l);
  return () => listeners.delete(l);
}

/** uploadState is the state of the upload of an image in this tab, or undefined when none runs here. */
export function uploadState(project: string, name: string): UploadState | undefined {
  return entries.get(uploadKey(project, name))?.state;
}

/** useUpload is uploadState as a component reads it, again whenever it changes. */
export function useUpload(project: string, name: string): UploadState | undefined {
  return useSyncExternalStore(subscribeUploads, () => uploadState(project, name));
}

/** anyUploading says whether an upload is running in this tab, for the page to warn before it is closed. */
export function anyUploading(): boolean {
  for (const e of entries.values()) if (e.state.phase === "uploading" || e.state.phase === "retrying") return true;
  return false;
}

// Closing or reloading the tab stops its uploads, so the browser asks first
// while one runs.
if (typeof window !== "undefined") {
  window.addEventListener("beforeunload", (e) => {
    if (anyUploading()) e.preventDefault();
  });
}

/** UploadError is a PUT the API refused; offset is the node's count, when it told one, and retryAfter the seconds it asked to wait. */
export class UploadError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly offset?: number,
    readonly retryAfter?: number,
  ) {
    super(message);
  }
}

const hex = (b: ArrayBuffer) => [...new Uint8Array(b)].map((x) => x.toString(16).padStart(2, "0")).join("");

/** sha256Hex is a buffer's sha256 as lowercase hex. */
export async function sha256Hex(b: ArrayBuffer): Promise<string> {
  return hex(await crypto.subtle.digest("SHA-256", b));
}

/** putChunk sends one chunk at offset and returns the node's count after it. */
export async function putChunk(project: string, name: string, file: Blob, offset: number, size: number, chunk: number, signal?: AbortSignal): Promise<number> {
  const end = Math.min(offset + chunk, size);
  const body = await file.slice(offset, end).arrayBuffer();
  const resp = await fetch(`/v1/projects/${encodeURIComponent(project)}/customimages/${encodeURIComponent(name)}:upload`, {
    method: "PUT",
    credentials: "same-origin",
    signal,
    headers: {
      "Content-Type": "application/octet-stream",
      "Content-Range": `bytes ${offset}-${end - 1}/${size}`,
      "X-Hangar-Chunk-SHA256": await sha256Hex(body),
    },
    body,
  });
  const json = (await resp.json().catch(() => ({}))) as { received_bytes?: number | string; message?: string; code?: string };
  const at = json.received_bytes === undefined ? undefined : Number(json.received_bytes);
  if (!resp.ok) {
    const after = Number(resp.headers.get("Retry-After"));
    throw new UploadError(json.message || `${resp.status} ${resp.statusText}`, resp.status, at, Number.isFinite(after) && after > 0 ? after : undefined);
  }
  return at ?? end;
}

// A refusal no retry can change: the image is not awaiting this upload or
// the request is wrong (400), the caller is not signed in or may not (401,
// 403), or the image is gone (404). Anything else may pass: a node busy
// with another chunk of the image (503), a chunk garbled on its way, which
// the node drops (500), a node not connected.
const final = (s: number) => s === 400 || s === 401 || s === 403 || s === 404;

/**
 * startUpload sends file to the image from offset `from`, the image's
 * received_bytes as last read, in the background of this tab, in chunks of
 * at most `chunk` bytes. A start while one runs for the image is ignored.
 */
export function startUpload(project: string, name: string, file: File, from: number, chunk = uploadChunkBytes) {
  const key = uploadKey(project, name);
  const running = entries.get(key);
  if (running && (running.state.phase === "uploading" || running.state.phase === "retrying")) return;
  const abort = new AbortController();
  const size = file.size;
  set(key, { file, abort, chunk, state: { phase: "uploading", received: from, size, bytesPerSecond: 0 } });
  void run(key, project, name, file, from, chunk, abort.signal);
}

/** pauseUpload stops the upload after nothing more: the chunk in flight is dropped, and the node keeps what it counted. */
export function pauseUpload(project: string, name: string) {
  const key = uploadKey(project, name);
  const e = entries.get(key);
  if (!e || (e.state.phase !== "uploading" && e.state.phase !== "retrying")) return;
  e.abort?.abort();
  set(key, { ...e, abort: undefined, state: { phase: "paused", received: e.state.received, size: e.state.size } });
}

/** resumeUpload goes on with the file this tab still holds. */
export function resumeUpload(project: string, name: string) {
  const e = entries.get(uploadKey(project, name));
  if (e && e.state.phase !== "done") startUpload(project, name, e.file, "received" in e.state ? e.state.received : 0, e.chunk);
}

/** forgetUpload drops this tab's record of an image's upload, as when the image is deleted. */
export function forgetUpload(project: string, name: string) {
  const key = uploadKey(project, name);
  entries.get(key)?.abort?.abort();
  entries.delete(key);
  for (const l of listeners) l();
}

async function run(key: string, project: string, name: string, file: File, from: number, chunkBytes: number, signal: AbortSignal) {
  let at = from;
  let chunk = chunkBytes;
  let tries = 0;
  // The rate is measured over the last few chunks.
  const recent: { t: number; n: number }[] = [{ t: performance.now(), n: at }];
  const update = (state: UploadState) => {
    const e = entries.get(key);
    if (e && !signal.aborted) set(key, { ...e, state });
  };
  while (at < file.size && !signal.aborted) {
    try {
      at = await putChunk(project, name, file, at, file.size, chunk, signal);
      tries = 0;
      recent.push({ t: performance.now(), n: at });
      if (recent.length > 6) recent.shift();
      const span = (recent[recent.length - 1].t - recent[0].t) / 1000;
      update({ phase: "uploading", received: at, size: file.size, bytesPerSecond: span > 0 ? (at - recent[0].n) / span : 0 });
    } catch (err) {
      if (signal.aborted) return;
      if (err instanceof UploadError && err.status === 409 && err.offset !== undefined && err.offset !== at) {
        // The node counted otherwise, such as after another tab's chunk: go on from its count.
        at = err.offset;
        continue;
      }
      // A 409 at the offset sent tells no progress; it is waited out below, never sent again at once.
      if (err instanceof UploadError && err.status === 413 && chunk > 1 << 20) {
        chunk = Math.floor(chunk / 2);
        continue;
      }
      const message = err instanceof Error ? err.message : String(err);
      if (err instanceof UploadError && final(err.status)) {
        update({ phase: "failed", received: at, size: file.size, error: message });
        return;
      }
      const backoff = retryDelays[Math.min(tries++, retryDelays.length - 1)];
      const wait = err instanceof UploadError && err.retryAfter ? Math.max(err.retryAfter * 1000, 1000) : backoff;
      update({ phase: "retrying", received: at, size: file.size, error: message, inSeconds: Math.round(wait / 1000) });
      await new Promise((r) => setTimeout(r, wait));
    }
  }
  if (!signal.aborted) update({ phase: "done", size: file.size });
}

/** ImageFormat is what a file's first bytes say it is. */
export type ImageFormat = "qcow2" | "raw" | "unknown";

/**
 * sniffFormat reads a file's first bytes: qcow2 by its magic, and raw for a
 * whole number of 512-byte sectors that is nothing else the node refuses
 * by its magic (an ISO, vmdk or vhdx). It only guides the form; the
 * converter decides.
 */
export async function sniffFormat(file: Blob): Promise<ImageFormat> {
  const head = new Uint8Array(await file.slice(0, 0x8008).arrayBuffer());
  const at = (o: number, s: string) => [...s].every((c, i) => head[o + i] === c.charCodeAt(0));
  if (head[0] === 0x51 && head[1] === 0x46 && head[2] === 0x49 && head[3] === 0xfb) return "qcow2";
  if (at(0, "KDMV") || at(0, "vhdxfile") || at(0x8001, "CD001")) return "unknown";
  return file.size % 512 === 0 ? "raw" : "unknown";
}
