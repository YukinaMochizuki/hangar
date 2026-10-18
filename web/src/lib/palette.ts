// What the command palette finds and in which order: a query matches an
// entry's name (its ID) or its label, case-insensitively, and an exact match
// ranks above a prefix, a prefix above a match anywhere.

/** How well an entry matches a query; higher is better, 0 is no match. */
export enum Rank {
  None = 0,
  Contains = 1,
  Prefix = 2,
  Exact = 3,
}

/** The rank of the best of the texts against the query; an empty query matches everything at Contains. */
export function rank(query: string, ...texts: (string | undefined)[]): Rank {
  const q = query.trim().toLowerCase();
  if (!q) return Rank.Contains;
  let best = Rank.None;
  for (const t of texts) {
    if (!t) continue;
    const s = t.toLowerCase();
    const r = s === q ? Rank.Exact : s.startsWith(q) ? Rank.Prefix : s.includes(q) ? Rank.Contains : Rank.None;
    if (r > best) best = r;
  }
  return best;
}

/** Where the query first occurs in text, for marking it; undefined when it does not. */
export function matchAt(query: string, text: string): [number, number] | undefined {
  const q = query.trim().toLowerCase();
  if (!q) return undefined;
  const i = text.toLowerCase().indexOf(q);
  return i < 0 ? undefined : [i, i + q.length];
}

/** The entries that match, best first; equal ranks keep the order given. texts says what of an entry is matched. */
export function filterRanked<T>(query: string, entries: T[], texts: (e: T) => (string | undefined)[]): { entry: T; rank: Rank }[] {
  return entries
    .map((entry, i) => ({ entry, rank: rank(query, ...texts(entry)), i }))
    .filter((x) => x.rank !== Rank.None)
    .sort((a, b) => b.rank - a.rank || a.i - b.i)
    .map(({ entry, rank }) => ({ entry, rank }));
}

type Matchable = { title: string; id2?: string };

/**
 * The sections that match the query, each sorted best first, and the section
 * with the best match first; equal sections keep their order. With no query,
 * the sections always shown, as they are.
 */
export function matchSections<E extends Matchable, S extends { entries: E[] }>(query: string, always: S[], searched: S[]): S[] {
  if (!query.trim()) return always;
  return [...always, ...searched]
    .map((s, i) => {
      const hits = filterRanked(query, s.entries, (e) => [e.title, e.id2]);
      return { s: { ...s, entries: hits.map((h) => h.entry) }, best: hits[0]?.rank ?? Rank.None, i };
    })
    .filter((x) => x.best !== Rank.None)
    .sort((a, b) => b.best - a.best || a.i - b.i)
    .map((x) => x.s);
}
