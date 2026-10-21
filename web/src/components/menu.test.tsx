// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { Trash2 } from "lucide-react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { placeLayer, RowMenu } from "@/components/menu";

afterEach(cleanup);

const viewport = { width: 1000, height: 800 };
const rect = (top: number, bottom: number, right = 900) => ({ top, bottom, right }) as DOMRect;

describe("placeLayer", () => {
  it("puts the layer below its control when it fits there", () => {
    expect(placeLayer(rect(100, 132), 80, viewport)).toEqual({ top: 136, right: 100 });
  });

  it("puts the layer above its control when the viewport has no room below", () => {
    expect(placeLayer(rect(740, 772), 80, viewport)).toEqual({ top: 656, right: 100 });
  });

  it("keeps the layer below when it fits on neither side", () => {
    expect(placeLayer(rect(40, 72), 780, viewport).top).toBe(76);
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
