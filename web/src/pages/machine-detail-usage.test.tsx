// @vitest-environment jsdom

// A machine's Usage card: its CPU and memory now, under the memory's own
// name for each kind; one line for a machine not running, which its node is
// not asked about; and the API's word that it stopped taken as the same.

import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { MachineMemoryKind } from "@hangar/api/hangar/compute/v1alpha1/machine_capability_pb";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { MachineDetail } from "@/pages/machine-detail";
import { never, withApi } from "@/test/harness";
import { paletteApi } from "@/test/palette-api";
import { toUsage } from "@/lib/usage";
import { plotPaths } from "@/components/series-chart";

vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  useNavigate: () => vi.fn(),
  Link: (await import("@/test/fake-link")).FakeLink,
}));

beforeAll(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {} })) as unknown as typeof window.matchMedia;
});
afterEach(cleanup);

/** The Usage card, by its heading, as it is now: it is drawn anew when its answer comes. */
function usageCard(): HTMLElement {
  return screen.getByRole("heading", { name: "Usage" }).parentElement!.parentElement!;
}

const GiB = 1024n ** 3n;
const at = (minAgo: number) => timestampFromDate(new Date(Date.now() - minAgo * 60_000));
const answer = (memoryKind: MachineMemoryKind) => ({
  vcpus: 2,
  memoryLimitBytes: 2n * GiB,
  memoryKind,
  points: [
    { time: at(3), cpu: 0.1, memoryBytes: GiB },
    { time: at(2), cpu: 0.2, memoryBytes: GiB },
  ],
  latest: { time: at(0), cpu: 0.37, memoryBytes: (3n * GiB) / 2n },
});

describe("a machine's Usage", () => {
  it("shows a container's CPU and the memory its guest uses, now", async () => {
    const api = paletteApi({ usage: () => answer(MachineMemoryKind.GUEST_USED) });
    render(<MachineDetail kind="Container" name="web" />, { wrapper: withApi(api) });
    expect(await screen.findByText("37%")).toBeTruthy();
    expect(screen.getByText("Memory used")).toBeTruthy();
    expect(screen.getByText("1.5 GiB")).toBeTruthy();
    expect(api.requests.find((c) => c.method === "Usage")?.request).toMatchObject({ project: "demo", kind: "Container", name: "web" });
  });

  it("names a VM's memory as what it holds on the host", async () => {
    render(<MachineDetail kind="VirtualMachine" name="postgres" />, { wrapper: withApi(paletteApi({ usage: () => answer(MachineMemoryKind.HOST_HELD) })) });
    expect(await screen.findByText("Host memory")).toBeTruthy();
    expect(screen.queryByText("Memory used")).toBeNull();
  });

  it("says a stopped machine is not running, and does not ask its node", async () => {
    const api = paletteApi({ usage: () => answer(MachineMemoryKind.GUEST_USED) });
    render(<MachineDetail kind="Container" name="api-web" />, { wrapper: withApi(api) });
    await waitFor(() => expect(within(usageCard()).getByText("api-web is not running.")).toBeTruthy());
    expect(api.calls).not.toContain("Usage");
  });

  it("takes FailedPrecondition, a machine stopped since its page loaded, as not running", async () => {
    const api = paletteApi({
      usage: () => {
        throw new ConnectError("not running", Code.FailedPrecondition);
      },
    });
    render(<MachineDetail kind="Container" name="web" />, { wrapper: withApi(api) });
    await waitFor(() => expect(within(usageCard()).getByText("web is not running.")).toBeTruthy());
  });

  it("keeps both places while the first answer loads", async () => {
    render(<MachineDetail kind="Container" name="web" />, { wrapper: withApi(paletteApi({ usage: () => never() })) });
    await waitFor(() => expect(within(usageCard()).getAllByText("Loading…").length).toBe(2));
  });

  it("shows a container's CPU with no memory when its guest cannot say it", async () => {
    const r = answer(MachineMemoryKind.GUEST_USED);
    const bare = { ...r, points: r.points.map((p) => ({ time: p.time, cpu: p.cpu })), latest: { time: r.latest.time, cpu: r.latest.cpu } };
    render(<MachineDetail kind="Container" name="web" />, { wrapper: withApi(paletteApi({ usage: () => bare })) });
    await waitFor(() => expect(within(usageCard()).getByText("37%")).toBeTruthy());
    expect(within(usageCard()).getByText("no data")).toBeTruthy();
  });

  it("takes the last answer off the card when a later ask fails", async () => {
    let fail = false;
    const api = paletteApi({
      usage: () => {
        if (fail) throw new ConnectError("node gone", Code.Unavailable);
        return answer(MachineMemoryKind.GUEST_USED);
      },
    });
    render(<MachineDetail kind="Container" name="web" />, { wrapper: withApi(api) });
    await waitFor(() => expect(within(usageCard()).getByText("37%")).toBeTruthy());
    fail = true;
    await api.client.refetchQueries();
    await waitFor(() => expect(within(usageCard()).getAllByText("Usage did not load.").length).toBe(2));
    expect(within(usageCard()).queryByText("37%")).toBeNull();
  });

  it("keeps both places when the ask fails otherwise", async () => {
    const api = paletteApi({
      usage: () => {
        throw new ConnectError("node gone", Code.Unavailable);
      },
    });
    render(<MachineDetail kind="Container" name="web" />, { wrapper: withApi(api) });
    await waitFor(() => expect(within(usageCard()).getAllByText("Usage did not load.").length).toBe(2));
  });
});

describe("toUsage", () => {
  const now = new Date();
  const min = (m: number) => timestampFromDate(new Date(now.getTime() - m * 60_000));
  /** The CPU series as drawn: how many lines, and how many lone points as dots. */
  const drawn = (r: Parameters<typeof toUsage>[0]) => {
    const { line, dots } = plotPaths(toUsage(r, "Container").cpu, "1h", 1, now);
    return { lines: line.split("M").length - 1, dots: dots.split("M").length - 1 };
  };
  const pts = (...mins: number[]) => mins.map((m) => ({ time: min(m), cpu: 0.2 }));

  it("breaks the line at a missing minute, and joins the latest sample up to 150 s past the last point", () => {
    expect(drawn({ points: pts(10, 9, 8, 7) } as never)).toEqual({ lines: 1, dots: 0 });
    expect(drawn({ points: pts(10, 9, 7, 6) } as never)).toEqual({ lines: 2, dots: 0 });
    expect(drawn({ points: pts(10, 9, 7, 6), latest: { time: min(3.6), cpu: 0.3 } } as never)).toEqual({ lines: 2, dots: 0 });
    expect(drawn({ points: pts(10, 9, 7, 6), latest: { time: min(3.4), cpu: 0.3 } } as never)).toEqual({ lines: 2, dots: 1 });
    // Without a latest sample, the last minute point gets no leeway.
    expect(drawn({ points: pts(10, 9, 7) } as never)).toEqual({ lines: 1, dots: 1 });
  });

  it("ends each series at the latest sample, and drops a point without memory from memory alone", () => {
    const r = answer(MachineMemoryKind.GUEST_USED);
    const u = toUsage({ ...r, points: [{ time: at(3), cpu: 0.1 }, ...r.points] } as never, "Container");
    expect(u.cpu.points.map((p) => p.value)).toEqual([0.1, 0.1, 0.2, 0.37]);
    expect(u.memory.points.length).toBe(3);
    expect(u.memory.capacity).toBe(2 * 1024 ** 3);
    expect(u.cpu.capacity).toBe(1);
  });
});
