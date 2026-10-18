import { create, fromBinary, toBinary, type MessageInitShape } from "@bufbuild/protobuf";
import { Code } from "@connectrpc/connect";
import {
  MachineExecRequestSchema,
  MachineExecResponseSchema,
  type MachineExecRequest,
} from "@hangar/api/hangar/compute/v1alpha1/machine_capability_pb";
import { afterEach, describe, expect, it, vi } from "vitest";
import { endOf, execSubprotocol, execURL, openExec, type ExecEnd } from "@/lib/exec";

/** A stand-in for the browser's WebSocket: what the client sent, and the server's side to play. */
class FakeSocket {
  readyState = 0;
  // As in a browser: binary messages arrive as Blobs unless asked for as ArrayBuffers.
  binaryType: BinaryType = "blob";
  sent: (Uint8Array | ArrayBuffer)[] = [];
  closedWith?: number;
  onopen: (() => void) | null = null;
  onmessage: ((e: { data: unknown }) => void) | null = null;
  onclose: ((e: { code: number; reason: string }) => void) | null = null;
  constructor(
    readonly url: string,
    readonly protocols: string[],
  ) {}
  send(d: Uint8Array | ArrayBuffer) {
    this.sent.push(d);
  }
  close(code: number) {
    this.closedWith = code;
  }
  // The server's side.
  accept() {
    this.readyState = 1;
    this.onopen?.();
  }
  reply(m: MessageInitShape<typeof MachineExecResponseSchema>) {
    const b = toBinary(MachineExecResponseSchema, create(MachineExecResponseSchema, m));
    const data = b.buffer.slice(b.byteOffset, b.byteOffset + b.byteLength);
    this.onmessage?.({ data: this.binaryType === "arraybuffer" ? data : new Blob([data]) });
  }
  end(code: number, reason = "") {
    this.readyState = 3;
    this.onclose?.({ code, reason });
  }
  requests(): MachineExecRequest[] {
    return this.sent.filter((d) => (d as Uint8Array).byteLength > 0).map((d) => fromBinary(MachineExecRequestSchema, d as Uint8Array));
  }
}

function start() {
  let socket!: FakeSocket;
  const output: string[] = [];
  const ends: ExecEnd[] = [];
  const decoder = new TextDecoder();
  const exec = openExec(
    { project: "web", plural: "containers", name: "api", command: ["sh"], cols: 120, rows: 40 },
    { onOutput: (d) => output.push(decoder.decode(d)), onEnd: (e) => ends.push(e) },
    {
      origin: "https://console.example.com",
      socket: (u, p) => {
        socket = new FakeSocket(u, p);
        return socket as unknown as WebSocket;
      },
    },
  );
  return { exec, socket, output, ends };
}

afterEach(() => {
  vi.useRealTimers();
});

describe("execURL", () => {
  it("is the object's exec path, on the console's origin, over wss for https", () => {
    expect(execURL({ project: "web", plural: "containers", name: "api" }, "https://console.example.com/projects/web")).toBe(
      "wss://console.example.com/v1/projects/web/containers/api:exec",
    );
    expect(execURL({ project: "web", plural: "containers", name: "api" }, "http://127.0.0.1:4173")).toBe("ws://127.0.0.1:4173/v1/projects/web/containers/api:exec");
  });
});

describe("openExec", () => {
  it("asks for the exec subprotocol and starts a shell on a terminal of the given size", () => {
    const { socket } = start();
    expect(socket.protocols).toEqual([execSubprotocol]);
    expect(socket.sent).toEqual([]);
    socket.accept();
    const [first] = socket.requests();
    expect(first.message.case).toBe("start");
    if (first.message.case !== "start") return;
    expect(first.message.value.command).toEqual(["sh"]);
    expect(first.message.value.tty).toBe(true);
    expect(first.message.value.stdin).toBe(true);
    expect(first.message.value.size).toMatchObject({ cols: 120, rows: 40 });
  });

  it("sends typed text as stdin and a new size as resize, after the start", () => {
    const { exec, socket } = start();
    socket.accept();
    exec.send("ls\r");
    exec.resize(80, 24);
    const [, stdin, resize] = socket.requests();
    expect(stdin.message.case).toBe("stdin");
    expect(new TextDecoder().decode(stdin.message.value as Uint8Array)).toBe("ls\r");
    expect(resize.message).toMatchObject({ case: "resize", value: { cols: 80, rows: 24 } });
  });

  it("keeps what is typed while connecting, and sends it after the start, in order", () => {
    const { exec, socket } = start();
    exec.send("l");
    exec.resize(100, 30);
    exec.send("s");
    expect(socket.sent).toEqual([]);
    socket.accept();
    expect(socket.requests().map((r) => r.message.case)).toEqual(["start", "stdin", "resize", "stdin"]);
  });

  it("sends nothing once it has ended", () => {
    const { exec, socket } = start();
    socket.accept();
    socket.end(1006);
    const before = socket.sent.length;
    exec.send("x");
    expect(socket.sent.length).toBe(before);
  });

  it("gives the server's error close as the API's error, through the socket", () => {
    const { socket, ends } = start();
    socket.accept();
    socket.end(4009, "the container is not running");
    expect(ends).toHaveLength(1);
    expect(ends[0]).toMatchObject({ kind: "failed", error: { code: Code.FailedPrecondition, rawMessage: "the container is not running" } });
  });

  it("passes stdout and stderr on, and ends with the exit the server sent before closing", () => {
    const { socket, output, ends } = start();
    socket.accept();
    socket.reply({ message: { case: "stdout", value: new TextEncoder().encode("hello\r\n") } });
    socket.reply({ message: { case: "stderr", value: new TextEncoder().encode("oops\r\n") } });
    socket.reply({ message: { case: "exit", value: { code: 130, signal: 2 } } });
    socket.end(1000);
    expect(output).toEqual(["hello\r\n", "oops\r\n"]);
    expect(ends).toEqual([{ kind: "exited", code: 130, signal: 2 }]);
  });

  it("ignores the server's empty keepalive messages", () => {
    const { socket, output } = start();
    socket.accept();
    socket.onmessage?.({ data: new ArrayBuffer(0) });
    expect(output).toEqual([]);
  });

  it("sends an empty message every 30 seconds while open, and stops once ended", () => {
    vi.useFakeTimers();
    const { socket } = start();
    socket.accept();
    const before = socket.sent.length;
    vi.advanceTimersByTime(30_000);
    expect(socket.sent.length).toBe(before + 1);
    expect((socket.sent.at(-1) as Uint8Array).byteLength).toBe(0);
    socket.end(1006);
    vi.advanceTimersByTime(60_000);
    expect(socket.sent.length).toBe(before + 1);
  });

  it("closes with 1000 when the caller closes it, ending once", () => {
    const { exec, socket, ends } = start();
    socket.accept();
    exec.close();
    socket.end(1000);
    expect(socket.closedWith).toBe(1000);
    expect(ends).toEqual([{ kind: "closed" }]);
  });
});

describe("endOf", () => {
  it("reads 4000 plus a Connect code as the API's error, with the reason as its message", () => {
    const e = endOf(4009, "the container is not running", true, undefined);
    expect(e.kind).toBe("failed");
    if (e.kind !== "failed") return;
    expect(e.error.code).toBe(Code.FailedPrecondition);
    expect(e.error.rawMessage).toBe("the container is not running");
  });

  it("calls a close before the socket opened unreachable: a refusal before the upgrade has no reason in a browser", () => {
    expect(endOf(1006, "", false, undefined)).toEqual({ kind: "unreachable" });
  });

  it("calls an abnormal close after it opened lost, and a normal one with no exit closed", () => {
    expect(endOf(1006, "", true, undefined)).toEqual({ kind: "lost" });
    expect(endOf(1000, "", true, undefined)).toEqual({ kind: "closed" });
  });
});
