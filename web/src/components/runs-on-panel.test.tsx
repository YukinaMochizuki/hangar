// @vitest-environment jsdom

import { create } from "@bufbuild/protobuf";
import { PlacementSchema } from "@hangar/api/hangar/compute/v1alpha1/placement_pb";
import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { RunsOnPanel } from "@/components/runs-on-panel";
import { placementOf } from "@/lib/placement";
import type { RunsOnState } from "@/lib/runs-on";

const host = placementOf(create(PlacementSchema, { cpuModel: "AMD EPYC 7313", region: "ap-southeast", chassis: "generic-1u-2x-u2" }), "sgp02-01")!;
const placed: RunsOnState = { kind: "placed", host };
const scheduling: RunsOnState = { kind: "scheduling" };
const unschedulable: RunsOnState = { kind: "unschedulable", message: "no ready node has 8192Mi of memory free" };
const stopped: RunsOnState = { kind: "stopped" };

let reduced = false;
beforeEach(() => {
  reduced = false;
  vi.useFakeTimers();
  vi.stubGlobal("matchMedia", (q: string) => ({
    matches: q.includes("reduced-motion") && reduced,
    addEventListener: () => {},
    removeEventListener: () => {},
  }));
});
afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

const sweep = () => document.querySelector(".runs-on-sweep");
const drawing = () => document.querySelector(".art-draw, .arrive-row");
// What the panel says besides its title, the hidden layout excluded.
const said = (c: HTMLElement) => {
  const copy = c.cloneNode(true) as HTMLElement;
  copy.querySelectorAll("[aria-hidden]").forEach((e) => e.remove());
  return copy.textContent?.replace(/\u200b/g, "").replace("Runs on", "").trim();
};

describe("RunsOnPanel", () => {
  it("scheduling: says so in the header, the body empty with a light sweeping", () => {
    const { container } = render(<RunsOnPanel state={scheduling} status="pending" />);
    expect(said(container)).toBe("Scheduling…");
    expect(sweep()).not.toBeNull();
  });

  it("not schedulable: says so in the header and the scheduler's words in the body, nothing sweeping", () => {
    const { container } = render(<RunsOnPanel state={unschedulable} status="pending" />);
    expect(said(container)).toBe("Not schedulableNo ready node has 8192Mi of memory free.");
    // A warning, not an error: the machine may yet be placed.
    expect(screen.getByText("Not schedulable").className).toContain("text-pending");
    expect(sweep()).toBeNull();
  });

  it("stopped before it was placed: nothing in the header, nothing in the body", () => {
    const { container } = render(<RunsOnPanel state={stopped} status="stopped" />);
    expect(said(container)).toBe("");
    expect(sweep()).toBeNull();
  });

  it("placed when the page opens: the host's name and its rows, with nothing played", () => {
    render(<RunsOnPanel state={placed} status="running" />);
    expect(screen.getByText("sgp02-01")).toBeTruthy();
    expect(screen.getByText("AMD EPYC 7313")).toBeTruthy();
    expect(drawing()).toBeNull();
  });

  it("placed while watched: the host draws itself in and its rows fade in, the sweep gone", () => {
    const { rerender } = render(<RunsOnPanel state={unschedulable} status="pending" />);
    rerender(<RunsOnPanel state={scheduling} status="pending" />);
    expect(sweep()).not.toBeNull();
    rerender(<RunsOnPanel state={placed} status="running" />);
    expect(sweep()).toBeNull();
    expect(drawing()).not.toBeNull();
    expect(screen.getByText("sgp02-01")).toBeTruthy();
    act(() => void vi.advanceTimersByTime(1100));
    expect(drawing()).toBeNull();
    expect(screen.getByText("AMD EPYC 7313")).toBeTruthy();
  });

  it("with reduced motion asked for: no sweep, and the host appears at once", () => {
    reduced = true;
    const { rerender } = render(<RunsOnPanel state={scheduling} status="pending" />);
    expect(sweep()).toBeNull();
    rerender(<RunsOnPanel state={placed} status="running" />);
    expect(drawing()).toBeNull();
    expect(screen.getByText("AMD EPYC 7313")).toBeTruthy();
  });

  it("never says the machine is not placed yet", () => {
    for (const s of [scheduling, unschedulable, stopped]) {
      const { unmount } = render(<RunsOnPanel state={s} status="pending" />);
      expect(screen.queryByText(/Not placed yet/)).toBeNull();
      unmount();
    }
  });
});
