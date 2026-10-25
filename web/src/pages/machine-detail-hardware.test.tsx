// @vitest-environment jsdom

// A VM's serial number and TPM on its page: stated when it has them, and
// left out of the summary when it has the platform's serial and no TPM.

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

describe("a VM's page", () => {
  it("states its serial number and TPM", async () => {
    const api = paletteApi({
      listVirtualMachines: () => ({
        items: lists.listVirtualMachines().items.map((v) => ({ ...v, spec: { platform: { serialNumber: "ARX-0042" }, tpm: { enabled: true } } })),
      }),
    });
    render(<MachineDetail kind="VirtualMachine" name="postgres" />, { wrapper: withApi(api) });
    await screen.findByRole("heading", { level: 1, name: "postgres" });
    expect(screen.getByText("Serial number")).toBeTruthy();
    expect(screen.getByText("ARX-0042")).toBeTruthy();
    expect(screen.getByText("TPM 2.0")).toBeTruthy();
  });

  it("leaves them out when it has neither", async () => {
    render(<MachineDetail kind="VirtualMachine" name="postgres" />, { wrapper: withApi(paletteApi()) });
    await screen.findByRole("heading", { level: 1, name: "postgres" });
    expect(screen.getByText("SSH keys")).toBeTruthy();
    expect(screen.queryByText("Serial number")).toBeNull();
    expect(screen.queryByText("TPM")).toBeNull();
  });
});
