import { describe, expect, it } from "vitest";
import { filterRanked, matchAt, matchSections, rank, Rank } from "@/lib/palette";

const names = (xs: { entry: { title: string } }[]) => xs.map((x) => x.entry.title);

describe("rank", () => {
  it("ranks an exact match above a prefix, and a prefix above a match anywhere", () => {
    expect(rank("web", "web")).toBe(Rank.Exact);
    expect(rank("web", "web-worker")).toBe(Rank.Prefix);
    expect(rank("web", "api-web")).toBe(Rank.Contains);
    expect(rank("web", "postgres")).toBe(Rank.None);
  });

  it("ignores case and the spaces around the query", () => {
    expect(rank("  WEB ", "Web")).toBe(Rank.Exact);
    expect(rank("stag", "Web staging")).toBe(Rank.Contains);
  });

  it("takes the best of the texts, so a display name or an ID matches", () => {
    expect(rank("acme-web", "Web", "acme-web")).toBe(Rank.Exact);
    expect(rank("we", "Web", undefined)).toBe(Rank.Prefix);
  });

  it("matches everything with an empty query", () => {
    expect(rank("", "anything")).toBe(Rank.Contains);
  });
});

describe("matchAt", () => {
  it("is where the query first occurs, case aside", () => {
    expect(matchAt("Gres", "postgres")).toEqual([4, 8]);
    expect(matchAt("x", "postgres")).toBeUndefined();
    expect(matchAt("", "postgres")).toBeUndefined();
  });
});

describe("filterRanked", () => {
  const entries = [{ title: "api-web" }, { title: "web-worker" }, { title: "cron" }, { title: "web" }, { title: "my-web" }];

  it("keeps what matches, exact first, then prefixes, then the rest, each in the order given", () => {
    expect(names(filterRanked("web", entries, (e) => [e.title]))).toEqual(["web", "web-worker", "api-web", "my-web"]);
  });

  it("drops what does not match", () => {
    expect(filterRanked("zz", entries, (e) => [e.title])).toEqual([]);
  });
});

describe("matchSections", () => {
  const s = (key: string, ...titles: string[]) => ({ key, entries: titles.map((title) => ({ title })) });
  const always = [s("actions", "New disk"), s("pages", "Disks", "Overview")];
  const searched = [s("containers", "web-disk"), s("disks", "disk"), s("projects")];

  it("shows the sections always shown, as they are, with no query", () => {
    expect(matchSections("  ", always, searched)).toBe(always);
  });

  it("puts the section holding the best match first and drops the sections with none", () => {
    const got = matchSections("disk", always, searched);
    // disks holds the exact match; pages a prefix; actions and containers matches inside.
    expect(got.map((x) => x.key)).toEqual(["disks", "pages", "actions", "containers"]);
    expect(got.find((x) => x.key === "pages")!.entries.map((e) => e.title)).toEqual(["Disks"]);
  });

  it("matches an entry by its ID as well as its title", () => {
    const got = matchSections("acme-web", [], [{ key: "projects", entries: [{ title: "Web", id2: "acme-web" }] }]);
    expect(got[0]!.entries).toHaveLength(1);
  });
});
