// @vitest-environment jsdom

// FittingText measures again whenever what it measured no longer holds: its
// box resized, even between its first measure and its observer starting,
// and fonts loaded, which widen the text but not the box. jsdom lays nothing
// out, so each text is as wide as its length times charWidth, and every box
// is boxWidth wide.

import { act, cleanup, render, screen } from "@testing-library/react";
import { useLayoutEffect } from "react";
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from "vitest";
import { FittingText } from "@/components/fitting-text";

let charWidth = 6;
let boxWidth = 200;
const observers: ResizeObserverCallback[] = [];
const fonts = new EventTarget();

beforeAll(() => {
  const proto = HTMLElement.prototype;
  Object.defineProperty(proto, "scrollWidth", {
    configurable: true,
    get(this: HTMLElement) {
      return Math.ceil((this.textContent ?? "").length * charWidth);
    },
  });
  Object.defineProperty(proto, "clientWidth", {
    configurable: true,
    get: () => boxWidth,
  });
  globalThis.ResizeObserver = class {
    constructor(cb: ResizeObserverCallback) {
      observers.push(cb);
    }
    observe() {}
    unobserve() {}
    disconnect() {}
  } as unknown as typeof ResizeObserver;
  Object.defineProperty(document, "fonts", { configurable: true, value: Object.assign(fonts, { ready: new Promise(() => {}) }) });
});
afterAll(() => {
  // Element's own, which these shadowed, show through again.
  delete (HTMLElement.prototype as { scrollWidth?: number }).scrollWidth;
  delete (HTMLElement.prototype as { clientWidth?: number }).clientWidth;
});
beforeEach(() => {
  charWidth = 6;
  boxWidth = 200;
  observers.length = 0;
});
afterEach(cleanup);

// 26, 15 and 8 characters.
const texts = ["7.5 GiB over plan · shared", "+7.5 GiB shared", "+7.5 GiB"];
const resized = () => act(() => observers.forEach((cb) => cb([], {} as ResizeObserver)));

describe("FittingText", () => {
  it("draws the longest text that fits", () => {
    render(<FittingText texts={texts} />);
    expect(screen.getByText(texts[0])).toBeTruthy();
    cleanup();
    boxWidth = 100;
    render(<FittingText texts={texts} />);
    expect(screen.getByText(texts[1])).toBeTruthy();
    cleanup();
    boxWidth = 20;
    render(<FittingText texts={texts} />);
    expect(screen.getByText(texts[2])).toBeTruthy();
  });

  it("measures again when its box narrows after it measured, though the observer first reports the narrow width", () => {
    // The row settles narrower after the text measured and before its
    // observer starts, so the observer's first report is already of the
    // narrow width.
    function Settles() {
      useLayoutEffect(() => {
        boxWidth = 100;
      }, []);
      return null;
    }
    render(
      <>
        <FittingText texts={texts} />
        <Settles />
      </>,
    );
    expect(screen.getByText(texts[0])).toBeTruthy();
    resized();
    expect(screen.getByText(texts[1])).toBeTruthy();
    boxWidth = 300;
    resized();
    expect(screen.getByText(texts[0])).toBeTruthy();
  });

  it("measures again once fonts load, which widen the text in the same box", () => {
    render(<FittingText texts={texts} />);
    expect(screen.getByText(texts[0])).toBeTruthy();
    charWidth = 9;
    act(() => {
      fonts.dispatchEvent(new Event("loadingdone"));
    });
    expect(screen.getByText(texts[1])).toBeTruthy();
  });
});
