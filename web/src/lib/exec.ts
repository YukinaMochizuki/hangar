// An exec in a machine from the browser: the WebSocket the API serves at
// /v1/projects/{project}/{plural}/{name}:exec, one MachineExecRequest or
// MachineExecResponse per binary message
// (proto/hangar/compute/v1alpha1/machine_capability.proto,
// docs/decisions/streaming-transport.md). The browser sends the session
// cookie and Origin on the handshake, which the apiserver checks as it does
// for a Connect call.

import { create, fromBinary, toBinary, type MessageInitShape } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { MachineExecRequestSchema, MachineExecResponseSchema } from "@hangar/api/hangar/compute/v1alpha1/machine_capability_pb";

/** The subprotocol the apiserver requires of an exec. */
export const execSubprotocol = "hangar.exec.v1alpha1";

// The server pings every 30 s; an empty message as often keeps a proxy that
// drops idle connections (Cloudflare's, after 100 s) from closing it.
const keepaliveMs = 30_000;

// A close status of 4000 plus a Connect code is an exec that failed with it.
const closeBase = 4000;

// WebSocket.OPEN, which a test's stand-in socket need not define.
const open = 1;

/** How an exec ended: the process exited, the API ended it, or the connection did. */
export type ExecEnd =
  | { kind: "exited"; code: number; signal: number }
  | { kind: "failed"; error: ConnectError }
  | { kind: "unreachable" }
  | { kind: "lost" }
  | { kind: "closed" };

/** What an exec tells its caller. */
export interface ExecEvents {
  onOpen?: () => void;
  onOutput: (data: Uint8Array) => void;
  onEnd: (end: ExecEnd) => void;
}

/** What starts an exec: the object, the program and the terminal's first size. */
export interface ExecTarget {
  project: string;
  /** The kind's plural, as its resource names it, such as "containers". */
  plural: string;
  name: string;
  command: string[];
  cols: number;
  rows: number;
}

/** A running exec. */
export interface Exec {
  send: (data: string | Uint8Array) => void;
  resize: (cols: number, rows: number) => void;
  close: () => void;
}

/** The exec URL of an object, on the origin the console was loaded from. */
export function execURL(t: Pick<ExecTarget, "project" | "plural" | "name">, origin: string): string {
  const u = new URL(origin);
  u.protocol = u.protocol === "https:" ? "wss:" : "ws:";
  u.pathname = `/v1/projects/${encodeURIComponent(t.project)}/${encodeURIComponent(t.plural)}/${encodeURIComponent(t.name)}:exec`;
  u.search = "";
  u.hash = "";
  return u.toString();
}

/**
 * endOf says how an exec ended from its close and what came before it. A
 * refusal before the upgrade reaches a browser as 1006 with no reason, which
 * is told apart from a connection lost later only by whether it opened.
 */
export function endOf(code: number, reason: string, opened: boolean, exit: { code: number; signal: number } | undefined): ExecEnd {
  if (exit) return { kind: "exited", ...exit };
  if (code >= closeBase + 1 && code <= closeBase + Code.Unauthenticated) {
    return { kind: "failed", error: new ConnectError(reason, (code - closeBase) as Code) };
  }
  if (!opened) return { kind: "unreachable" };
  if (code === 1000) return { kind: "closed" };
  return { kind: "lost" };
}

const encoder = new TextEncoder();

/** openExec starts a process on a terminal, its stdin open, in the object's machine. */
export function openExec(
  t: ExecTarget,
  events: ExecEvents,
  opts: { origin?: string; socket?: (url: string, protocols: string[]) => WebSocket } = {},
): Exec {
  const url = execURL(t, opts.origin ?? window.location.origin);
  const ws = (opts.socket ?? ((u, p) => new WebSocket(u, p)))(url, [execSubprotocol]);
  ws.binaryType = "arraybuffer";
  let opened = false;
  let ended = false;
  let exit: { code: number; signal: number } | undefined;
  let keepalive: ReturnType<typeof setInterval> | undefined;
  // What is typed or resized before the start is sent goes after it, in order.
  let queued: MessageInitShape<typeof MachineExecRequestSchema>["message"][] = [];

  const send = (m: MessageInitShape<typeof MachineExecRequestSchema>["message"]) =>
    ws.send(toBinary(MachineExecRequestSchema, create(MachineExecRequestSchema, { message: m })));
  const write = (m: MessageInitShape<typeof MachineExecRequestSchema>["message"]) => {
    if (ended) return;
    if (!opened) queued.push(m);
    else if (ws.readyState === open) send(m);
  };
  const end = (e: ExecEnd) => {
    if (ended) return;
    ended = true;
    clearInterval(keepalive);
    events.onEnd(e);
  };

  ws.onopen = () => {
    opened = true;
    send({
      case: "start",
      value: { command: t.command, stdin: true, tty: true, size: { rows: t.rows, cols: t.cols } },
    });
    for (const m of queued) send(m);
    queued = [];
    keepalive = setInterval(() => {
      if (ws.readyState === open) ws.send(new Uint8Array(0));
    }, keepaliveMs);
    events.onOpen?.();
  };
  ws.onmessage = (e: MessageEvent) => {
    if (!(e.data instanceof ArrayBuffer) || e.data.byteLength === 0) return;
    const r = fromBinary(MachineExecResponseSchema, new Uint8Array(e.data));
    switch (r.message.case) {
      case "stdout":
      case "stderr":
        events.onOutput(r.message.value);
        break;
      case "exit":
        exit = { code: r.message.value.code, signal: r.message.value.signal };
        break;
    }
  };
  ws.onclose = (e: CloseEvent) => end(endOf(e.code, e.reason, opened, exit));

  return {
    send: (data) => write({ case: "stdin", value: typeof data === "string" ? encoder.encode(data) : data }),
    resize: (cols, rows) => write({ case: "resize", value: { cols, rows } }),
    close: () => {
      end({ kind: "closed" });
      ws.close(1000);
    },
  };
}
