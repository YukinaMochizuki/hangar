// How the console reaches the API: Connect over fetch, to the origin the
// console was loaded from. hangar-edge serves the console and the API on one
// origin, so the browser sends the session cookie (HttpOnly, never readable
// here) with every call, and its Origin header, which the apiserver checks
// before it believes the cookie. See docs/decisions/console-design.md.

import { Code, ConnectError } from "@connectrpc/connect";
import { createConnectTransport, type ConnectTransportOptions } from "@connectrpc/connect-web";

/** The transport's options for the console at origin. */
export function transportOptions(origin: string): ConnectTransportOptions {
  return {
    baseUrl: origin,
    // Every call is a POST, which always carries Origin. Connect's GET form
    // for side-effect-free methods is never used: a GET may come without
    // Origin, and the apiserver refuses the cookie on one.
    useHttpGet: false,
    fetch: (input, init) => fetch(input, { ...init, credentials: "same-origin", redirect: "error" }),
  };
}

/** The transport every call goes through, to the origin the console came from. */
export function makeTransport() {
  return createConnectTransport(transportOptions(window.location.origin));
}

/**
 * Whether a failed query is worth asking again: only when the API or the
 * way to it failed, never when it answered no.
 */
export function shouldRetry(failures: number, err: unknown): boolean {
  if (failures >= 2) return false;
  if (!(err instanceof ConnectError)) return true;
  return [Code.Unavailable, Code.DeadlineExceeded, Code.Internal, Code.Unknown, Code.Aborted].includes(err.code);
}

/** Whether err is the API saying the caller is not signed in. */
export function isUnauthenticated(err: unknown): boolean {
  return err instanceof ConnectError && err.code === Code.Unauthenticated;
}

export function isNotFound(err: unknown): boolean {
  return err instanceof ConnectError && err.code === Code.NotFound;
}

export function isPermissionDenied(err: unknown): boolean {
  return err instanceof ConnectError && err.code === Code.PermissionDenied;
}

/** The API's own words for what went wrong, without Connect's code prefix. */
export function errorMessage(err: unknown): string {
  if (err instanceof ConnectError) return err.rawMessage || codeText(err.code);
  if (err instanceof Error) return err.message;
  return String(err);
}

function codeText(code: Code): string {
  switch (code) {
    case Code.Unavailable:
      return "The API is unavailable.";
    case Code.DeadlineExceeded:
      return "The API did not answer in time.";
    case Code.PermissionDenied:
      return "Permission denied.";
    default:
      return `The API answered ${Code[code]}.`;
  }
}

/** Starts a browser sign-in that comes back to the page it started on. */
export function signInURL(): string {
  const back = window.location.pathname + window.location.search;
  return `/auth/login?return=${encodeURIComponent(back)}`;
}

/** Ends the session. The apiserver checks Origin on the POST, as on every other. */
export async function signOut(): Promise<void> {
  const res = await fetch("/auth/logout", { method: "POST", credentials: "same-origin" });
  if (!res.ok && res.status !== 204) throw new Error(`Signing out failed: HTTP ${res.status}.`);
}
