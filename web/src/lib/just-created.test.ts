// @vitest-environment jsdom

import { renderHook } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { clearJustCreated, createdKey, isJustCreated, markJustCreated, useJustCreated } from "@/lib/just-created";

describe("just-created marks", () => {
  it("holds a mark until its page takes it, for that machine alone", () => {
    const key = createdKey("demo", "Container", "web");
    expect(isJustCreated(key)).toBe(false);
    markJustCreated(key);
    expect(isJustCreated(key)).toBe(true);
    expect(isJustCreated(createdKey("demo", "VirtualMachine", "web"))).toBe(false);
    expect(isJustCreated(createdKey("other", "Container", "web"))).toBe(false);
    clearJustCreated(key);
    expect(isJustCreated(key)).toBe(false);
  });

  it("is fresh on the first page opened after the create, and not on the next", () => {
    const key = createdKey("demo", "Container", "api");
    markJustCreated(key);
    const first = renderHook(() => useJustCreated(key));
    expect(first.result.current).toBe(true);
    // Still fresh for the page that opened, through its renders.
    first.rerender();
    expect(first.result.current).toBe(true);
    first.unmount();
    expect(renderHook(() => useJustCreated(key)).result.current).toBe(false);
  });

  it("is not fresh for a machine the console did not just create", () => {
    expect(renderHook(() => useJustCreated(createdKey("demo", "Container", "old"))).result.current).toBe(false);
  });
});
