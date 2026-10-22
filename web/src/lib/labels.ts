// An object's labels as the Edit labels dialog edits them: rows of key and
// value, checked as the apiserver checks them (checkMetaKeys in
// internal/apiserver/admission.go), and the labels they make.

/** The API's domain: a key whose prefix is it, or a subdomain of it, is the system's. */
export const systemDomain = "hangarlab.cloud";

/** The longest key and value the API takes, in bytes. */
export const maxKeyBytes = 317;
export const maxValueBytes = 63;

/** One row of the dialog. A row the system set is shown and sent back unchanged. */
export type LabelRow = { id: number; key: string; value: string; system?: boolean };

const bytes = (s: string) => new TextEncoder().encode(s).length;

/** Whether a key's prefix is the system's, as the apiserver's reservedKey says. */
export function systemKey(key: string): boolean {
  const slash = key.indexOf("/");
  if (slash < 0) return false;
  const prefix = key.slice(0, slash);
  return prefix === systemDomain || prefix.endsWith(`.${systemDomain}`);
}

/** The rows an object's labels start the dialog with, the system's first, each in key order. */
export function rowsOf(labels: Record<string, string>): LabelRow[] {
  const entries = Object.entries(labels).sort(([a], [b]) => a.localeCompare(b));
  const system = entries.filter(([k]) => systemKey(k));
  const own = entries.filter(([k]) => !systemKey(k));
  return [...system.map(([key, value]) => ({ key, value, system: true })), ...own.map(([key, value]) => ({ key, value }))].map((r, id) => ({ ...r, id }));
}

/** A row left wholly empty, which is no label. */
const blank = (r: LabelRow) => r.key === "" && r.value === "";

/** What is wrong with a row, by field, as the API would refuse it; nothing for a row it takes. */
export type RowProblem = { key?: string; value?: string };

/**
 * The problems of each row, by its id: an empty or too long key, a value
 * too long, a key under the system's prefix that the object does not
 * already hold with that value, and a key given twice.
 */
export function rowProblems(rows: LabelRow[], stored: Record<string, string>): Map<number, RowProblem> {
  const out = new Map<number, RowProblem>();
  const seen = new Set<string>();
  for (const r of rows) {
    if (r.system || blank(r)) {
      if (r.system) seen.add(r.key);
      continue;
    }
    const p: RowProblem = {};
    if (r.key === "") p.key = "A label needs a key.";
    else if (bytes(r.key) > maxKeyBytes) p.key = `A key is at most ${maxKeyBytes} bytes.`;
    else if (systemKey(r.key) && stored[r.key] !== r.value) p.key = `Keys under ${systemDomain}/ are the system's.`;
    else if (seen.has(r.key)) p.key = "This key is given above.";
    if (bytes(r.value) > maxValueBytes) p.value = `A value is at most ${maxValueBytes} bytes.`;
    seen.add(r.key);
    if (p.key || p.value) out.set(r.id, p);
  }
  return out;
}

/** The labels the rows make; empty rows are none. */
export function labelsOf(rows: LabelRow[]): Record<string, string> {
  return Object.fromEntries(rows.filter((r) => !blank(r)).map((r) => [r.key, r.value]));
}

/** What applying the labels does to the object's: each key added, changed or removed. */
export function labelChanges(from: Record<string, string>, to: Record<string, string>) {
  const added = Object.keys(to).filter((k) => !(k in from));
  const changed = Object.keys(to).filter((k) => k in from && from[k] !== to[k]);
  const removed = Object.keys(from).filter((k) => !(k in to));
  return { added, changed, removed, none: added.length + changed.length + removed.length === 0 };
}
