// @vitest-environment jsdom

import { describe, expect, it } from "vitest";
import { newRoute } from "@/router";

const search = (s: Record<string, unknown>) => (newRoute.options.validateSearch as (s: Record<string, unknown>) => unknown)(s);

describe("the new-machine route's search", () => {
  it("passes a size's name through, whatever it names", () => {
    expect(search({ kind: "vm", size: "highmem" })).toEqual({ kind: "vm", size: "highmem" });
    expect(search({ size: "q7" })).toEqual({ size: "q7" });
  });

  it("drops a size that is no name, and a kind it does not know", () => {
    expect(search({ size: "" })).toEqual({});
    expect(search({ size: 3 })).toEqual({});
    expect(search({ kind: "pod", size: "m" })).toEqual({ size: "m" });
  });
});
