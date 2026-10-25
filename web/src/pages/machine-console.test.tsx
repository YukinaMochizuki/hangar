// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor, act } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Opener, SessionEvents, TerminalState } from "@/components/terminal";
import type { ConsoleEvents, ConsoleTarget } from "@/lib/console";
import type { MachineView } from "@/lib/machines";
import { consoleBlock, ConsolePage } from "@/pages/machine-console";
import { execPermission, sshPermission } from "@/pages/machine-terminal";
import { never, withApi } from "@/test/harness";
import { paletteApi } from "@/test/palette-api";

// xterm draws on a canvas jsdom does not have: the terminal is a stand-in
// that opens its session on a fake terminal at connect, as useXterm does,
// and the console is one that records how it was attached.
const fakeTerm = { cols: 80, rows: 24, options: {} as Record<string, unknown>, resize: vi.fn() };
const terminal = { connects: 0, state: { kind: "idle" } as TerminalState, events: undefined as SessionEvents | undefined, resized: [] as [number, number][] };
vi.mock("@/components/terminal", async () => {
  const { useState } = await import("react");
  return {
    useXterm: (open: Opener) => {
      const [state, setState] = useState<TerminalState>(terminal.state);
      return {
        attach: () => {},
        state,
        connect: async () => {
          terminal.connects++;
          open(fakeTerm as never, {
            onOpen: () => setState({ kind: "open", cols: 80, rows: 24 }),
            onOutput: () => {},
            onEnd: (end) => setState({ kind: "ended", end }),
          });
        },
        disconnect: () => {},
        close: () => {},
        fits: () => ({ cols: 150, rows: 40 }),
        resize: (c: number, r: number) => terminal.resized.push([c, r]),
      };
    },
  };
});
const consoles: { target: ConsoleTarget; events: ConsoleEvents }[] = [];
vi.mock("@/lib/console", async (orig) => ({
  ...(await orig<typeof import("@/lib/console")>()),
  openConsole: (target: ConsoleTarget, events: ConsoleEvents) => {
    consoles.push({ target, events });
    return { send: () => {}, resize: () => {}, close: () => {} };
  },
}));

vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  Link: (await import("@/test/fake-link")).FakeLink,
}));

beforeEach(() => {
  terminal.connects = 0;
  terminal.state = { kind: "idle" };
  terminal.resized = [];
  consoles.length = 0;
  fakeTerm.options = {};
  fakeTerm.resize.mockClear();
});
afterEach(cleanup);

/** The server's side of the last attach: it opens and sends the client's state. */
function attached(input: boolean, cols = 80, rows = 24) {
  const c = consoles.at(-1)!;
  act(() => {
    c.events.onOpen?.();
    c.events.onState({ input, size: { cols, rows } });
  });
}

describe("the serial console page", () => {
  it("attaches to the VM's console on opening, taking the input only if no one holds it", async () => {
    render(<ConsolePage name="postgres" />, { wrapper: withApi(paletteApi()) });
    await waitFor(() => expect(consoles).toHaveLength(1));
    expect(consoles[0].target).toEqual({ project: "demo", plural: "virtualmachines", name: "postgres", force: false });
    expect(screen.getByText("The serial console is slower than SSH.")).toBeTruthy();
    expect(screen.getByText(/hangar console vm postgres -p demo/)).toBeTruthy();
  });

  it("draws the terminal at the console's size, and lets the one typing set it for everyone", async () => {
    render(<ConsolePage name="postgres" />, { wrapper: withApi(paletteApi()) });
    await waitFor(() => expect(consoles).toHaveLength(1));
    attached(true, 100, 30);
    expect(fakeTerm.resize).toHaveBeenCalledWith(100, 30);
    expect(fakeTerm.options.disableStdin).toBe(false);
    expect(screen.getByText("Typing")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /100×30/ }));
    fireEvent.click(screen.getByRole("menuitemradio", { name: /160×50/ }));
    fireEvent.click(screen.getByRole("button", { name: /100×30/ }));
    fireEvent.click(screen.getByRole("menuitemradio", { name: /Fit window/ }));
    expect(terminal.resized).toEqual([
      [160, 50],
      [150, 40],
    ]);
  });

  it("only watches while someone else types, and Take input attaches again with force", async () => {
    render(<ConsolePage name="postgres" />, { wrapper: withApi(paletteApi()) });
    await waitFor(() => expect(consoles).toHaveLength(1));
    attached(false);
    expect(screen.getByText(/someone else is typing/)).toBeTruthy();
    expect(fakeTerm.options.disableStdin).toBe(true);
    expect(screen.queryByRole("button", { name: /80×24/ })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: /Take input/ }));
    await waitFor(() => expect(consoles).toHaveLength(2));
    expect(consoles[1].target.force).toBe(true);
    // Force is for that one attach: Reconnect later asks as before.
    attached(true);
    act(() => consoles[1].events.onEnd({ kind: "lost" }));
    fireEvent.click(screen.getByRole("button", { name: /Reconnect/ }));
    await waitFor(() => expect(consoles).toHaveLength(3));
    expect(consoles[2].target.force).toBe(false);
  });

  it("says a refused attach is the CLI's to explain", async () => {
    render(<ConsolePage name="postgres" />, { wrapper: withApi(paletteApi()) });
    await waitFor(() => expect(consoles).toHaveLength(1));
    act(() => consoles[0].events.onEnd({ kind: "unreachable" }));
    expect(screen.getByText(/The console could not connect/)).toBeTruthy();
  });

  it("asks what the VM's terminal asks, and needs exec, not ssh", async () => {
    const api = paletteApi({ perms: () => [sshPermission] });
    render(<ConsolePage name="postgres" />, { wrapper: withApi(api) });
    await screen.findByText(/do not include attaching/);
    const asked = api.requests.find((r) => r.method === "TestPermissions")?.request as { permissions: string[] };
    expect(asked.permissions).toEqual([execPermission, sshPermission]);
    expect(terminal.connects).toBe(0);
  });

  it("shows nothing and attaches nowhere while the permission is not known", async () => {
    const api = paletteApi({ perms: () => never() });
    render(<ConsolePage name="postgres" />, { wrapper: withApi(api) });
    await waitFor(() => expect(api.calls).toContain("ListMachines"));
    expect(screen.queryByText(/do not include/)).toBeNull();
    expect(terminal.connects).toBe(0);
  });

  it("says there is no such VM", async () => {
    render(<ConsolePage name="nope" />, { wrapper: withApi(paletteApi()) });
    await screen.findByText("No such virtual machine");
  });
});

describe("consoleBlock", () => {
  const m = (over: Partial<MachineView>) => ({ name: "db", deleting: false, stopped: false, ...over }) as MachineView;
  it("takes a VM that is booting or running, and refuses one stopped or being deleted", () => {
    expect(consoleBlock(m({}))).toBeUndefined();
    expect(consoleBlock(m({ stopped: true }))).toBe("db is stopped.");
    expect(consoleBlock(m({ deleting: true }))).toBe("db is being deleted.");
  });
});
