// @vitest-environment jsdom

// A start waiting on its node for room: the page's head says so, a
// banner gives the API's message, and Stop and the run state stay as they
// are for a machine asked to run. A condition from an older generation
// says nothing of the spec as it is, so it is not shown as waiting.

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { MachineDetail } from "@/pages/machine-detail";
import { withApi } from "@/test/harness";
import { lists, paletteApi, roomMessage, waiting } from "@/test/palette-api";

vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  useNavigate: () => vi.fn(),
  Link: (await import("@/test/fake-link")).FakeLink,
}));

beforeAll(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {} })) as unknown as typeof window.matchMedia;
});
afterEach(cleanup);

/** The API with one machine of the kind waiting for room, its condition computed from generation observed. */
function apiWith(kind: "Container" | "VirtualMachine", name: string, observed?: bigint) {
  const w = waiting(kind, name, observed);
  const machines = { listMachines: () => ({ items: [...lists.listMachines().items, w.machine] }) };
  return kind === "Container"
    ? paletteApi({ ...machines, listContainers: () => ({ items: [...lists.listContainers().items, w.container] }) })
    : paletteApi({ ...machines, listVirtualMachines: () => ({ items: [...lists.listVirtualMachines().items, w.vm] }) });
}

/** The page's head: its name, state and actions. */
async function head(name: string) {
  const h1 = await screen.findByRole("heading", { level: 1, name });
  return h1.closest(".border-b") as HTMLElement;
}

describe("a machine whose start waits for room", () => {
  it.each([
    ["Container", "build"],
    ["VirtualMachine", "builder-vm"],
  ] as const)("a %s's page says it waits, with the API's message, and keeps Stop and its run state", async (kind, name) => {
    render(<MachineDetail kind={kind} name={name} />, { wrapper: withApi(apiWith(kind, name)) });
    const top = await head(name);
    expect(within(top).getByText("Waiting for room")).toBeTruthy();
    expect(within(top).queryByText("Stopped")).toBeNull();
    expect(within(top).getByRole("button", { name: "Stop" })).toBeTruthy();
    expect(within(top).queryByRole("button", { name: "Start" })).toBeNull();

    const banner = screen.getByText(`${name} is waiting for room on its node`).closest("[role=status]") as HTMLElement;
    expect(within(banner).getByText(roomMessage)).toBeTruthy();
    expect(within(banner).getByText("Stop withdraws the start.")).toBeTruthy();

    const runState = screen.getByText("Run state").closest("div") as HTMLElement;
    expect(within(runState).getByText("Running")).toBeTruthy();
  });

  it("is not shown waiting when the condition is from an older generation", async () => {
    render(<MachineDetail kind="Container" name="build" />, { wrapper: withApi(apiWith("Container", "build", 1n)) });
    const top = await head("build");
    expect(within(top).getByText("Stopped")).toBeTruthy();
    expect(screen.queryByText("Waiting for room")).toBeNull();
    expect(screen.queryByText("build is waiting for room on its node")).toBeNull();
  });
});
