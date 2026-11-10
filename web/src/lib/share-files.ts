// What a file share holds, as FileShareFilesService lists it: one directory
// at a time, sorted by name as bytes, a page at a time
// (docs/decisions/file-shares.md). Names are bytes, which need not be
// UTF-8: they are shown with every byte that is not part of a UTF-8
// character, and the bytes of every control and bidirectional formatting
// character, escaped as \xNN, and carried in the page's URL as
// percent-encoded bytes, so any name round-trips.

import { Code, ConnectError } from "@connectrpc/connect";
import { useInfiniteQuery } from "@connectrpc/connect-query";
import { FileShareEntryType, FileShareFilesService, type FileShareEntry } from "@hangar/api/hangar/storage/v1alpha1/file_share_files_pb";

/** How many entries a page asks for: the API's default. */
export const filesPageSize = 500;

/** A share's directory, as its names from the root; none is the root. */
export type SharePath = Uint8Array[];

const hex = (b: number) => b.toString(16).padStart(2, "0").toUpperCase();

/** The length of the UTF-8 character starting at bytes[i], or 0 when none starts there. */
function utf8Length(bytes: Uint8Array, i: number): number {
  const b = bytes[i];
  const cont = (k: number) => i + k < bytes.length && (bytes[i + k] & 0xc0) === 0x80;
  if (b < 0x80) return 1;
  if (b >= 0xc2 && b <= 0xdf) return cont(1) ? 2 : 0;
  if (b >= 0xe0 && b <= 0xef) {
    const b1 = bytes[i + 1];
    if ((b === 0xe0 && b1 < 0xa0) || (b === 0xed && b1 >= 0xa0)) return 0;
    return cont(1) && cont(2) ? 3 : 0;
  }
  if (b >= 0xf0 && b <= 0xf4) {
    const b1 = bytes[i + 1];
    if ((b === 0xf0 && b1 < 0x90) || (b === 0xf4 && b1 >= 0x90)) return 0;
    return cont(1) && cont(2) && cont(3) ? 4 : 0;
  }
  return 0;
}

const decoder = new TextDecoder();

/**
 * Whether a character is shown escaped: a control character (C0, DEL, C1)
 * or a bidirectional formatting one, which could make a name read as
 * another (U+202E turns "fdp.exe" around).
 */
function escaped(cp: number): boolean {
  return cp < 0x20 || (cp >= 0x7f && cp <= 0x9f) || cp === 0x061c || cp === 0x200e || cp === 0x200f || (cp >= 0x202a && cp <= 0x202e) || (cp >= 0x2066 && cp <= 0x2069);
}

/**
 * A name as shown: its UTF-8 characters as they are, and every other byte,
 * and the bytes of every character escaped() holds, as \xNN; utf8 says
 * whether it was valid UTF-8 throughout.
 */
export function displayName(name: Uint8Array): { text: string; utf8: boolean } {
  let text = "";
  let utf8 = true;
  for (let i = 0; i < name.length; ) {
    const n = utf8Length(name, i);
    if (n === 0) {
      utf8 = false;
      text += `\\x${hex(name[i])}`;
      i++;
      continue;
    }
    const ch = decoder.decode(name.subarray(i, i + n));
    text += escaped(ch.codePointAt(0)!) ? Array.from(name.subarray(i, i + n), (b) => `\\x${hex(b)}`).join("") : ch;
    i += n;
  }
  return { text, utf8 };
}

const unreserved = /[A-Za-z0-9\-._~]/;

/** A path as the page's URL carries it: each name's bytes percent-encoded, joined by "/". */
export function encodePath(path: SharePath): string {
  return path
    .map((name) => Array.from(name, (b) => (b < 0x80 && unreserved.test(String.fromCharCode(b)) ? String.fromCharCode(b) : `%${hex(b)}`)).join(""))
    .join("/");
}

/** The path a URL's encodePath gave; undefined for one no path gives. */
export function decodePath(s: string | undefined): SharePath | undefined {
  if (!s) return [];
  const out: SharePath = [];
  for (const part of s.split("/")) {
    const bytes: number[] = [];
    for (let i = 0; i < part.length; i++) {
      if (part[i] === "%") {
        const digits = part.slice(i + 1, i + 3);
        if (!/^[0-9A-Fa-f]{2}$/.test(digits)) return undefined;
        bytes.push(Number.parseInt(digits, 16));
        i += 2;
      } else {
        const c = part.charCodeAt(i);
        if (c >= 0x80) return undefined;
        bytes.push(c);
      }
    }
    if (bytes.length === 0) return undefined;
    out.push(Uint8Array.from(bytes));
  }
  return out;
}

const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];

/** A size in bytes for people: "0 B", "512 B", "1.5 KiB", "20 GiB". */
export function formatSize(bytes: bigint): string {
  let v = Number(bytes);
  let u = 0;
  while (v >= 1024 && u < units.length - 1) {
    v /= 1024;
    u++;
  }
  return u === 0 ? `${v} B` : `${v < 10 ? v.toFixed(1).replace(/\.0$/, "") : Math.round(v)} ${units[u]}`;
}

/** What an entry is, in a word. */
export function typeWord(t: FileShareEntryType): string {
  switch (t) {
    case FileShareEntryType.DIRECTORY:
      return "Directory";
    case FileShareEntryType.REGULAR:
      return "File";
    case FileShareEntryType.SYMLINK:
      return "Symlink";
    default:
      return "Other";
  }
}

/** Why a listing failed, as its page tells it. */
export type FilesFailure =
  | { kind: "serverDown"; message: string }
  | { kind: "oldServer" }
  | { kind: "notFound" }
  | { kind: "refused"; message: string }
  | { kind: "tooLarge" }
  | { kind: "deleting" }
  | { kind: "other"; error: unknown };

/** The failure an error from ListFiles is. */
export function filesFailure(err: unknown): FilesFailure {
  if (!(err instanceof ConnectError)) return { kind: "other", error: err };
  switch (err.code) {
    case Code.Unavailable:
      return { kind: "serverDown", message: err.rawMessage };
    case Code.Unimplemented:
      return { kind: "oldServer" };
    case Code.NotFound:
      return { kind: "notFound" };
    case Code.FailedPrecondition:
      return { kind: "refused", message: err.rawMessage };
    case Code.ResourceExhausted:
      return { kind: "tooLarge" };
  }
  return { kind: "other", error: err };
}

/**
 * The share's directory at path, its first page asked once and each next
 * one when loadMore is called; nothing is asked again on its own, as a
 * listing reads the whole directory on the share's server.
 */
export function useShareFiles(project: string, name: string, path: SharePath) {
  const q = useInfiniteQuery(
    FileShareFilesService.method.listFiles,
    { project, name, path, pageSize: filesPageSize, pageToken: "" },
    { pageParamKey: "pageToken", getNextPageParam: (last) => last.nextPageToken || undefined, retry: false, staleTime: Infinity, refetchOnWindowFocus: false },
  );
  return {
    entries: q.data?.pages.flatMap((p) => p.entries) ?? [],
    loading: q.isPending,
    // A failure of a later page leaves the pages listed before it shown.
    error: q.isError && !q.data ? q.error : undefined,
    moreError: q.isFetchNextPageError ? q.error : undefined,
    more: q.hasNextPage,
    loadingMore: q.isFetchingNextPage,
    loadMore: () => void q.fetchNextPage(),
    refetch: () => void q.refetch(),
  };
}

export type { FileShareEntry };
