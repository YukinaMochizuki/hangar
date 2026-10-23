// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { CliSetupCard } from "@/components/cli-setup";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function onPlatform(platform: string) {
  vi.stubGlobal("navigator", { ...navigator, platform });
}

const card = () => <CliSetupCard project="web" release="v0.2.0" origin="https://console.hangarlab.cloud" />;

describe("CliSetupCard", () => {
  it("shows the Windows lines first in a browser on Windows", () => {
    onPlatform("Win32");
    render(card());
    expect(screen.getByRole("radio", { name: "Windows" }).getAttribute("aria-checked")).toBe("true");
    expect(screen.getByText(/install\.ps1/).textContent).toBe(
      "PS> & ([scriptblock]::Create((irm https://github.com/YukinaMochizuki/hangar/releases/download/v0.2.0/install.ps1))) -Endpoint https://api.hangarlab.cloud",
    );
    expect(screen.queryByText(/install\.sh/)).toBeNull();
  });

  it("shows the macOS and Linux lines first elsewhere, and switches", () => {
    onPlatform("MacIntel");
    render(card());
    expect(screen.getByRole("radio", { name: "macOS/Linux" }).getAttribute("aria-checked")).toBe("true");
    expect(screen.getByText(/install\.sh/)).toBeTruthy();

    fireEvent.click(screen.getByRole("radio", { name: "Windows" }));
    expect(screen.getByText(/install\.ps1/)).toBeTruthy();
    expect(screen.queryByText(/install\.sh/)).toBeNull();
    expect(screen.getByText(/%LOCALAPPDATA%/)).toBeTruthy();

    fireEvent.click(screen.getByRole("radio", { name: "macOS/Linux" }));
    expect(screen.getByText(/install\.sh/)).toBeTruthy();
  });
});
