// @vitest-environment jsdom

// Where a container's page offers its terminal: the header's Terminal link
// and the Terminal card, only for a container and only once TestPermissions
// holds compute.machines.exec; and a failed poll of the lists keeps the page,
// and so a terminal open on it, on the last answer. A VM's serial console
// is offered only in the head's "..." menu, on compute.machines.exec.

import { Code, ConnectError } from "@connectrpc/connect";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { MachineDetail } from "@/pages/machine-detail";
import { never, withApi, type Api } from "@/test/harness";
import { lists, paletteApi, type PaletteFake } from "@/test/palette-api";

vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  useNavigate: () => vi.fn(),
  Link: (await import("@/test/fake-link")).FakeLink,
}));

beforeAll(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {} })) as unknown as typeof window.matchMedia;
});
afterEach(cleanup);

const terminalLink = () => screen.queryByRole("link", { name: /^Terminal$/ });
const card = () => screen.queryByRole("button", { name: /^Connect$/ });

async function show(kind: "Container" | "VirtualMachine", name: string, f: PaletteFake = {}): Promise<Api> {
  const api = paletteApi(f);
  render(<MachineDetail kind={kind} name={name} />, { wrapper: withApi(api) });
  await screen.findByRole("heading", { level: 1, name });
  await waitFor(() => expect(api.calls).toContain("TestPermissions"));
  return api;
}

describe("a container's terminal on its page", () => {
  it("is offered, by the header's link and the card, when the exec permission is held", async () => {
    await show("Container", "web");
    await waitFor(() => expect(terminalLink()?.getAttribute("href")).toBe("/projects/demo/containers/web/terminal"));
    expect(card()).toBeTruthy();
  });

  it("is not offered when the answer says the permission is not held", async () => {
    const api = await show("Container", "web", { perms: () => [] });
    await waitFor(() => expect(api.client.isFetching()).toBe(0));
    expect(terminalLink()).toBeNull();
    expect(card()).toBeNull();
  });

  it("is not offered while the answer has not come", async () => {
    await show("Container", "web", { perms: () => never() });
    expect(terminalLink()).toBeNull();
    expect(card()).toBeNull();
  });

  it("is offered on a virtual machine's page to a caller who holds ssh, linking the VM's terminal page", async () => {
    await show("VirtualMachine", "postgres", { perms: () => ["compute.machines.ssh"] });
    await waitFor(() => expect(terminalLink()?.getAttribute("href")).toBe("/projects/demo/vms/postgres/terminal"));
    expect(card()).toBeTruthy();
  });

  it("is not offered on a virtual machine's page to a caller who holds exec alone", async () => {
    const api = await show("VirtualMachine", "postgres", { perms: () => ["compute.machines.exec"] });
    await waitFor(() => expect(api.client.isFetching()).toBe(0));
    expect(terminalLink()).toBeNull();
    expect(card()).toBeNull();
  });

  it("is not offered on a container's page to a caller who holds ssh alone", async () => {
    const api = await show("Container", "web", { perms: () => ["compute.machines.ssh"] });
    await waitFor(() => expect(api.client.isFetching()).toBe(0));
    expect(terminalLink()).toBeNull();
    expect(card()).toBeNull();
  });

  it("stays on the last answer when asking the lists again fails, saying so", async () => {
    let calls = 0;
    const api = await show("Container", "web", {
      listMachines: () => {
        calls++;
        if (calls > 1) throw new ConnectError("the apiserver is unavailable", Code.FailedPrecondition);
        return lists.listMachines();
      },
    });
    await waitFor(() => expect(card()).toBeTruthy());
    await api.client.refetchQueries();
    expect(await screen.findByText(/did not reload; showing the last answer/)).toBeTruthy();
    expect(card()).toBeTruthy();
    expect(screen.queryByText("The container did not load")).toBeNull();
  });
});

describe("a virtual machine's serial console on its page", () => {
  const more = () => screen.queryByRole("button", { name: "More actions" });
  const serialConsole = () => screen.queryByRole("menuitem", { name: /Serial console/ });

  it("is offered in the head's menu alone, to a caller who holds exec", async () => {
    await show("VirtualMachine", "postgres", { perms: () => ["compute.machines.exec"] });
    await waitFor(() => expect(more()).toBeTruthy());
    expect(screen.queryByRole("link", { name: /Serial console/ })).toBeNull();
    fireEvent.click(more()!);
    expect((serialConsole() as HTMLButtonElement).disabled).toBe(false);
  });

  it("is not offered to a caller who holds ssh alone, nor on a container's page", async () => {
    const api = await show("VirtualMachine", "postgres", { perms: () => ["compute.machines.ssh"] });
    await waitFor(() => expect(api.client.isFetching()).toBe(0));
    expect(more()).toBeNull();
    cleanup();
    const ct = await show("Container", "web", { perms: () => ["compute.machines.exec"] });
    await waitFor(() => expect(ct.client.isFetching()).toBe(0));
    expect(more()).toBeNull();
  });
});
