// Kubernetes-style quantities, as the API writes cpu, memory and disk:
// "2", "2500m", "512Mi", "16Gi".

const binary: Record<string, number> = { Ki: 2 ** 10, Mi: 2 ** 20, Gi: 2 ** 30, Ti: 2 ** 40, Pi: 2 ** 50 };
const decimal: Record<string, number> = { m: 1e-3, k: 1e3, M: 1e6, G: 1e9, T: 1e12, P: 1e15 };

/** The quantity's value in base units (cores, bytes); NaN when it is not one. */
export function parseQuantity(q: string | undefined): number {
  if (!q) return NaN;
  const m = q.trim().match(/^([0-9]+(?:\.[0-9]+)?)([a-zA-Z]*)$/);
  if (!m) return NaN;
  const n = Number(m[1]);
  const unit = m[2];
  if (unit === "") return n;
  if (unit in binary) return n * binary[unit];
  if (unit in decimal) return n * decimal[unit];
  return NaN;
}

const GiB = 2 ** 30;

/** Bytes as GiB, rounded to at most one decimal. */
export function toGiB(bytes: number): number {
  return Math.round((bytes / GiB) * 10) / 10;
}

/** A memory or disk quantity in GiB; NaN when there is none. */
export function quantityGiB(q: string | undefined): number {
  return toGiB(parseQuantity(q));
}

/** A cpu quantity in whole or fractional vCPUs. */
export function quantityCPU(q: string | undefined): number {
  return parseQuantity(q);
}

/** A number for display: integers as they are, others to one decimal. */
export function formatNumber(n: number): string {
  if (!Number.isFinite(n)) return "–";
  return Number.isInteger(n) ? String(n) : n.toFixed(1);
}
