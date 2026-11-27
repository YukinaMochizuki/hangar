// @vitest-environment jsdom

// The Overview's memory pool card: who holds the pool now, in order, marked,
// muted where the caller reads no further; its card on hover, focus and tap;
// and nothing at all until the first answer.

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { MachinePhase } from "@hangar/api/hangar/compute/v1alpha1/machine_pb";
import {
  MemoryPoolUsageResponseSchema,
  TenantCapabilityService,
  type MemoryPoolUsageResponse,
} from "@hangar/api/hangar/tenancy/v1alpha1/tenant_capability_pb";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { PoolCard } from "@/components/pool-card";
import { poolRefetchMs } from "@/lib/memory-pool";
import { fakeApi, never, withApi } from "@/test/harness";

const navigated: string[] = [];
vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  // An anchor that keeps its handlers, and goes to its path unless a handler stopped it.
  Link: ({ to, params, children, onClick, ...rest }: { to: string; params?: Record<string, string>; children?: ReactNode; onClick?: (ev: React.MouseEvent) => void }) => {
    const href = Object.entries(params ?? {}).reduce((p, [k, v]) => p.replace(`$${k}`, v), to);
    return (
      <a
        href={href}
        {...rest}
        onClick={(ev) => {
          onClick?.(ev);
          if (!ev.defaultPrevented) navigated.push(href);
          ev.preventDefault();
        }}
      >
        {children}
      </a>
    );
  },
}));

afterEach(() => {
  cleanup();
  navigated.length = 0;
  vi.useRealTimers();
});

const GiB = 1024n ** 3n;
const gi = (v: number) => BigInt(Math.round(v * 1024)) * (GiB / 1024n);
const hoursAgo = (h: number) => timestampFromDate(new Date(Date.now() - h * 3600_000));

/** A machine of the pool: memory, reserved and now in GiB. */
function machine(project: string, name: string, memory: number, reserved: number, now?: number, more: object = {}) {
  return {
    project,
    kind: "Container",
    name,
    phase: MachinePhase.RUNNING,
    memoryBytes: gi(memory),
    reservedBytes: gi(reserved),
    ...(now === undefined ? {} : { currentBytes: gi(now) }),
    ...more,
  };
}

/** Acme's 8 GiB pool: db Dedicated 1, web 2.75 of 1–4, worker stopped for memory 0.5 of 0.5–2, batch not sharing 1 of 1–2, all in web but batch, in jobs. */
const acme = (more: object = {}) =>
  create(MemoryPoolUsageResponseSchema, {
    sizeBytes: 8n * GiB,
    sampleTime: timestampFromDate(new Date()),
    machines: [
      machine("web", "worker", 2, 0.5, 0.5, { lastOomKillTime: hoursAgo(0.4) }),
      machine("web", "web", 4, 1, 2.75),
      machine("jobs", "batch", 2, 1, 1, { balloonUnresponsiveSince: hoursAgo(0.2) }),
      machine("web", "db", 1, 1, 1),
    ],
    ...more,
  });

function api(answer: () => Promise<MemoryPoolUsageResponse> | MemoryPoolUsageResponse) {
  return fakeApi((r) => {
    r.service(TenantCapabilityService, { memoryPoolUsage: async () => answer() });
  });
}

const segments = () => [...document.querySelectorAll("[data-segment]")] as HTMLElement[];
const segment = (name: string) => segments().find((s) => s.getAttribute("aria-label")?.startsWith(name))!;

describe("the pool card", () => {
  it("draws the tenant's machines Dedicated first, then Shared ones largest first, each as long as it has now, with a legend and no percent", async () => {
    render(<PoolCard tenant="acme" />, { wrapper: withApi(api(() => acme())) });
    await waitFor(() => expect(segments()).toHaveLength(4));
    expect(segments().map((s) => s.getAttribute("data-segment"))).toEqual(["web/Container/db", "web/Container/web", "jobs/Container/batch", "web/Container/worker"]);
    expect(segment("web").style.flex).toBe("2.75 1 0px");
    expect(segment("web").getAttribute("href")).toBe("/projects/web/containers/web");
    const card = screen.getByLabelText("Memory pool");
    for (const word of ["reserved", "shared", "free"]) expect(within(card).getByText(word)).toBeTruthy();
    // 5.25 of 8 GiB held: the scale says both.
    expect(within(card).getByText("5.25 GiB")).toBeTruthy();
    expect(within(card).getByText("8 GiB")).toBeTruthy();
    expect(screen.getByTestId("pool-track").hasAttribute("data-full")).toBe(false);
    expect(card.textContent).not.toContain("%");
  });

  it("marks a machine stopped for memory within the day, and one not sharing memory, on its segment", async () => {
    render(<PoolCard tenant="acme" />, { wrapper: withApi(api(() => acme())) });
    await waitFor(() => expect(segments()).toHaveLength(4));
    expect(segment("worker").querySelector('[data-mark="killed"]')).toBeTruthy();
    expect(segment("worker").getAttribute("aria-label")).toBe("worker, stopped for memory, 0.5 GiB now");
    expect(segment("batch").querySelector('[data-mark="stuck"]')).toBeTruthy();
    expect(segment("web").querySelector("[data-mark]")).toBeNull();
    cleanup();
    const old = acme({ machines: [machine("web", "worker", 2, 0.5, 0.5, { lastOomKillTime: hoursAgo(25) })] });
    render(<PoolCard tenant="acme" />, { wrapper: withApi(api(() => old)) });
    await waitFor(() => expect(segments()).toHaveLength(1));
    expect(segment("worker").querySelector("[data-mark]")).toBeNull();
  });

  it("on a project's Overview, names its machines and folds the rest, and what the API summed, into one muted piece", async () => {
    const answer = acme({ other: { memoryBytes: 4n * GiB, reservedBytes: 1n * GiB, currentBytes: gi(1.5) } });
    render(<PoolCard tenant="acme" project="web" />, { wrapper: withApi(api(() => answer)) });
    await waitFor(() => expect(segments()).toHaveLength(4));
    const other = segments()[3];
    expect(other.getAttribute("data-segment")).toBe("other");
    expect(other.tagName).toBe("BUTTON");
    // batch's 1 GiB and the 1.5 GiB of projects the caller does not read.
    expect(other.style.flex).toBe("2.5 1 0px");
    expect(other.textContent).toBe("");
    fireEvent.pointerEnter(other, { pointerType: "mouse" });
    expect(screen.getByRole("tooltip").textContent).toBe("Other projects · 2.5 GiB now");
  });

  it("opens a machine's card on hover and on focus, dims the others, and a click goes to the machine", async () => {
    render(<PoolCard tenant="acme" />, { wrapper: withApi(api(() => acme())) });
    await waitFor(() => expect(segments()).toHaveLength(4));
    fireEvent.pointerEnter(segment("web"), { pointerType: "mouse" });
    const tip = screen.getByRole("tooltip");
    // Its name, and its project's.
    expect(within(tip).getAllByText("web")).toHaveLength(2);
    expect(within(tip).getByText("Container")).toBeTruthy();
    expect(within(tip).getByText("Running")).toBeTruthy();
    expect(tip.textContent).toContain("2.75 GiB now");
    expect(tip.textContent).toContain("1 GiB reserved + 1.75 GiB shared");
    expect(tip.textContent).toContain("up to 4 GiB");
    expect(segment("db").className).toContain("opacity-35");
    expect(segment("web").className).not.toContain("opacity-35");
    fireEvent.pointerDown(segment("web"), { pointerType: "mouse" });
    fireEvent.click(segment("web"), { detail: 1 });
    expect(navigated).toEqual(["/projects/web/containers/web"]);
    fireEvent.pointerLeave(segment("web"), { pointerType: "mouse" });
    expect(screen.queryByRole("tooltip")).toBeNull();

    act(() => segment("worker").focus());
    expect(screen.getByRole("tooltip").textContent).toContain("Stopped for memory 24m ago");
    act(() => segment("worker").blur());
    expect(screen.queryByRole("tooltip")).toBeNull();
  });

  it("pins the card on a tap, its name the link, and a tap elsewhere closes it and still reaches what it tapped", async () => {
    render(
      <>
        <PoolCard tenant="acme" />
        <button type="button">elsewhere</button>
      </>,
      { wrapper: withApi(api(() => acme())) },
    );
    await waitFor(() => expect(segments()).toHaveLength(4));
    fireEvent.pointerDown(segment("batch"), { pointerType: "touch" });
    fireEvent.click(segment("batch"), { detail: 1 });
    expect(navigated).toEqual([]);
    const tip = screen.getByRole("tooltip");
    expect(tip.textContent).toContain("Not sharing memory");
    fireEvent.click(within(tip).getByRole("link"));
    expect(navigated).toEqual(["/projects/jobs/containers/batch"]);

    const elsewhere = screen.getByRole("button", { name: "elsewhere" });
    const tapped = vi.fn();
    elsewhere.addEventListener("pointerdown", tapped);
    fireEvent.pointerDown(elsewhere, { pointerType: "touch" });
    expect(tapped).toHaveBeenCalled();
    expect(screen.queryByRole("tooltip")).toBeNull();
  });

  it("goes to the machine on Enter, whatever pointer pressed last", async () => {
    render(<PoolCard tenant="acme" />, { wrapper: withApi(api(() => acme())) });
    await waitFor(() => expect(segments()).toHaveLength(4));
    fireEvent.pointerDown(segment("db"), { pointerType: "touch" });
    // A key's click has no pointer: detail 0.
    fireEvent.click(segment("web"), { detail: 0 });
    expect(navigated).toEqual(["/projects/web/containers/web"]);
  });

  it("inks the track's edge and shows only the pool's size once it is full", async () => {
    const full = acme({ machines: [machine("web", "big", 8, 2, 7.95)] });
    render(<PoolCard tenant="acme" />, { wrapper: withApi(api(() => full)) });
    await waitFor(() => expect(screen.getByTestId("pool-track").hasAttribute("data-full")).toBe(true));
    const size = screen.getByText("8 GiB");
    expect(size.className).toContain("font-medium");
    expect(screen.queryByText("7.95 GiB")).toBeNull();
  });

  it("draws a machine its node could not tell at its reservation, and says nothing of now", async () => {
    const unread = acme({ machines: [machine("web", "web", 4, 1)] });
    render(<PoolCard tenant="acme" />, { wrapper: withApi(api(() => unread)) });
    await waitFor(() => expect(segments()).toHaveLength(1));
    expect(segment("web").style.flex).toBe("1 1 0px");
    fireEvent.pointerEnter(segment("web"), { pointerType: "mouse" });
    expect(screen.getByRole("tooltip").textContent).not.toContain("now");
  });

  it("shows nothing while the first answer is pending, when it fails, for a tenant with no pool, or while not asked", async () => {
    for (const answer of [() => never<MemoryPoolUsageResponse>(), () => Promise.reject(new ConnectError("no", Code.Unavailable)), () => create(MemoryPoolUsageResponseSchema, {})]) {
      const a = api(answer);
      const { container } = render(<PoolCard tenant="acme" />, { wrapper: withApi(a) });
      await waitFor(() => expect(a.calls).toContain("MemoryPoolUsage"));
      await act(async () => {});
      expect(container.innerHTML).toBe("");
      cleanup();
    }
    const a = api(() => acme());
    const { container } = render(<PoolCard tenant="acme" enabled={false} />, { wrapper: withApi(a) });
    await act(async () => {});
    expect(a.calls).toEqual([]);
    expect(container.innerHTML).toBe("");
  });

  it("asks again every 10 s", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const a = api(() => acme());
    render(<PoolCard tenant="acme" />, { wrapper: withApi(a) });
    await waitFor(() => expect(segments()).toHaveLength(4));
    expect(poolRefetchMs).toBe(10_000);
    const before = a.calls.length;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(poolRefetchMs + 100);
    });
    expect(a.calls.length).toBeGreaterThan(before);
  });
});
