// @vitest-environment jsdom

import { MachinePhase } from "@hangar/api/hangar/compute/v1alpha1/machine_pb";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { TerminalState } from "@/components/terminal";
import type { MachineView } from "@/lib/machines";
import { execPermission, sshPermission, TerminalCard, TerminalPage } from "@/pages/machine-terminal";
import { never, withApi } from "@/test/harness";
import { paletteApi } from "@/test/palette-api";

// xterm draws on a canvas jsdom does not have; the tests check what the
// pages decide, so the terminal is a stand-in that records its connects.
const terminal = { connects: 0, state: { kind: "idle" } as TerminalState, target: undefined as unknown };
vi.mock("@/components/terminal", () => ({
  useTerminal: (target: unknown) => ({
    attach: () => {},
    state: terminal.state,
    plain: false,
    connect: async () => {
      terminal.connects++;
      terminal.target = target;
    },
    disconnect: () => {},
  }),
}));

vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  Link: (await import("@/test/fake-link")).FakeLink,
}));

beforeEach(() => {
  terminal.connects = 0;
  terminal.state = { kind: "idle" };
  terminal.target = undefined;
});
afterEach(cleanup);

const machine = (over: Partial<MachineView> = {}) =>
  ({ kind: "Container", name: "web", project: "demo", phase: MachinePhase.RUNNING, deleting: false, uid: "u1", ...over }) as MachineView;

describe("the Terminal card", () => {
  it("is its header alone until Connect, and connects only then", () => {
    render(<TerminalCard m={machine()} />, { wrapper: withApi(paletteApi()) });
    expect(screen.getByText("Terminal")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Disconnect/ })).toBeNull();
    expect(terminal.connects).toBe(0);
    fireEvent.click(screen.getByRole("button", { name: /Connect/ }));
    expect(terminal.connects).toBe(1);
    expect(screen.getByRole("link", { name: /Full page/ }).getAttribute("href")).toBe("/projects/demo/containers/web/terminal");
  });

  it("says why it cannot connect to a container that is not running, with Connect off", () => {
    render(<TerminalCard m={machine({ phase: MachinePhase.STOPPED })} />, { wrapper: withApi(paletteApi()) });
    expect(screen.getByText("web is not running.")).toBeTruthy();
    expect((screen.getByRole("button", { name: /Connect/ }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("says a container being deleted takes no terminal", () => {
    render(<TerminalCard m={machine({ deleting: true })} />, { wrapper: withApi(paletteApi()) });
    expect(screen.getByText("web is being deleted.")).toBeTruthy();
  });
});

describe("the terminal page", () => {
  it("connects on opening a running container's terminal", async () => {
    render(<TerminalPage kind="Container" name="web" />, { wrapper: withApi(paletteApi()) });
    await waitFor(() => expect(terminal.connects).toBe(1));
    expect(screen.getByText(/hangar exec -it container web -p demo -- sh/)).toBeTruthy();
  });

  it("does not connect to a stopped container, and says it is not running", async () => {
    render(<TerminalPage kind="Container" name="api-web" />, { wrapper: withApi(paletteApi()) });
    await screen.findByText("api-web is not running.");
    expect(terminal.connects).toBe(0);
    expect(screen.queryByRole("button", { name: /Connect/ })).toBeNull();
  });

  it("states that the caller's roles hold no exec, once the answer says so", async () => {
    render(<TerminalPage kind="Container" name="web" />, { wrapper: withApi(paletteApi({ perms: () => [] })) });
    await screen.findByText(/do not include opening a terminal/);
    expect(terminal.connects).toBe(0);
  });

  it("shows nothing and connects nowhere while the permission is not known", async () => {
    const api = paletteApi({ perms: () => never() });
    render(<TerminalPage kind="Container" name="web" />, { wrapper: withApi(api) });
    await waitFor(() => expect(api.calls).toContain("ListMachines"));
    expect(screen.queryByText(/do not include/)).toBeNull();
    expect(terminal.connects).toBe(0);
  });

  it("asks for the exec permission at the project", async () => {
    const api = paletteApi();
    render(<TerminalPage kind="Container" name="web" />, { wrapper: withApi(api) });
    await waitFor(() => expect(api.calls).toContain("TestPermissions"));
    const asked = api.requests.find((r) => r.method === "TestPermissions")?.request as { permissions: string[] };
    expect(asked.permissions).toEqual([execPermission]);
  });
});

describe("a virtual machine's terminal", () => {
  it("logs in with no command, on the VM's exec, and says it logs in over SSH as the image's default user", async () => {
    render(<TerminalPage kind="VirtualMachine" name="postgres" />, { wrapper: withApi(paletteApi()) });
    await waitFor(() => expect(terminal.connects).toBe(1));
    expect(terminal.target).toEqual({ project: "demo", plural: "virtualmachines", name: "postgres", command: [] });
    expect(screen.getByText(/Logs in as the image's default user/)).toBeTruthy();
    expect(screen.getByText(/hangar ssh vm postgres -p demo/)).toBeTruthy();
  });

  it("calls what it runs a login shell, once connected", async () => {
    terminal.state = { kind: "open", cols: 120, rows: 40 };
    render(<TerminalPage kind="VirtualMachine" name="postgres" />, { wrapper: withApi(paletteApi()) });
    expect(await screen.findByText("login shell · 120×40")).toBeTruthy();
  });

  it("asks for the ssh permission, not exec's", async () => {
    const api = paletteApi();
    render(<TerminalPage kind="VirtualMachine" name="postgres" />, { wrapper: withApi(api) });
    await waitFor(() => expect(api.calls).toContain("TestPermissions"));
    const asked = api.requests.find((r) => r.method === "TestPermissions")?.request as { permissions: string[] };
    expect(asked.permissions).toEqual([sshPermission]);
  });

  it("is refused to a caller who holds exec but not ssh", async () => {
    render(<TerminalPage kind="VirtualMachine" name="postgres" />, { wrapper: withApi(paletteApi({ perms: () => [execPermission] })) });
    await screen.findByText(/do not include opening a terminal in this project's virtual machines/);
    expect(terminal.connects).toBe(0);
  });

  it("links the card's Full page to the VM's terminal page", () => {
    render(<TerminalCard m={machine({ kind: "VirtualMachine", name: "postgres" })} />, { wrapper: withApi(paletteApi()) });
    fireEvent.click(screen.getByRole("button", { name: /Connect/ }));
    expect(screen.getByRole("link", { name: /Full page/ }).getAttribute("href")).toBe("/projects/demo/vms/postgres/terminal");
  });
});
