import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, it } from "vitest";
import { checkName, createError, type KnownNames } from "@/lib/create";

const known = (over: Partial<KnownNames> = {}): KnownNames => ({ loading: false, error: null, names: ["web"], ...over });

describe("checkName", () => {
  it("lets a free DNS label through once the names are known", () => {
    expect(checkName("api", "container", known())).toEqual({ ok: true });
  });

  it("refuses a name the project holds", () => {
    expect(checkName("web", "container", known())).toEqual({ ok: false, reason: "A container named web exists in this project." });
  });

  // An empty list while loading or after a failure says nothing about which
  // names are taken: sending then could replace an existing machine.
  it("refuses every name while the names load", () => {
    for (const name of ["web", "api", ""]) {
      const c = checkName(name, "container", known({ loading: true, names: [] }));
      expect(c.ok).toBe(false);
    }
  });

  it("refuses every name when the names failed to load", () => {
    const c = checkName("api", "virtual machine", known({ error: new Error("unavailable"), names: [] }));
    expect(c).toEqual({ ok: false, reason: "The list of the project's virtual machines did not load, so which names are taken is not known." });
  });

  it("refuses what is not a DNS label", () => {
    for (const name of ["", "Web", "-web", "web-", "a".repeat(64), "we_b"]) {
      expect(checkName(name, "container", known()).ok, name).toBe(false);
    }
    expect(checkName("a".repeat(63), "container", known()).ok).toBe(true);
  });
});

describe("createError", () => {
  it("says a taken name in the form's words", () => {
    const err = createError(new ConnectError("VirtualMachine box already exists", Code.AlreadyExists), "virtual machine", "box");
    expect(err).toMatchObject({ code: Code.AlreadyExists, rawMessage: "A virtual machine named box exists in this project." });
  });

  it("keeps any other error as it is", () => {
    const api = new ConnectError("down", Code.Unavailable);
    expect(createError(api, "container", "api")).toBe(api);
  });
});
