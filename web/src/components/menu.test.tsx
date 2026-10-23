// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { Trash2 } from "lucide-react";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { AnchoredLayer, placeLayer, RowMenu } from "@/components/menu";

afterEach(cleanup);

const viewport = { width: 1000, height: 800 };
const rect = (top: number, bottom: number, right = 900, left = right - 32) => ({ top, bottom, right, left }) as DOMRect;
const size = (height: number, width = 176) => ({ width, height });

describe("placeLayer", () => {
  it("puts the layer below its control when it fits there", () => {
    expect(placeLayer(rect(100, 132), size(80), viewport)).toEqual({ top: 136, right: 100 });
  });

  it("puts the layer above its control when the viewport has no room below", () => {
    expect(placeLayer(rect(740, 772), size(80), viewport)).toEqual({ top: 656, right: 100 });
  });

  it("keeps the layer below when it fits on neither side", () => {
    expect(placeLayer(rect(40, 72), size(780), viewport).top).toBe(76);
  });

  it("lines up left edges when aligned left", () => {
    expect(placeLayer(rect(100, 132, 340, 300), size(80, 256), viewport, "left")).toEqual({ top: 136, left: 300 });
  });

  it("keeps a right-aligned layer inside the viewport's left edge", () => {
    expect(placeLayer(rect(100, 132, 200, 170), size(80, 256), viewport)).toEqual({ top: 136, right: 736 });
  });

  it("keeps a left-aligned layer inside the viewport's right edge", () => {
    expect(placeLayer(rect(100, 132, 980, 900), size(80, 256), viewport, "left")).toEqual({ top: 136, left: 736 });
  });
});

describe("AnchoredLayer", () => {
  // A modal dialog makes the rest of the page inert and draws over it, so a
  // layer on the body under one could be neither seen nor clicked.
  it("goes in the dialog its control sits in", () => {
    const Probe = () => {
      const [anchor, setAnchor] = useState<HTMLElement | null>(null);
      return (
        <dialog open data-testid="dialog">
          <span ref={setAnchor}>control</span>
          <AnchoredLayer anchor={anchor} role="menu">
            layer
          </AnchoredLayer>
        </dialog>
      );
    };
    render(<Probe />);
    expect(screen.getByTestId("dialog").contains(screen.getByRole("menu"))).toBe(true);
  });

  it("goes on the body outside a dialog", () => {
    render(<AnchoredLayer anchor={document.body} role="menu">layer</AnchoredLayer>);
    expect(screen.getByRole("menu").parentElement).toBe(document.body);
  });
});

describe("RowMenu", () => {
  // A list's table sits in a card that scrolls sideways, which clips what
  // overflows it: the last row's menu was cut off at the card's bottom.
  it("draws its menu outside the card that scrolls the table", () => {
    render(
      <div data-testid="card" className="overflow-x-auto">
        <RowMenu label="Actions for web-3" actions={[{ label: "Delete", icon: Trash2, danger: true, onSelect: () => {} }]} />
      </div>,
    );
    fireEvent.click(screen.getByRole("button", { name: "Actions for web-3" }));
    const menu = screen.getByRole("menu");
    expect(screen.getByTestId("card").contains(menu)).toBe(false);
    expect(menu.style.position).toBe("fixed");
  });

  it("runs the action chosen and closes", () => {
    const onSelect = vi.fn();
    render(<RowMenu label="Actions" actions={[{ label: "Remove", icon: Trash2, onSelect }]} />);
    fireEvent.click(screen.getByRole("button", { name: "Actions" }));
    fireEvent.click(screen.getByRole("menuitem", { name: /Remove/ }));
    expect(onSelect).toHaveBeenCalledOnce();
    expect(screen.queryByRole("menu")).toBeNull();
  });

  it("closes on a click outside it and on Escape", () => {
    render(<RowMenu label="Actions" actions={[{ label: "Remove", icon: Trash2, onSelect: () => {} }]} />);
    fireEvent.click(screen.getByRole("button", { name: "Actions" }));
    fireEvent.mouseDown(document.body);
    expect(screen.queryByRole("menu")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Actions" }));
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.queryByRole("menu")).toBeNull();
  });
});
