import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, it, vi } from "vitest";
import { checkName, createIfAbsent, type KnownNames } from "@/lib/create";

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

const notFound = () => Promise.reject(new ConnectError("not found", Code.NotFound));

describe("createIfAbsent", () => {
  it("applies when the Get finds nothing", async () => {
    const apply = vi.fn(async () => ({ created: true }));
    await createIfAbsent({ get: notFound, apply }, "container", "api");
    expect(apply).toHaveBeenCalledOnce();
  });

  it("does not apply when the object exists", async () => {
    const apply = vi.fn(async () => ({ created: true }));
    const err = await createIfAbsent({ get: async () => ({}), apply }, "container", "web").catch((e) => e);
    expect(apply).not.toHaveBeenCalled();
    expect(err).toBeInstanceOf(ConnectError);
    expect((err as ConnectError).code).toBe(Code.AlreadyExists);
  });

  it("does not apply when the Get fails otherwise", async () => {
    const apply = vi.fn(async () => ({ created: true }));
    const get = () => Promise.reject(new ConnectError("down", Code.Unavailable));
    await expect(createIfAbsent({ get, apply }, "container", "api")).rejects.toMatchObject({ code: Code.Unavailable });
    expect(apply).not.toHaveBeenCalled();
  });

  it("says so when the apply replaced an object made in between", async () => {
    const apply = vi.fn(async () => ({ created: false }));
    await expect(createIfAbsent({ get: notFound, apply }, "container", "api")).rejects.toMatchObject({ code: Code.AlreadyExists });
  });
});
