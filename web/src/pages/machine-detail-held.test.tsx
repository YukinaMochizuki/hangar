// @vitest-environment jsdom

// A container held back before it is scheduled, such as by a file share on
// no node yet, and a machine on no node: what the page says of them.

import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { MachineDetail } from "@/pages/machine-detail";
import { withApi } from "@/test/harness";
import { lists, paletteApi } from "@/test/palette-api";

vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  useNavigate: () => vi.fn(),
  Link: (await import("@/test/fake-link")).FakeLink,
}));

beforeAll(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {} })) as unknown as typeof window.matchMedia;
});
afterEach(cleanup);

const held = {
  metadata: { name: "notebook", project: "demo" },
  spec: { image: "jupyter/base-notebook", volumes: [{ source: "scratch", sourceKind: "FileShare", target: "/work" }] },
  status: {
    conditions: [
      { type: "Ready", status: ConditionStatus.FALSE, reason: "MachineHeldBack", message: 'failed_precondition: file share "scratch" is on no node yet' },
    ],
  },
};

describe("a held back container's page", () => {
  it("shows Scheduled not reached and held back, the reason once, in Runs on, and no logs as a fact, asking for none", async () => {
    const api = paletteApi({ listContainers: () => ({ items: [...lists.listContainers().items, held] }) });
    render(<MachineDetail kind="Container" name="notebook" />, { wrapper: withApi(api) });
    await screen.findByRole("heading", { level: 1, name: "notebook" });
    // The taxiway's note and the Runs on header both say it; the reason stands once.
    await waitFor(() => expect(screen.getAllByText("Held back")).toHaveLength(2));
    expect(screen.getAllByText(/file share "scratch" is on no node yet/)).toHaveLength(1);
    expect(screen.queryByText("no host yet")).toBeNull();
    expect(screen.getByText("No logs: notebook has not started.")).toBeTruthy();
    expect(screen.queryByText("The log did not load")).toBeNull();
    await waitFor(() => expect(api.calls).toContain("ListMachines"));
    expect(api.calls).not.toContain("Logs");
  });

  it("streams the log of a machine on a node", async () => {
    const api = paletteApi({
      listMachines: () => ({
        items: lists.listMachines().items.map((m) => (m.metadata.name === "c-web" ? { ...m, spec: { node: "node-a" } } : m)),
      }),
    });
    render(<MachineDetail kind="Container" name="web" />, { wrapper: withApi(api) });
    expect(await screen.findByText("listening on :8080")).toBeTruthy();
    expect(api.calls).toContain("Logs");
    expect(screen.queryByText(/No logs:/)).toBeNull();
  });

  it("states no logs for any machine on no node, rather than an error", async () => {
    // web has a Machine, placed on no node.
    const api = paletteApi();
    render(<MachineDetail kind="Container" name="web" />, { wrapper: withApi(api) });
    await screen.findByRole("heading", { level: 1, name: "web" });
    expect(await screen.findByText("No logs: web has not started.")).toBeTruthy();
    expect(api.calls).not.toContain("Logs");
  });
});
