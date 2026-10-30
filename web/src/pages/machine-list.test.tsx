// @vitest-environment jsdom

// A start waiting on its node for room, in a list: its state says so rather
// than Stopped, and it is under Running, as a machine asked to run. A
// condition from an older generation is not shown as waiting.

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { MachineList } from "@/pages/machine-list";
import { withApi } from "@/test/harness";
import { lists, paletteApi, waiting } from "@/test/palette-api";

vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  useNavigate: () => vi.fn(),
  Link: (await import("@/test/fake-link")).FakeLink,
}));

afterEach(cleanup);

/** The API with a machine of the kind waiting for room, and one whose condition is from generation 1. */
function apiWith(kind: "Container" | "VirtualMachine") {
  const now = waiting(kind, "build");
  const stale = waiting(kind, "old", 1n);
  const machines = { listMachines: () => ({ items: [...lists.listMachines().items, now.machine, stale.machine] }) };
  return kind === "Container"
    ? paletteApi({ ...machines, listContainers: () => ({ items: [...lists.listContainers().items, now.container, stale.container] }) })
    : paletteApi({ ...machines, listVirtualMachines: () => ({ items: [...lists.listVirtualMachines().items, now.vm, stale.vm] }) });
}

/** The state cell of the table's row of name. */
function state(name: string) {
  const row = within(screen.getByRole("table")).getByText(name).closest("tr") as HTMLElement;
  return row.querySelectorAll("td")[1].textContent;
}

/** The names in the table's rows. */
function names() {
  return within(screen.getByRole("table"))
    .getAllByRole("row")
    .slice(1)
    .map((r) => r.querySelector("td .font-medium")?.textContent);
}

describe("a list's machine whose start waits for room", () => {
  it.each(["Container", "VirtualMachine"] as const)("reads Waiting for room for a %s, and is under Running, not Stopped", async (kind) => {
    render(<MachineList kind={kind} />, { wrapper: withApi(apiWith(kind)) });
    await within(await screen.findByRole("table")).findByText("build");
    expect(state("build")).toBe("Waiting for room");
    expect(state("old")).toBe("Stopped");

    fireEvent.click(screen.getByRole("radio", { name: /^Running/ }));
    expect(names()).toContain("build");
    expect(names()).not.toContain("old");
    fireEvent.click(screen.getByRole("radio", { name: /^Stopped/ }));
    expect(names()).not.toContain("build");
    expect(names()).toContain("old");
  });
});
