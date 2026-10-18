// @vitest-environment jsdom

// useTerminal's lifecycle: one exec at a time, ended by leaving, by closing,
// by connecting again and by the page going into the back/forward cache;
// nothing drawn where WebGL is missing; mouse bytes sent as stdin. xterm and
// the browser's WebSocket are stand-ins that record what is done to them.

import { fromBinary } from "@bufbuild/protobuf";
import { MachineExecRequestSchema } from "@hangar/api/hangar/compute/v1alpha1/machine_capability_pb";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useTerminal } from "@/components/terminal";

// Hoisted with vi.mock, so the terminal's dynamic import gets these whenever it runs.
const { terms, FakeTerm } = vi.hoisted(() => {
  const terms: InstanceType<typeof FakeTerm>[] = [];
  class FakeTerm {
    cols = 80;
    rows = 24;
    options: Record<string, unknown> = {};
    disposed = false;
    opened = false;
    binary?: (d: string) => void;
    constructor() {
      terms.push(this);
    }
    loadAddon() {}
    open() {
      this.opened = true;
    }
    write() {}
    focus() {}
    dispose() {
      this.disposed = true;
    }
    onData() {}
    onBinary(cb: (d: string) => void) {
      this.binary = cb;
    }
    onResize() {}
  }
  return { terms, FakeTerm };
});
vi.mock("@/components/xterm", () => ({
  Terminal: FakeTerm,
  FitAddon: class {
    fit() {}
  },
  WebglAddon: class {
    onContextLoss() {}
    dispose() {}
  },
}));

const sockets: FakeSocket[] = [];
class FakeSocket {
  static OPEN = 1;
  readyState = 0;
  binaryType = "blob";
  closed?: number;
  sent: Uint8Array[] = [];
  onopen: (() => void) | null = null;
  onclose: ((e: { code: number; reason: string }) => void) | null = null;
  onmessage: unknown = null;
  constructor(readonly url: string) {
    sockets.push(this);
  }
  send(d: Uint8Array) {
    this.sent.push(d);
  }
  close(code: number) {
    this.closed = code;
  }
  accept() {
    this.readyState = 1;
    this.onopen?.();
  }
}

let gl = true;
beforeEach(() => {
  terms.length = 0;
  sockets.length = 0;
  gl = true;
  vi.stubGlobal("WebSocket", FakeSocket);
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLCanvasElement.prototype, "getContext").mockImplementation((() => (gl ? { getExtension: () => null } : null)) as never);
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const target = { project: "demo", plural: "containers", name: "web", command: ["sh"] };

function mounted() {
  const el = document.createElement("div");
  const hook = renderHook(() => useTerminal(target));
  hook.result.current.attach(el);
  return hook;
}

async function connected(hook: ReturnType<typeof mounted>) {
  const n = sockets.length;
  await act(() => hook.result.current.connect());
  await waitFor(() => expect(sockets.length).toBe(n + 1));
  return sockets.at(-1)!;
}

describe("useTerminal", () => {
  it("opens nothing until connect, then one exec on the object's path", async () => {
    const hook = mounted();
    expect(sockets).toHaveLength(0);
    const s = await connected(hook);
    expect(s.url).toBe(`ws://${window.location.host}/v1/projects/demo/containers/web:exec`);
    expect(terms).toHaveLength(1);
    expect(terms[0]!.opened).toBe(true);
  });

  it("ends the exec and lets the terminal go when the page leaves", async () => {
    const hook = mounted();
    const s = await connected(hook);
    hook.unmount();
    expect(s.closed).toBe(1000);
    expect(terms[0]!.disposed).toBe(true);
  });

  it("ends the previous exec when it connects again", async () => {
    const hook = mounted();
    const first = await connected(hook);
    const second = await connected(hook);
    expect(first.closed).toBe(1000);
    expect(terms[0]!.disposed).toBe(true);
    expect(second.closed).toBeUndefined();
    expect(terms[1]!.disposed).toBe(false);
  });

  it("opens nothing for a connect overtaken while it waited", async () => {
    const hook = mounted();
    await act(async () => {
      const waiting = hook.result.current.connect();
      hook.result.current.close();
      await waiting;
    });
    expect(sockets).toHaveLength(0);
    expect(terms).toHaveLength(0);
    expect(hook.result.current.state).toEqual({ kind: "idle" });
  });

  it("ends the exec when the page goes into the back/forward cache", async () => {
    const hook = mounted();
    const s = await connected(hook);
    act(() => void window.dispatchEvent(new Event("pagehide")));
    expect(s.closed).toBe(1000);
  });

  it("lets the terminal go on close, and is idle again", async () => {
    const hook = mounted();
    const s = await connected(hook);
    act(() => hook.result.current.close());
    expect(s.closed).toBe(1000);
    expect(terms[0]!.disposed).toBe(true);
    expect(hook.result.current.state).toEqual({ kind: "idle" });
  });

  it("draws nothing and opens no exec where the browser gives no WebGL", async () => {
    gl = false;
    const hook = mounted();
    await act(() => hook.result.current.connect());
    expect(hook.result.current.state).toEqual({ kind: "unsupported" });
    expect(sockets).toHaveLength(0);
    expect(terms).toHaveLength(0);
  });

  it("sends a mouse report's bytes as stdin", async () => {
    const hook = mounted();
    const s = await connected(hook);
    act(() => s.accept());
    terms[0]!.binary!("\x1b[M \xff\x21");
    const last = fromBinary(MachineExecRequestSchema, s.sent.at(-1)!);
    expect(last.message.case).toBe("stdin");
    expect([...(last.message.value as Uint8Array)]).toEqual([0x1b, 0x5b, 0x4d, 0x20, 0xff, 0x21]);
  });
});
