import { ChassisArt, type ArtStatus } from "@/components/chassis-art";
import type { Chassis } from "@/mock/data";

const models: { chassis: Chassis; label: string; status: ArtStatus }[] = [
  { chassis: "1u-2u2", label: "1U · 2 × U.2 NVMe", status: "running" },
  { chassis: "storage-2u", label: "2U · 12-bay storage", status: "pending" },
  { chassis: "mini", label: "Mini PC", status: "failed" },
  { chassis: "unknown", label: "Unknown model (fallback)", status: "stopped" },
];

/** Every chassis side by side, to compare the drawings. Proof of concept only. */
export function ArtSheet() {
  return (
    <div className="grid grid-cols-2 gap-6 p-8">
      {models.map((m) => (
        <figure key={m.chassis} className="rounded-lg border bg-surface p-6">
          <ChassisArt chassis={m.chassis} status={m.status} className="mx-auto max-h-[28rem]" />
          <figcaption className="mt-4 flex justify-between text-sm text-muted">
            <span>{m.label}</span>
            <span className="font-mono text-xs">{m.status}</span>
          </figcaption>
        </figure>
      ))}
    </div>
  );
}
