// @vitest-environment jsdom

// The Overview's memory pool card: who uses the pool now, in order, marked,
// muted where the caller reads no further; its card on hover, focus and tap;
// its place held while the first answer is pending; and an answer not of
// now shown as such.

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
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { PoolCard } from "@/components/pool-card";
import { poolDropMs, poolRefetchMs } from "@/lib/memory-pool";
import { fakeApi, withApi } from "@/test/harness";

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

/**
 * Acme's 8 GiB pool, in GiB: db Dedicated 1 using 0.6; web 1–4 holding
 * 2.75, using 1.4; worker 0.5–2 holding 1.2, using 0.4, stopped for memory;
 * batch 1–2 holding 1, its guest saying nothing of its use, not sharing
 * memory. All in web but batch, in jobs. By what they hold, worker would
 * come before batch.
 */
const acme = (more: object = {}) =>
  create(MemoryPoolUsageResponseSchema, {
    sizeBytes: 8n * GiB,
    sampleTime: timestampFromDate(new Date()),
    machines: [
      machine("web", "worker", 2, 0.5, 1.2, { inUseBytes: gi(0.4), lastOomKillTime: hoursAgo(0.4) }),
      machine("web", "web", 4, 1, 2.75, { inUseBytes: gi(1.4) }),
      machine("jobs", "batch", 2, 1, 1, { balloonUnresponsiveSince: hoursAgo(0.2) }),
      machine("web", "db", 1, 1, 1, { inUseBytes: gi(0.6) }),
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
  it("draws the tenant's machines Dedicated first, then Shared ones by what they use, largest first, each as long as that, with a legend and no percent", async () => {
    render(<PoolCard tenant="acme" />, { wrapper: withApi(api(() => acme())) });
    await waitFor(() => expect(segments()).toHaveLength(4));
    expect(segments().map((s) => s.getAttribute("data-segment"))).toEqual(["web/Container/db", "web/Container/web", "jobs/Container/batch", "web/Container/worker"]);
    // 1.4 GiB, as whole MiB.
    expect(parseFloat(segment("web").style.flex)).toBeCloseTo(1.4, 2);
    // Up to its reservation solid, above it the lighter fill.
    const [reservedPart, sharedPart] = [...segment("web").children] as HTMLElement[];
    expect(parseFloat(reservedPart.style.width)).toBeCloseTo(100 / 1.4, 0);
    expect(sharedPart).toBeTruthy();
    // db uses less than its reservation: all of it solid.
    expect(segment("db").children).toHaveLength(1);
    expect(segment("web").getAttribute("href")).toBe("/projects/web/containers/web");
    const card = screen.getByLabelText("Memory pool");
    for (const word of ["reserved", "shared", "free"]) expect(within(card).getByText(word)).toBeTruthy();
    // 3.4 of 8 GiB in use, though they hold 5.95: the rest is free.
    expect(within(card).getByText("3.4 GiB")).toBeTruthy();
    expect(within(card).getByText("8 GiB")).toBeTruthy();
    expect(screen.getByTestId("pool-track").hasAttribute("data-full")).toBe(false);
    expect(card.textContent).not.toContain("%");
  });

  it("marks a machine stopped for memory within the day, and one not sharing memory, on its segment", async () => {
    render(<PoolCard tenant="acme" />, { wrapper: withApi(api(() => acme())) });
    await waitFor(() => expect(segments()).toHaveLength(4));
    expect(segment("worker").querySelector('[data-mark="killed"]')).toBeTruthy();
    expect(segment("worker").getAttribute("aria-label")).toBe("worker, stopped for memory, 0.4 GiB in use");
    expect(segment("batch").querySelector('[data-mark="stuck"]')).toBeTruthy();
    expect(segment("web").querySelector("[data-mark]")).toBeNull();
    cleanup();
    const old = acme({ machines: [machine("web", "worker", 2, 0.5, 0.5, { lastOomKillTime: hoursAgo(25) })] });
    render(<PoolCard tenant="acme" />, { wrapper: withApi(api(() => old)) });
    await waitFor(() => expect(segments()).toHaveLength(1));
    expect(segment("worker").querySelector("[data-mark]")).toBeNull();
  });

  it("on a project's Overview, names its machines and folds the rest, and what the API summed, into one muted piece", async () => {
    const answer = acme({ other: { memoryBytes: 4n * GiB, reservedBytes: 1n * GiB, currentBytes: gi(3), inUseBytes: gi(1.5) } });
    render(<PoolCard tenant="acme" project="web" />, { wrapper: withApi(api(() => answer)) });
    await waitFor(() => expect(segments()).toHaveLength(4));
    const other = segments()[3];
    expect(other.getAttribute("data-segment")).toBe("other");
    expect(other.tagName).toBe("BUTTON");
    // batch's 1 GiB, from what it holds, and the 1.5 GiB in use of projects the caller does not read.
    expect(other.style.flex).toBe("2.5 1 0px");
    expect(other.textContent).toBe("");
    fireEvent.pointerEnter(other, { pointerType: "mouse" });
    expect(screen.getByRole("tooltip").textContent).toBe("Other projects · 2.5 GiB in use");
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
    expect(tip.textContent).toContain("1.4 GiB in use · holds 2.75 GiB · 1–4 GiB");
    expect(tip.textContent).not.toContain("Size from");
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
    // Its guest says nothing of its use: its segment is what it holds, and its card says so.
    expect(tip.textContent).toContain("holds 1 GiB · 1–2 GiB");
    expect(tip.textContent).toContain("Size from what it holds");
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

  it("inks the track's edge and shows only the pool's size once what is in use fills it, not what is held", async () => {
    const held = acme({ machines: [machine("web", "big", 8, 2, 7.95, { inUseBytes: gi(3) })] });
    render(<PoolCard tenant="acme" />, { wrapper: withApi(api(() => held)) });
    await waitFor(() => expect(segments()).toHaveLength(1));
    expect(screen.getByTestId("pool-track").hasAttribute("data-full")).toBe(false);
    expect(screen.getByText("3 GiB")).toBeTruthy();
    cleanup();
    const full = acme({ machines: [machine("web", "big", 8, 2, 7.95, { inUseBytes: gi(7.95) })] });
    render(<PoolCard tenant="acme" />, { wrapper: withApi(api(() => full)) });
    await waitFor(() => expect(screen.getByTestId("pool-track").hasAttribute("data-full")).toBe(true));
    const size = screen.getByText("8 GiB");
    expect(size.className).toContain("font-medium");
    expect(screen.queryByText("7.95 GiB")).toBeNull();
  });

  it("draws a machine its node could not tell at its reservation, and its card says so", async () => {
    const unread = acme({ machines: [machine("web", "web", 4, 1)] });
    render(<PoolCard tenant="acme" />, { wrapper: withApi(api(() => unread)) });
    await waitFor(() => expect(segments()).toHaveLength(1));
    expect(segment("web").style.flex).toBe("1 1 0px");
    fireEvent.pointerEnter(segment("web"), { pointerType: "mouse" });
    const tip = screen.getByRole("tooltip").textContent;
    expect(tip).toContain("1–4 GiB");
    expect(tip).toContain("Size from its reservation");
    expect(tip).not.toMatch(/in use|holds/);
  });

  it("holds its place while the first answer is pending, as tall as the card and with no words, and shows nothing when it fails, for a tenant with no pool, or while not asked", async () => {
    let answer!: (r: MemoryPoolUsageResponse) => void;
    const pending = new Promise<MemoryPoolUsageResponse>((resolve) => (answer = resolve));
    render(<PoolCard tenant="acme" />, { wrapper: withApi(api(() => pending)) });
    const held = await screen.findByLabelText("Memory pool");
    expect(held.getAttribute("aria-busy")).toBe("true");
    // Its lines are the card's own, unseen, and a blank track as tall as the strip's.
    const lines = [...held.children] as HTMLElement[];
    expect(lines).toHaveLength(3);
    expect(lines[0].className).toContain("invisible");
    expect(lines[0].textContent).toContain("reserved");
    expect(screen.getByTestId("pool-track-placeholder").className).toContain("h-6");
    expect(lines[2].className).toContain("invisible");
    for (const text of [...held.querySelectorAll("*")].filter((el) => el.childNodes.length && [...el.childNodes].some((n) => n.nodeType === 3 && n.textContent!.trim())))
      expect(text.closest(".invisible")).toBeTruthy();
    const heldClass = held.className;
    await act(async () => answer(acme()));
    const card = await screen.findByLabelText("Memory pool");
    await waitFor(() => expect(card.hasAttribute("aria-busy")).toBe(false));
    // The same box, the same lines: the legend, the strip as tall, the scale.
    expect(card.className).toBe(heldClass);
    expect(screen.getByTestId("pool-track").className).toContain("h-6");
    cleanup();

    for (const fails of [() => Promise.reject(new ConnectError("no", Code.Unavailable)), () => create(MemoryPoolUsageResponseSchema, {})]) {
      const a = api(fails);
      const { container } = render(<PoolCard tenant="acme" />, { wrapper: withApi(a) });
      await waitFor(() => expect(a.calls).toContain("MemoryPoolUsage"));
      await waitFor(() => expect(container.innerHTML).toBe(""));
      cleanup();
    }
    const a = api(() => acme());
    const { container } = render(<PoolCard tenant="acme" enabled={false} />, { wrapper: withApi(a) });
    await act(async () => {});
    expect(a.calls).toEqual([]);
    expect(container.innerHTML).toBe("");
  });

  it("dims the strip and says it is not current when its nodes' read is old, or a node was not read, and not otherwise", async () => {
    const strip = () => screen.getByTestId("pool-track").parentElement!;
    const cases: [string, MemoryPoolUsageResponse, boolean][] = [
      ["a read of now", acme(), true],
      ["a read a minute old", acme({ sampleTime: timestampFromDate(new Date(Date.now() - 60_000)) }), false],
      ["a node unread", acme({ nodesUnread: 1 }), false],
      ["machines and no read", acme({ sampleTime: undefined }), false],
    ];
    for (const [what, answer, current] of cases) {
      render(<PoolCard tenant="acme" />, { wrapper: withApi(api(() => answer)) });
      await waitFor(() => expect(segments()).toHaveLength(4));
      expect(screen.queryByText(/not current/) !== null, what).toBe(!current);
      expect(strip().className.includes("opacity-50"), what).toBe(!current);
      cleanup();
    }
  });

  it("shows its last answer as not current while asking again fails, and goes once it has failed for a while", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let failing = false;
    const a = api(() => (failing ? Promise.reject(new ConnectError("no", Code.Unavailable)) : acme({ sampleTime: timestampFromDate(new Date()) })));
    const { container } = render(<PoolCard tenant="acme" />, { wrapper: withApi(a) });
    await waitFor(() => expect(segments()).toHaveLength(4));
    expect(screen.queryByText(/not current/)).toBeNull();
    failing = true;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(poolRefetchMs + 100);
    });
    await waitFor(() => expect(screen.getByText(/not current/)).toBeTruthy());
    expect(segments()).toHaveLength(4);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(poolDropMs);
    });
    await waitFor(() => expect(container.innerHTML).toBe(""));
  });

  it("pins a tapped card above the card's legend and strip, not over them", async () => {
    render(<PoolCard tenant="acme" />, { wrapper: withApi(api(() => acme())) });
    await waitFor(() => expect(segments()).toHaveLength(4));
    fireEvent.pointerDown(segment("web"), { pointerType: "touch" });
    fireEvent.click(segment("web"), { detail: 1 });
    const tip = screen.getByRole("tooltip");
    // Placed from the box that holds the legend too: on a narrow screen its
    // bottom is past that box's top, above the legend.
    expect(tip.className).toContain("max-sm:bottom-[calc(100%+16px)]");
    expect(tip.className).toContain("max-sm:top-auto");
    expect(tip.parentElement!.textContent).toContain("reserved");
    expect(tip.parentElement!.contains(screen.getByTestId("pool-track"))).toBe(true);
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

  describe("on a narrow screen with many machines", () => {
    const own = Array.from({ length: 15 }, (_, i) => machine("web", `m${String(i + 1).padStart(2, "0")}`, 2, 0.5, 1, { inUseBytes: gi(1.6 - i * 0.1) }));
    const many = () => acme({ machines: own, other: { memoryBytes: 2n * GiB, reservedBytes: GiB / 2n, currentBytes: GiB, inUseBytes: GiB } });
    /** A box's width as laid out: the track 300 px, the strip's own box 326, each segment 26 px a 28 px step apart. */
    const matchMedia = window.matchMedia;
    beforeEach(() => {
      window.matchMedia = ((q: string) => ({ matches: true, media: q, addEventListener() {}, removeEventListener() {} })) as unknown as typeof window.matchMedia;
      Object.defineProperty(HTMLElement.prototype, "clientWidth", {
        configurable: true,
        get(this: HTMLElement) {
          if (this.dataset.testid === "pool-track") return 300;
          if (this.querySelector(":scope > [data-current], :scope > div > [data-testid=pool-track]")) return 326;
          return 0;
        },
      });
      Object.defineProperty(HTMLElement.prototype, "offsetLeft", {
        configurable: true,
        get(this: HTMLElement) {
          const all = [...document.querySelectorAll("[data-segment]")];
          return Math.max(all.indexOf(this), 0) * 28 + 2;
        },
      });
      Object.defineProperty(HTMLElement.prototype, "offsetWidth", { configurable: true, get: () => 26 });
    });
    afterEach(() => {
      window.matchMedia = matchMedia;
      for (const k of ["clientWidth", "offsetLeft", "offsetWidth"]) delete (HTMLElement.prototype as unknown as Record<string, unknown>)[k];
    });

    it("keeps what fits at 26 px each, the smallest as one +N piece before the other projects'", async () => {
      render(<PoolCard tenant="acme" project="web" />, { wrapper: withApi(api(many)) });
      // (300 - 2) / 28: ten fit; eight machines, +7, and the other projects.
      await waitFor(() => expect(segments()).toHaveLength(10));
      const keys = segments().map((s) => s.getAttribute("data-segment"));
      expect(keys.slice(0, 8)).toEqual(own.slice(0, 8).map((m) => `web/Container/${m.name}`));
      expect(keys.slice(8)).toEqual(["more", "other"]);
      const more = segments()[8];
      expect(more.textContent).toBe("+7");
      fireEvent.pointerEnter(more, { pointerType: "mouse" });
      expect(screen.getByRole("tooltip").textContent).toContain("7 more machines");
      expect(screen.getByRole("tooltip").textContent).toContain("m09, m10");
    });

    it("places the card from its segment as drawn, held inside the strip", async () => {
      render(<PoolCard tenant="acme" project="web" />, { wrapper: withApi(api(many)) });
      await waitFor(() => expect(segments()).toHaveLength(10));
      // The sixth segment's middle is 5 * 28 + 2 + 13 = 155: the card's left 155 - 130.
      fireEvent.pointerEnter(segments()[5], { pointerType: "mouse" });
      await waitFor(() => expect(screen.getByRole("tooltip").style.left).toBe("25px"));
      // The last, other's, would run past the strip's 326 px: held at 326 - 260.
      fireEvent.pointerLeave(segments()[5], { pointerType: "mouse" });
      fireEvent.pointerEnter(segments()[9], { pointerType: "mouse" });
      await waitFor(() => expect(screen.getByRole("tooltip").style.left).toBe("66px"));
    });
  });
});
