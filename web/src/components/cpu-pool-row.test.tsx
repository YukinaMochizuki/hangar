// @vitest-environment jsdom

// The CPU pool on the Overview's pool card: calm, one row with the hour's
// slowed share by minute; throttled, a live strip of what each machine
// uses now, which stays open until the pool has not slowed its machines
// for five minutes; hovering a machine in either strip lights it in both;
// an answer not of now shown as such; its place held while pending.

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { MachinePhase } from "@hangar/api/hangar/compute/v1alpha1/machine_pb";
import {
  CPUPoolUsageResponseSchema,
  MemoryPoolUsageResponseSchema,
  TenantCapabilityService,
  type CPUPoolUsageResponse,
} from "@hangar/api/hangar/tenancy/v1alpha1/tenant_capability_pb";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { PoolCard } from "@/components/pool-card";
import { poolRefetchMs } from "@/lib/memory-pool";
import { fakeApi, withApi } from "@/test/harness";

vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  Link: ({ to, params, children, ...rest }: { to: string; params?: Record<string, string>; children?: ReactNode }) => (
    <a href={Object.entries(params ?? {}).reduce((p, [k, v]) => p.replace(`$${k}`, v), to)} {...rest}>
      {children}
    </a>
  ),
}));

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

const minuteMs = 60_000;
/** The start of the minute i minutes before now's. */
const minuteAgo = (i: number) => timestampFromDate(new Date(Math.floor(Date.now() / minuteMs) * minuteMs - i * minuteMs));

/** A machine of the pool, its CPU now in CPUs, unknown when undefined. */
function machine(project: string, name: string, vcpus: number, now?: number) {
  return { project, kind: "Container", name, phase: MachinePhase.RUNNING, vcpus, ...(now === undefined ? {} : { cpuNowMilli: BigInt(Math.round(now * 1000)) }) };
}

/** Acme's machines: web 1.6 of 2 vCPUs, worker 1.2 of 2, db 0.6 of 1, all in web; batch 0.4 of 1, in jobs. */
const machines = [machine("web", "worker", 2, 1.2), machine("web", "web", 2, 1.6), machine("jobs", "batch", 1, 0.4), machine("web", "db", 1, 0.6)];

/** Calm: slowed 22% and 10% in two minutes long ago, none in the last; 1% of the hour. */
const calm = (more: object = {}) =>
  create(CPUPoolUsageResponseSchema, {
    slowed: 0.01,
    milliCpu: 5000n,
    sampleTime: timestampFromDate(new Date()),
    minutes: [
      { time: minuteAgo(45), slowed: 0.22 },
      { time: minuteAgo(44), slowed: 0.1 },
      { time: minuteAgo(10), slowed: 0 },
      { time: minuteAgo(1), slowed: 0 },
    ],
    machines,
    ...more,
  });

/** Throttled: slowed in each of the last three minutes; 15% of the hour. */
const throttled = (more: object = {}) =>
  calm({
    slowed: 0.15,
    minutes: [
      { time: minuteAgo(3), slowed: 0.3 },
      { time: minuteAgo(2), slowed: 0.5 },
      { time: minuteAgo(1), slowed: 0.6 },
    ],
    ...more,
  });

const GiB = 1024n ** 3n;
/** The memory pool the card also draws, with web and db. */
const memory = () =>
  create(MemoryPoolUsageResponseSchema, {
    sizeBytes: 8n * GiB,
    sampleTime: timestampFromDate(new Date()),
    machines: ["web", "db"].map((name) => ({ project: "web", kind: "Container", name, phase: MachinePhase.RUNNING, memoryBytes: GiB, reservedBytes: GiB, currentBytes: GiB, inUseBytes: GiB / 2n })),
  });

function api(cpu: () => Promise<CPUPoolUsageResponse> | CPUPoolUsageResponse) {
  return fakeApi((r) => {
    r.service(TenantCapabilityService, { cPUPoolUsage: async () => cpu(), memoryPoolUsage: async () => memory() });
  });
}

const plan = { planCPU: 5 };
const pieces = () => [...document.querySelectorAll("[data-cpu-segment]")] as HTMLElement[];
const piece = (key: string) => pieces().find((s) => s.getAttribute("data-cpu-segment") === key)!;
const row = () => screen.getByLabelText("CPU pool");

describe("the pool card's CPU row", () => {
  it("is calm one row: CPU, the hour's minutes as bars, amber where slowed, and the hour's share; no strip", async () => {
    render(<PoolCard tenant="acme" cpu={plan} />, { wrapper: withApi(api(() => calm())) });
    await waitFor(() => expect(row().textContent).toContain("slowed 1%"));
    expect(row().hasAttribute("data-throttled")).toBe(false);
    expect(screen.queryByTestId("cpu-track")).toBeNull();
    // 60 slots, the last the minute before now's; only slowed minutes have a bar.
    const bars = [...screen.getByTestId("cpu-sparkline").querySelectorAll("rect")];
    expect(bars.map((b) => b.getAttribute("data-minute"))).toEqual(["15", "16"]);
    expect(bars.every((b) => b.getAttribute("class") === "fill-pending")).toBe(true);
    // The share is not amber while calm.
    expect(within(row()).getByText("slowed 1%").parentElement!.className).toContain("text-subtle");
    // Under the memory strip, after a rule.
    expect(row().className).toContain("border-t");
  });

  it("says nothing of a share under 1%", async () => {
    render(<PoolCard tenant="acme" cpu={plan} />, { wrapper: withApi(api(() => calm({ slowed: 0.004 }))) });
    await waitFor(() => expect(row().textContent).toContain("CPU"));
    expect(row().textContent).not.toContain("slowed");
  });

  it("opens into a live strip while throttled: the plan's CPUs, each machine as long as its use now, largest first, idle CPUs bare", async () => {
    render(<PoolCard tenant="acme" cpu={plan} />, { wrapper: withApi(api(() => throttled())) });
    await waitFor(() => expect(pieces()).toHaveLength(4));
    expect(row().hasAttribute("data-throttled")).toBe(true);
    expect(pieces().map((s) => s.getAttribute("data-cpu-segment"))).toEqual(["web/Container/web", "web/Container/worker", "web/Container/db", "jobs/Container/batch"]);
    expect(parseFloat(piece("web/Container/web").style.flex)).toBeCloseTo(1.6, 3);
    // One tone for every machine: nothing is reserved.
    expect(new Set(pieces().map((s) => s.className.match(/bg-foreground\/\d+/)![0]))).toEqual(new Set(["bg-foreground/45"]));
    const track = screen.getByTestId("cpu-track");
    // 3.8 of 5 used: 1.2 idle, the bare track.
    expect(track.hasAttribute("data-full")).toBe(false);
    expect((track.lastElementChild as HTMLElement).style.flex).toMatch(/^1\.2/);
    expect(within(row()).getByText("3.8 CPUs")).toBeTruthy();
    expect(within(row()).getByText("5 CPUs")).toBeTruthy();
    // The head stays, amber.
    expect(within(row()).getByText("slowed 15%").parentElement!.className).toContain("text-pending");
    expect(screen.getByTestId("cpu-sparkline").querySelectorAll("rect")).toHaveLength(3);
  });

  it("inks the track's edge and shows only the plan's CPUs, bold, once they are all used", async () => {
    const full = [machine("web", "web", 2, 2), machine("web", "worker", 2, 2), machine("web", "db", 1, 0.95)];
    render(<PoolCard tenant="acme" cpu={plan} />, { wrapper: withApi(api(() => throttled({ machines: full }))) });
    await waitFor(() => expect(pieces()).toHaveLength(3));
    const track = screen.getByTestId("cpu-track");
    expect(track.hasAttribute("data-full")).toBe(true);
    expect(track.className).toContain("ring-2");
    expect(within(row()).queryByText("4.9 CPUs")).toBeNull();
    expect(within(row()).getByText("5 CPUs").className).toContain("font-medium");
  });

  it("opens a machine's card on hover, its CPU now and vCPUs, and lights the same machine in the memory strip, and the other way", async () => {
    render(<PoolCard tenant="acme" cpu={plan} />, { wrapper: withApi(api(() => throttled())) });
    await waitFor(() => expect(pieces()).toHaveLength(4));
    await waitFor(() => expect(document.querySelectorAll("[data-segment]")).toHaveLength(2));
    fireEvent.pointerEnter(piece("web/Container/web"), { pointerType: "mouse" });
    const tip = screen.getByRole("tooltip");
    expect(tip.textContent).toContain("web");
    expect(tip.textContent).toContain("1.6 CPUs now · 2 vCPU");
    expect(piece("web/Container/worker").className).toContain("opacity-35");
    const memoryOf = (name: string) => document.querySelector(`[data-segment="web/Container/${name}"]`)!;
    await waitFor(() => expect(memoryOf("db").className).toContain("opacity-35"));
    expect(memoryOf("web").className).not.toContain("opacity-35");
    fireEvent.pointerLeave(piece("web/Container/web"), { pointerType: "mouse" });
    await waitFor(() => expect(memoryOf("db").className).not.toContain("opacity-35"));

    fireEvent.pointerEnter(memoryOf("db"), { pointerType: "mouse" });
    await waitFor(() => expect(piece("web/Container/web").className).toContain("opacity-35"));
    expect(piece("web/Container/db").className).not.toContain("opacity-35");
    // A machine not in the CPU strip dims nothing there.
    fireEvent.pointerLeave(memoryOf("db"), { pointerType: "mouse" });
    await waitFor(() => expect(piece("web/Container/web").className).not.toContain("opacity-35"));
  });

  it("pins a machine's card on a tap, its name the link", async () => {
    render(<PoolCard tenant="acme" cpu={plan} />, { wrapper: withApi(api(() => throttled())) });
    await waitFor(() => expect(pieces()).toHaveLength(4));
    fireEvent.pointerDown(piece("web/Container/db"), { pointerType: "touch" });
    fireEvent.click(piece("web/Container/db"), { detail: 1 });
    const tip = screen.getByRole("tooltip");
    const link = within(tip).getByRole("link");
    expect(link.getAttribute("href")).toBe("/projects/web/containers/db");
    // Placed from the box that holds the head too: on a narrow screen it
    // opens above the head and the strip, not over them.
    expect(tip.className).toContain("max-sm:bottom-[calc(100%+16px)]");
    expect(tip.parentElement!.contains(screen.getByTestId("cpu-sparkline"))).toBe(true);
    expect(tip.parentElement!.contains(screen.getByTestId("cpu-track"))).toBe(true);
  });

  it("on a project's Overview, names its machines and folds the rest, with what the API summed, into one muted piece", async () => {
    render(<PoolCard tenant="acme" project="web" cpu={plan} />, { wrapper: withApi(api(() => throttled({ other: { cpuNowMilli: 500n } }))) });
    await waitFor(() => expect(pieces()).toHaveLength(4));
    expect(pieces().map((s) => s.getAttribute("data-cpu-segment"))).toEqual(["web/Container/web", "web/Container/worker", "web/Container/db", "other"]);
    const other = piece("other");
    expect(parseFloat(other.style.flex)).toBeCloseTo(0.9, 3);
    expect(other.className).toContain("bg-foreground/15");
    expect(other.textContent).toBe("");
    fireEvent.pointerEnter(other, { pointerType: "mouse" });
    expect(screen.getByRole("tooltip").textContent).toBe("Other projects · 0.9 CPUs now");
  });

  it("leaves out of the strip a machine with no sample lately", async () => {
    render(<PoolCard tenant="acme" cpu={plan} />, { wrapper: withApi(api(() => throttled({ machines: [...machines, machine("web", "new", 1)] }))) });
    await waitFor(() => expect(pieces()).toHaveLength(4));
    expect(pieces().some((s) => s.getAttribute("data-cpu-segment")?.endsWith("/new"))).toBe(false);
  });

  it("dims and says it is not current when its nodes' samples are old, or a node was not read", async () => {
    const cases: [string, CPUPoolUsageResponse, boolean][] = [
      ["a sample of now", throttled(), true],
      ["a sample a minute old", throttled({ sampleTime: timestampFromDate(new Date(Date.now() - 60_000)) }), false],
      ["a node unread", throttled({ nodesUnread: 1 }), false],
    ];
    for (const [what, answer, current] of cases) {
      render(<PoolCard tenant="acme" cpu={plan} />, { wrapper: withApi(api(() => answer)) });
      await waitFor(() => expect(pieces()).toHaveLength(4));
      expect(within(row()).queryByText(/not current/) !== null, what).toBe(!current);
      expect(screen.getByTestId("cpu-track").parentElement!.className.includes("opacity-50"), what).toBe(!current);
      expect(screen.getByTestId("cpu-sparkline").closest("[data-current], .opacity-50")!.className.includes("opacity-50"), what).toBe(!current);
      // A machine's card is not dimmed with them.
      fireEvent.pointerEnter(piece("web/Container/web"), { pointerType: "mouse" });
      expect(screen.getByRole("tooltip").closest(".opacity-50"), what).toBeNull();
      cleanup();
    }
  });

  it("stays open until five minutes after the last slowed minute, whatever each answer between says, then closes, and does not reopen on a calm answer", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const t0 = Math.floor(Date.now() / minuteMs) * minuteMs + 5_000;
    vi.setSystemTime(t0);
    // Slowed in the minute before t0's, which ended at t0's minute.
    const slowedMinute = timestampFromDate(new Date(t0 - 5_000 - minuteMs));
    let answer = () => calm({ minutes: [{ time: slowedMinute, slowed: 0.4 }], sampleTime: timestampFromDate(new Date()) });
    const a = api(() => answer());
    render(<PoolCard tenant="acme" cpu={plan} />, { wrapper: withApi(a) });
    await waitFor(() => expect(row().hasAttribute("data-throttled")).toBe(true));
    // An answer without that minute, as from a node unread, keeps it open.
    answer = () => calm({ minutes: [], sampleTime: timestampFromDate(new Date()) });
    const heights: boolean[] = [];
    for (let i = 0; i < 35; i++) {
      await act(async () => {
        await vi.advanceTimersByTimeAsync(poolRefetchMs);
      });
      heights.push(row().hasAttribute("data-throttled"));
    }
    // 4:55 after the minute ended it is open; past 5:00 closed, and it stays
    // so. heights[k] is (k + 1) polls after t0, 5 s after the minute ended.
    const at = (sinceEnd: number) => heights[(sinceEnd - 5_000) / poolRefetchMs - 1];
    expect(at(4 * minuteMs + 55_000)).toBe(true);
    expect(at(5 * minuteMs + 15_000)).toBe(false);
    const firstClosed = heights.indexOf(false);
    expect(heights.slice(0, firstClosed).every(Boolean)).toBe(true);
    expect(heights.slice(firstClosed).every((h) => !h)).toBe(true);
  });

  it("holds its place as a calm row while pending, and goes when the ask fails; a CPU-only tenant has a card of its own", async () => {
    let answer!: (r: CPUPoolUsageResponse) => void;
    const pending = new Promise<CPUPoolUsageResponse>((resolve) => (answer = resolve));
    render(<PoolCard tenant="acme" cpu={plan} />, { wrapper: withApi(api(() => pending)) });
    await waitFor(() => expect(screen.getByTestId("cpu-row-placeholder")).toBeTruthy());
    expect(screen.getByTestId("cpu-row-placeholder").className).toContain("border-t");
    await act(async () => answer(calm()));
    await waitFor(() => expect(screen.queryByTestId("cpu-row-placeholder")).toBeNull());
    expect(row().className).toContain("border-t");
    cleanup();

    const failing = api(() => Promise.reject(new ConnectError("no", Code.Unavailable)));
    render(<PoolCard tenant="acme" cpu={plan} />, { wrapper: withApi(failing) });
    await waitFor(() => expect(failing.calls).toContain("CPUPoolUsage"));
    await waitFor(() => expect(screen.queryByLabelText("CPU pool")).toBeNull());
    await waitFor(() => expect(screen.queryByTestId("cpu-row-placeholder")).toBeNull());
    expect(screen.getByLabelText("Memory pool")).toBeTruthy();
    cleanup();

    const only = api(() => calm());
    render(<PoolCard tenant="acme" enabled={false} cpu={plan} />, { wrapper: withApi(only) });
    const card = await screen.findByLabelText("CPU pool");
    await waitFor(() => expect(card.textContent).toContain("slowed 1%"));
    expect(only.calls).not.toContain("MemoryPoolUsage");
    expect(card.querySelector("[data-testid=pool-track]")).toBeNull();
    cleanup();

    const none = api(() => calm());
    const { container } = render(<PoolCard tenant="acme" enabled={false} />, { wrapper: withApi(none) });
    await act(async () => {});
    expect(none.calls).toEqual([]);
    expect(container.innerHTML).toBe("");
  });
});
