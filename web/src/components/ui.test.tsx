// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { CopyButton } from "@/components/ui";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function clipboard(writeText: (t: string) => Promise<void>) {
  vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
}

describe("CopyButton", () => {
  it("shows the check once the clipboard took the text", async () => {
    const writeText = vi.fn(async () => {});
    clipboard(writeText);
    render(<CopyButton text="hangar token get ci -p demo" />);
    fireEvent.click(screen.getByRole("button", { name: "Copy" }));
    await screen.findByLabelText("Copied");
    expect(writeText).toHaveBeenCalledWith("hangar token get ci -p demo");
    expect(screen.queryByText(/Not copied/)).toBeNull();
  });

  it("shows no check, and says so, when the clipboard refused", async () => {
    clipboard(async () => {
      throw new DOMException("denied", "NotAllowedError");
    });
    render(<CopyButton text="x" />);
    fireEvent.click(screen.getByRole("button", { name: "Copy" }));
    await screen.findByText("Not copied: the clipboard is unavailable.");
    expect(screen.queryByLabelText("Copied")).toBeNull();
  });

  it("says so when there is no clipboard at all", async () => {
    vi.stubGlobal("navigator", { ...navigator, clipboard: undefined });
    render(<CopyButton text="x" />);
    fireEvent.click(screen.getByRole("button", { name: "Copy" }));
    await screen.findByText("Not copied: the clipboard is unavailable.");
    expect(screen.queryByLabelText("Copied")).toBeNull();
  });
});
