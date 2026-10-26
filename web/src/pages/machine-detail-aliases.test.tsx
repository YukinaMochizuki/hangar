// @vitest-environment jsdom

// A machine's interfaces on its page show the aliases its network's resolver
// answers for it.

import { cleanup, render, screen } from "@testing-library/react";
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

describe("a machine's interfaces", () => {
  it("show their aliases", async () => {
    const api = paletteApi({
      listMachines: () => ({
        items: lists.listMachines().items.map((m) =>
          m.metadata.name === "vm-postgres"
            ? { ...m, spec: { interfaces: [{ name: "eth0", network: "default", ipv4: "10.0.0.5/24", securityGroups: ["default"], aliases: ["db", "pg"] }] } }
            : m,
        ),
      }),
    });
    render(<MachineDetail kind="VirtualMachine" name="postgres" />, { wrapper: withApi(api) });
    await screen.findByRole("heading", { level: 1, name: "postgres" });
    expect(screen.getAllByText("db, pg").length).toBeGreaterThan(0);
  });
});
