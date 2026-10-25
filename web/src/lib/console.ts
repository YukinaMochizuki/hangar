// A virtual machine's serial console from the browser: the WebSocket the
// API serves at /v1/projects/{project}/{plural}/{name}:console, one
// MachineConsoleRequest or MachineConsoleResponse per binary message
// (proto/hangar/compute/v1alpha1/machine_capability.proto,
// docs/decisions/serial-console.md). It is reached and refused as an exec
// is (web/src/lib/exec.ts): the session cookie and Origin on the
// handshake, a close of 4000 plus a Connect code for a refusal after it.

import { create, fromBinary, toBinary, type MessageInitShape } from "@bufbuild/protobuf";
import { MachineConsoleRequestSchema, MachineConsoleResponseSchema } from "@hangar/api/hangar/compute/v1alpha1/machine_capability_pb";
import { capabilityURL, endOf, type ExecEnd } from "@/lib/exec";

/** The subprotocol the apiserver requires of a serial console. */
export const consoleSubprotocol = "hangar.console.v1alpha1";

// As an exec's: the server pings every 30 s, and an empty message as often
// keeps an idle-dropping proxy from closing it.
const keepaliveMs = 30_000;

// WebSocket.OPEN, which a test's stand-in socket need not define.
const open = 1;

/** The screen's size in character cells, shared by every client of the console. */
export interface ConsoleSize {
  cols: number;
  rows: number;
}

/** The bounds the console takes a size within, as MachineConsoleSize says. */
export const consoleBounds = { cols: { min: 1, max: 500 }, rows: { min: 1, max: 200 } } as const;

/** How the client is attached: whether it holds the input, and the screen's size. */
export interface ConsoleState {
  input: boolean;
  size: ConsoleSize;
}

/** What a console tells its caller. */
export interface ConsoleEvents {
  onOpen?: () => void;
  /** The client's state: first, and again whenever it changes. */
  onState: (s: ConsoleState) => void;
  onOutput: (data: Uint8Array) => void;
  onEnd: (end: ExecEnd) => void;
}

/** Whose console, and how to attach to it. */
export interface ConsoleTarget {
  project: string;
  /** The kind's plural, as its resource names it: "virtualmachines". */
  plural: string;
  name: string;
  /** Only watches: never holds the input. */
  readOnly?: boolean;
  /** Takes the input from whoever holds it, who goes on watching. */
  force?: boolean;
}

/** An attached console. */
export interface Console {
  /** Types, reaching the guest only while the client holds the input. */
  send: (data: string | Uint8Array) => void;
  /** Sets the screen's size for every client, taken only from the one holding the input. */
  resize: (cols: number, rows: number) => void;
  close: () => void;
}

const encoder = new TextEncoder();

type Request = MessageInitShape<typeof MachineConsoleRequestSchema>["message"];

/** openConsole attaches to the object's virtual machine's serial console. */
export function openConsole(
  t: ConsoleTarget,
  events: ConsoleEvents,
  opts: { origin?: string; socket?: (url: string, protocols: string[]) => WebSocket } = {},
): Console {
  const url = capabilityURL(t, "console", opts.origin ?? window.location.origin);
  const ws = (opts.socket ?? ((u, p) => new WebSocket(u, p)))(url, [consoleSubprotocol]);
  ws.binaryType = "arraybuffer";
  let opened = false;
  let ended = false;
  let keepalive: ReturnType<typeof setInterval> | undefined;
  // What is typed before the start is sent goes after it, in order.
  let queued: Request[] = [];

  const send = (m: Request) => ws.send(toBinary(MachineConsoleRequestSchema, create(MachineConsoleRequestSchema, { message: m })));
  const write = (m: Request) => {
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
    send({ case: "start", value: { readOnly: !!t.readOnly, force: !!t.force } });
    for (const m of queued) send(m);
    queued = [];
    keepalive = setInterval(() => {
      if (ws.readyState === open) ws.send(new Uint8Array(0));
    }, keepaliveMs);
    events.onOpen?.();
  };
  ws.onmessage = (e: MessageEvent) => {
    if (!(e.data instanceof ArrayBuffer) || e.data.byteLength === 0) return;
    const r = fromBinary(MachineConsoleResponseSchema, new Uint8Array(e.data));
    switch (r.message.case) {
      case "output":
        events.onOutput(r.message.value);
        break;
      case "state": {
        const s = r.message.value;
        events.onState({ input: s.input, size: { cols: s.size?.columns || 80, rows: s.size?.rows || 24 } });
        break;
      }
    }
  };
  // A console has no exit: its end is the API's, or the connection's.
  ws.onclose = (e: CloseEvent) => end(endOf(e.code, e.reason, opened, undefined));

  return {
    send: (data) => write({ case: "input", value: typeof data === "string" ? encoder.encode(data) : data }),
    resize: (cols, rows) => write({ case: "resize", value: { columns: cols, rows } }),
    close: () => {
      end({ kind: "closed" });
      ws.close(1000);
    },
  };
}
