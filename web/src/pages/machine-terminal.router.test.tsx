// @vitest-environment jsdom

// A machine's terminal across the console's real routes, over memory history,
// with and without StrictMode: the card's exec goes on through Full page and
// Back and ends once the machine's pages are left, and a page that settles
// on showing no terminal ends the one it was handed. xterm and the browser's
// WebSocket are stand-ins that record what is done to them.

import { TransportProvider } from "@connectrpc/connect-query";
import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { StrictMode } from "react";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { PrefsProvider } from "@/lib/prefs";
import { routeTree } from "@/router";
import { polyfillDialog } from "@/test/harness";
import { paletteApi, type PaletteFake } from "@/test/palette-api";

// Hoisted with vi.mock, so the terminal's dynamic import gets these whenever it runs.
const { terms, FakeTerm } = vi.hoisted(() => {
  const terms: InstanceType<typeof FakeTerm>[] = [];
  class FakeTerm {
    cols = 80;
    rows = 24;
    options: Record<string, unknown> = {};
    disposed = false;
    constructor() {
      terms.push(this);
    }
    loadAddon() {}
    open() {}
    write() {}
    focus() {}
    dispose() {
      this.disposed = true;
    }
    onData() {}
    onBinary() {}
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
  onopen: (() => void) | null = null;
  onclose: unknown = null;
  onmessage: unknown = null;
  constructor(readonly url: string) {
    sockets.push(this);
  }
  send() {}
  close(code: number) {
    this.closed = code;
  }
  accept() {
    this.readyState = 1;
    this.onopen?.();
  }
}

beforeAll(() => {
  polyfillDialog();
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {} })) as unknown as typeof window.matchMedia;
  HTMLElement.prototype.scrollIntoView ??= () => {};
  window.scrollTo = () => {};
});
beforeEach(() => {
  terms.length = 0;
  sockets.length = 0;
  vi.stubGlobal("WebSocket", FakeSocket);
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLCanvasElement.prototype, "getContext").mockImplementation((() => ({ getExtension: () => null })) as never);
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function app(entry: string, strict: boolean, f: PaletteFake = {}) {
  const api = paletteApi(f);
  const history = createMemoryHistory({ initialEntries: [entry] });
  const router = createRouter({ routeTree, history });
  const tree = (
    <TransportProvider transport={api.transport}>
      <QueryClientProvider client={api.client}>
        <PrefsProvider>
          <RouterProvider router={router} />
        </PrefsProvider>
      </QueryClientProvider>
    </TransportProvider>
  );
  render(strict ? <StrictMode>{tree}</StrictMode> : tree);
  return { api, router, history };
}

const at = async (router: ReturnType<typeof app>["router"], path: string) => waitFor(() => expect(router.state.location.pathname).toBe(path));

describe.each([false, true])("a container's terminal through the routes (StrictMode %s)", (strict) => {
  it("goes on from the card through Full page and Back, and ends once the machine's pages are left", async () => {
    const { router, history } = app("/projects/demo/containers/web", strict);
    fireEvent.click(await screen.findByRole("button", { name: /Connect/ }));
    await waitFor(() => expect(sockets).toHaveLength(1));
    const s = sockets[0]!;
    act(() => s.accept());

    fireEvent.click(await screen.findByRole("link", { name: /Full page/ }));
    await at(router, "/projects/demo/containers/web/terminal");
    await screen.findByText("Connected");
    expect(sockets).toHaveLength(1);
    expect(s.closed).toBeUndefined();

    await act(async () => history.back());
    await at(router, "/projects/demo/containers/web");
    await screen.findByRole("button", { name: /Disconnect/ });
    expect(sockets).toHaveLength(1);
    expect(s.closed).toBeUndefined();
    expect(terms).toHaveLength(1);
    expect(terms[0]!.disposed).toBe(false);

    await act(async () => router.navigate({ to: "/projects/$project/containers", params: { project: "demo" } }));
    await at(router, "/projects/demo/containers");
    await waitFor(() => expect(s.closed).toBe(1000));
    expect(terms[0]!.disposed).toBe(true);
  });
});

describe("a terminal page that settles on showing none", () => {
  it("ends the exec it shows when the caller's roles no longer hold exec", async () => {
    let allowed = true;
    const { api } = app("/projects/demo/containers/web/terminal", false, { perms: () => (allowed ? ["compute.machines.exec"] : []) });
    await waitFor(() => expect(sockets).toHaveLength(1));
    const s = sockets[0]!;
    act(() => s.accept());
    allowed = false;
    await act(async () => api.client.invalidateQueries());
    await screen.findByText(/do not include opening a terminal/);
    await waitFor(() => expect(s.closed).toBe(1000));
    expect(terms[0]!.disposed).toBe(true);
  });
});
