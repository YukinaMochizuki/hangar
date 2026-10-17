import { afterEach, describe, expect, it, vi } from "vitest";
import { transportOptions } from "@/lib/api";

describe("transportOptions", () => {
  afterEach(() => vi.unstubAllGlobals());

  // A GET may come without Origin, which the apiserver's CSRF check needs:
  // every call must be a POST (docs/decisions/console-design.md).
  it("never uses Connect's GET form", () => {
    expect(transportOptions("https://console.example.com").useHttpGet).toBe(false);
  });

  it("calls the console's own origin, with the cookie and no redirect", async () => {
    const o = transportOptions("https://console.example.com");
    expect(o.baseUrl).toBe("https://console.example.com");
    const fetch = vi.fn(async () => new Response("{}"));
    vi.stubGlobal("fetch", fetch);
    await o.fetch!("https://console.example.com/x", { method: "POST" });
    expect(fetch).toHaveBeenCalledWith("https://console.example.com/x", { method: "POST", credentials: "same-origin", redirect: "error" });
  });
});
