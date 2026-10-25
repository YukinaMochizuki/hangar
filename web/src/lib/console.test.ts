import { create, fromBinary, toBinary, type MessageInitShape } from "@bufbuild/protobuf";
import { Code } from "@connectrpc/connect";
import {
  MachineConsoleRequestSchema,
  MachineConsoleResponseSchema,
  type MachineConsoleRequest,
} from "@hangar/api/hangar/compute/v1alpha1/machine_capability_pb";
import { afterEach, describe, expect, it, vi } from "vitest";
import { consoleSubprotocol, openConsole, type ConsoleState, type ConsoleTarget } from "@/lib/console";
import type { ExecEnd } from "@/lib/exec";

/** A stand-in for the browser's WebSocket: what the client sent, and the server's side to play. */
class FakeSocket {
  readyState = 0;
  binaryType: BinaryType = "blob";
  sent: Uint8Array[] = [];
  closedWith?: number;
  onopen: (() => void) | null = null;
  onmessage: ((e: { data: unknown }) => void) | null = null;
  onclose: ((e: { code: number; reason: string }) => void) | null = null;
  constructor(
    readonly url: string,
    readonly protocols: string[],
  ) {}
  send(d: Uint8Array) {
    this.sent.push(d);
  }
  close(code: number) {
    this.closedWith = code;
  }
  accept() {
    this.readyState = 1;
    this.onopen?.();
  }
  reply(m: MessageInitShape<typeof MachineConsoleResponseSchema>) {
    const b = toBinary(MachineConsoleResponseSchema, create(MachineConsoleResponseSchema, m));
    const data = b.buffer.slice(b.byteOffset, b.byteOffset + b.byteLength);
    this.onmessage?.({ data: this.binaryType === "arraybuffer" ? data : new Blob([data]) });
  }
  end(code: number, reason = "") {
    this.readyState = 3;
    this.onclose?.({ code, reason });
  }
  requests(): MachineConsoleRequest[] {
    return this.sent.filter((d) => d.byteLength > 0).map((d) => fromBinary(MachineConsoleRequestSchema, d));
  }
}

function attach(over: Partial<ConsoleTarget> = {}) {
  let socket!: FakeSocket;
  const output: string[] = [];
  const states: ConsoleState[] = [];
  const ends: ExecEnd[] = [];
  const decoder = new TextDecoder();
  const c = openConsole(
    { project: "web", plural: "virtualmachines", name: "db", ...over },
    { onOutput: (d) => output.push(decoder.decode(d)), onState: (s) => states.push(s), onEnd: (e) => ends.push(e) },
    {
      origin: "https://console.example.com",
      socket: (u, p) => {
        socket = new FakeSocket(u, p);
        return socket as unknown as WebSocket;
      },
    },
  );
  return { c, socket, output, states, ends };
}

afterEach(() => {
  vi.useRealTimers();
});

describe("openConsole", () => {
  it("asks for the console's path and subprotocol, and starts as asked", () => {
    const { socket } = attach({ force: true });
    expect(socket.url).toBe("wss://console.example.com/v1/projects/web/virtualmachines/db:console");
    expect(socket.protocols).toEqual([consoleSubprotocol]);
    expect(socket.binaryType).toBe("arraybuffer");
    socket.accept();
    expect(socket.requests()[0].message).toMatchObject({ case: "start", value: { readOnly: false, force: true } });
  });

  it("keeps what is typed and sized while connecting, and sends it after the start, in order", () => {
    const { c, socket } = attach();
    c.send("root\r");
    c.resize(120, 40);
    expect(socket.sent).toEqual([]);
    socket.accept();
    const [start, input, resize] = socket.requests();
    expect(start.message.case).toBe("start");
    expect(new TextDecoder().decode(input.message.value as Uint8Array)).toBe("root\r");
    expect(resize.message).toMatchObject({ case: "resize", value: { columns: 120, rows: 40 } });
  });

  it("passes the state and the screen on, in the order they came", () => {
    const { socket, output, states } = attach();
    socket.accept();
    socket.reply({ message: { case: "state", value: { input: false, size: { columns: 100, rows: 30 } } } });
    socket.reply({ message: { case: "output", value: new TextEncoder().encode("login: ") } });
    socket.reply({ message: { case: "state", value: { input: true, size: { columns: 100, rows: 30 } } } });
    expect(states).toEqual([
      { input: false, size: { cols: 100, rows: 30 } },
      { input: true, size: { cols: 100, rows: 30 } },
    ]);
    expect(output).toEqual(["login: "]);
  });

  it("takes a state with no size as 80x24", () => {
    const { socket, states } = attach();
    socket.accept();
    socket.reply({ message: { case: "state", value: { input: true } } });
    expect(states).toEqual([{ input: true, size: { cols: 80, rows: 24 } }]);
  });

  it("gives the server's error close as the API's error", () => {
    const { socket, ends } = attach();
    socket.accept();
    socket.end(4008, "a client fell behind");
    expect(ends).toEqual([expect.objectContaining({ kind: "failed", error: expect.objectContaining({ code: Code.ResourceExhausted }) })]);
  });

  it("calls a refusal before the upgrade unreachable, and the VM's end a normal close", () => {
    const refused = attach();
    refused.socket.end(1006);
    expect(refused.ends).toEqual([{ kind: "unreachable" }]);
    const stopped = attach();
    stopped.socket.accept();
    stopped.socket.end(1000);
    expect(stopped.ends).toEqual([{ kind: "closed" }]);
  });

  it("keeps the connection alive every 30 seconds, and stops once ended", () => {
    vi.useFakeTimers();
    const { socket } = attach();
    socket.accept();
    const before = socket.sent.length;
    vi.advanceTimersByTime(30_000);
    expect(socket.sent.length).toBe(before + 1);
    expect(socket.sent.at(-1)?.byteLength).toBe(0);
    socket.end(1006);
    vi.advanceTimersByTime(60_000);
    expect(socket.sent.length).toBe(before + 1);
  });

  it("closes with 1000 when the caller closes it, ending once and sending nothing after", () => {
    const { c, socket, ends } = attach();
    socket.accept();
    c.close();
    socket.end(1000);
    const before = socket.sent.length;
    c.send("x");
    expect(socket.closedWith).toBe(1000);
    expect(ends).toEqual([{ kind: "closed" }]);
    expect(socket.sent.length).toBe(before);
  });
});
