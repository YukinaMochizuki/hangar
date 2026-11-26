// @vitest-environment jsdom

// A Shared machine on its page and in its list: the Shared mark, its Memory
// row with what its pool leaves it now, the Usage card's line for it, and
// what its node says: a kill for memory within the day, and a balloon that
// does not follow.

import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { MachineMemoryKind } from "@hangar/api/hangar/compute/v1alpha1/machine_capability_pb";
import { MachinePhase } from "@hangar/api/hangar/compute/v1alpha1/machine_pb";
import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { MachineDetail } from "@/pages/machine-detail";
import { MachineList } from "@/pages/machine-list";
import { withApi } from "@/test/harness";
import { paletteApi } from "@/test/palette-api";

vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  useNavigate: () => vi.fn(),
  Link: (await import("@/test/fake-link")).FakeLink,
}));

beforeAll(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {} })) as unknown as typeof window.matchMedia;
});
afterEach(cleanup);

const GiB = 1024n ** 3n;
const project = "demo";
const minutesAgo = (m: number) => new Date(Date.now() - m * 60_000);

/** A running container of 4 GiB reserving `reserved`, its Machine in acme's pool with conditions. */
function machines(reserved: string, conditions: object[] = []) {
  return {
    listContainers: () => ({ items: [{ metadata: { name: "web", project }, spec: { image: "nginx", resources: { cpu: "2", memory: "4Gi", memoryReserved: reserved } } }] }),
    listMachines: () => ({
      items: [
        {
          metadata: { name: "c-web", project, ownerReferences: [{ kind: "Container", name: "web", controller: true }] },
          spec: { node: "n1", resources: { cpu: "2", memory: "4Gi" }, memoryPool: { name: "acme", size: "8192Mi" } },
          status: { phase: MachinePhase.RUNNING, conditions },
        },
      ],
    }),
    listTenants: () => ({ items: [{ metadata: { name: "acme" }, spec: { displayName: "Acme" } }] }),
  };
}

/** Its node's answer: 4 GiB, of which its balloon holds `balloon` GiB, unset when the node cannot tell. */
const usage = (balloon?: number) => () => ({
  vcpus: 2,
  memoryLimitBytes: 4n * GiB,
  memoryKind: MachineMemoryKind.GUEST_USED,
  points: [{ time: timestampFromDate(minutesAgo(2)), cpu: 0.2, memoryBytes: 2n * GiB }],
  latest: { time: timestampFromDate(minutesAgo(0)), cpu: 0.2, memoryBytes: 2n * GiB },
  ...(balloon === undefined ? {} : { balloonBytes: BigInt(Math.round(balloon * 1024)) * 1024n * 1024n }),
});

const oom = (at: Date) => ({
  type: "OOMKilled",
  status: ConditionStatus.TRUE,
  reason: "OutOfMemory",
  message: `the host killed the machine's VM whole at ${at.toISOString().replace(/\.\d+Z$/, "Z")}, its tenant's machines on the node out of the memory they share`,
});
const stuck = { type: "BalloonResponsive", status: ConditionStatus.FALSE, reason: "BalloonUnresponsive", message: "…", lastTransitionTime: timestampFromDate(minutesAgo(12)) };

const usageCard = () => screen.getByRole("heading", { name: "Usage" }).parentElement!.parentElement!;

describe("a Shared machine's page", () => {
  it("marks it Shared, and says its limit, what its pool leaves it now and what it keeps reserved", async () => {
    render(<MachineDetail kind="Container" name="web" />, { wrapper: withApi(paletteApi({ ...machines("2Gi"), usage: usage(1.4) })) });
    expect(await screen.findByText("Shared · 4 GiB limit · 2.6 GiB available now · 2 GiB reserved")).toBeTruthy();
    expect(screen.getAllByText("Shared").length).toBeGreaterThan(0);
    await waitFor(() => expect(within(usageCard()).getByText("available now, 2.6 GiB")).toBeTruthy());
    expect(within(usageCard()).getByText("of 2.6 GiB", { exact: false })).toBeTruthy();
    expect(screen.queryByText(/stopped for memory/i)).toBeNull();
  });

  it("leaves out what is available now when its node cannot tell", async () => {
    render(<MachineDetail kind="Container" name="web" />, { wrapper: withApi(paletteApi({ ...machines("2Gi"), usage: usage() })) });
    expect(await screen.findByText("Shared · 4 GiB limit · 2 GiB reserved")).toBeTruthy();
    await waitFor(() => expect(within(usageCard()).getByText("20%")).toBeTruthy());
    expect(screen.queryByText(/available now/)).toBeNull();
  });

  it("marks nothing on a machine with all of its memory reserved", async () => {
    render(<MachineDetail kind="Container" name="web" />, { wrapper: withApi(paletteApi({ ...machines(""), usage: usage(0) })) });
    await screen.findByText("Summary");
    await waitFor(() => expect(within(usageCard()).queryByText("Loading…")).toBeNull());
    expect(screen.queryByText("Shared")).toBeNull();
    expect(screen.queryByText(/available now/)).toBeNull();
  });

  it("says its VM was stopped for memory within the day, and not once a day has passed", async () => {
    render(<MachineDetail kind="Container" name="web" />, { wrapper: withApi(paletteApi({ ...machines("2Gi", [oom(minutesAgo(23))]), usage: usage(1) })) });
    expect(await screen.findByText("web was stopped for memory 23m ago")).toBeTruthy();
    expect(screen.getByText(/Acme's running machines needed more memory than the plan's 8 GiB, and a Shared machine is stopped first\. It started again\./)).toBeTruthy();
    expect(screen.getByText("Stopped for memory 23m ago")).toBeTruthy();
    cleanup();
    render(<MachineDetail kind="Container" name="web" />, { wrapper: withApi(paletteApi({ ...machines("2Gi", [oom(minutesAgo(25 * 60))]), usage: usage(1) })) });
    await screen.findByText(/Shared · 4 GiB limit/);
    expect(screen.queryByText(/stopped for memory/i)).toBeNull();
  });

  it("says its balloon does not follow, and shows nothing available now", async () => {
    render(<MachineDetail kind="Container" name="web" />, { wrapper: withApi(paletteApi({ ...machines("2Gi", [stuck]), usage: usage(1.4) })) });
    expect(await screen.findByText("web is not giving memory back")).toBeTruthy();
    expect(screen.getByText(/Its guest stopped answering requests for memory, so the 4 GiB it may hold cannot go to Acme's other machines\. Restarting it asks again\./)).toBeTruthy();
    await waitFor(() => expect(within(usageCard()).queryByText("Loading…")).toBeNull());
    expect(screen.queryByText(/available now/)).toBeNull();
  });
});

describe("a Shared machine in its list", () => {
  const row = () => screen.getByRole("table").querySelector("tbody tr")!;

  it("marks it Shared beside its size, and says under its state what its node said", async () => {
    render(<MachineList kind="Container" />, { wrapper: withApi(paletteApi(machines("2Gi", [oom(minutesAgo(23))]))) });
    await waitFor(() => expect(within(row() as HTMLElement).getByText("Shared")).toBeTruthy());
    expect(within(row() as HTMLElement).getByText("Stopped for memory 23m ago")).toBeTruthy();
    cleanup();
    render(<MachineList kind="Container" />, { wrapper: withApi(paletteApi(machines("2Gi", [stuck]))) });
    await waitFor(() => expect(within(row() as HTMLElement).getByText("Not giving memory back")).toBeTruthy());
    cleanup();
    render(<MachineList kind="Container" />, { wrapper: withApi(paletteApi(machines(""))) });
    await waitFor(() => expect(within(row() as HTMLElement).getByText("web")).toBeTruthy());
    expect(within(row() as HTMLElement).queryByText("Shared")).toBeNull();
  });
});
