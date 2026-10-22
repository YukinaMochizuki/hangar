import { describe, expect, it } from "vitest";
import { labelChanges, labelsOf, rowProblems, rowsOf, systemKey, type LabelRow } from "@/lib/labels";

const row = (id: number, key: string, value = "", system = false): LabelRow => ({ id, key, value, system });

describe("systemKey", () => {
  it("holds keys under the API's domain or a subdomain of it, as the apiserver's reservedKey does", () => {
    expect(systemKey("hangarlab.cloud/owner")).toBe(true);
    expect(systemKey("storage.hangarlab.cloud/root-disk-of")).toBe(true);
    expect(systemKey("hangar/intended-use")).toBe(false);
    expect(systemKey("nothangarlab.cloud/x")).toBe(false);
    expect(systemKey("hangarlab.cloud")).toBe(false);
  });
});

describe("rowsOf", () => {
  it("puts the system's labels first, marked, then the person's, each in key order", () => {
    const rows = rowsOf({ tier: "db", "storage.hangarlab.cloud/x": "1", app: "pg" });
    expect(rows.map((r) => [r.key, !!r.system])).toEqual([
      ["storage.hangarlab.cloud/x", true],
      ["app", false],
      ["tier", false],
    ]);
    expect(new Set(rows.map((r) => r.id)).size).toBe(3);
  });
});

describe("rowProblems", () => {
  it("takes what the API takes", () => {
    expect(rowProblems([row(0, "app", "web"), row(1, "a".repeat(317), "b".repeat(63)), row(2, "empty", "")], {}).size).toBe(0);
  });

  it("ignores a row left wholly empty", () => {
    expect(rowProblems([row(0, "", "")], {}).size).toBe(0);
    expect(labelsOf([row(0, "", ""), row(1, "app", "web")])).toEqual({ app: "web" });
  });

  it("refuses a value with no key", () => {
    expect(rowProblems([row(0, "", "web")], {}).get(0)).toEqual({ key: "A label needs a key." });
  });

  it("counts bytes, as the API does, for a key's and a value's length", () => {
    expect(rowProblems([row(0, "k".repeat(318))], {}).get(0)?.key).toBe("A key is at most 317 bytes.");
    // 159 two-byte characters are 318 bytes, and 32 are 64.
    expect(rowProblems([row(0, "é".repeat(159))], {}).get(0)?.key).toBe("A key is at most 317 bytes.");
    expect(rowProblems([row(0, "app", "é".repeat(32))], {}).get(0)?.value).toBe("A value is at most 63 bytes.");
    expect(rowProblems([row(0, "é".repeat(158), "é".repeat(31))], {}).size).toBe(0);
  });

  it("refuses a key under the system's prefix unless the object holds it with that value", () => {
    const stored = { "hangarlab.cloud/owner": "me" };
    expect(rowProblems([row(0, "hangarlab.cloud/owner", "you")], stored).get(0)?.key).toBe("Keys under hangarlab.cloud/ are the system's.");
    expect(rowProblems([row(0, "net.hangarlab.cloud/x", "1")], {}).get(0)?.key).toBe("Keys under hangarlab.cloud/ are the system's.");
    expect(rowProblems([row(0, "hangarlab.cloud/owner", "me")], stored).size).toBe(0);
    // A system row, shown locked, is sent back as it is.
    expect(rowProblems([row(0, "hangarlab.cloud/owner", "me", true)], stored).size).toBe(0);
  });

  it("refuses a key given twice, on the later row", () => {
    const p = rowProblems([row(0, "app", "a"), row(1, "app", "b")], {});
    expect(p.has(0)).toBe(false);
    expect(p.get(1)?.key).toBe("This key is given above.");
  });
});

describe("labelChanges", () => {
  it("says which keys are added, changed and removed", () => {
    expect(labelChanges({ app: "pg", tier: "db", old: "1" }, { app: "pg", tier: "data", team: "ops" })).toEqual({
      added: ["team"],
      changed: ["tier"],
      removed: ["old"],
      none: false,
    });
    expect(labelChanges({ app: "pg" }, { app: "pg" }).none).toBe(true);
  });
});
