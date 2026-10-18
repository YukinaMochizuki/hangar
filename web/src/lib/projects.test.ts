import { describe, expect, it } from "vitest";
import { randomSuffix, slug } from "@/lib/projects";

describe("slug", () => {
  // The cases internal/cli's TestSlug holds `hangar create` to, so the
  // console suggests the names the CLI would.
  it.each([
    ["My Demo", "my-demo"],
    ["  --Alice's   Lab!  ", "alice-s-lab"],
    ["Ünïcode café", "unicode-cafe"],
    ["Ｗｉｄｅ", "wide"],
    ["我的", ""],
    ["a very long display name here", "a-very-long-display"],
    ["abcdefghijklmnopqrst-uvw", "abcdefghijklmnopqrst"],
    ["abcdefghijklmnopqrs uvw", "abcdefghijklmnopqrs"],
    ["2024 plans", "2024-plans"],
  ])("makes %j into %j", (input, want) => {
    expect(slug(input)).toBe(want);
    expect(slug(input).length).toBeLessThanOrEqual(20);
  });
});

describe("randomSuffix", () => {
  it("is six digits", () => {
    for (let i = 0; i < 50; i++) expect(randomSuffix()).toMatch(/^\d{6}$/);
  });
});
