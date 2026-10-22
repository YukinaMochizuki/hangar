// A VM's data disks: the project's disks attached after its boot disk, in
// order (spec.disks). A disk another machine uses, or one that is a root
// disk, is listed but cannot be chosen.

import { HardDrive, Plus, X } from "lucide-react";
import { Select } from "@/components/form";
import { usedAsText } from "@/components/storage";
import { Button } from "@/components/ui";
import { gib, rootDiskOwner, useDisks, usedAs } from "@/lib/storage";

/** What stops the data disks being sent, each a sentence. */
export function dataDiskProblems(rows: string[]): string[] {
  const out: string[] = [];
  rows.forEach((r, i) => {
    if (!r) out.push(`Data disk ${i + 1} has no disk.`);
  });
  const named = rows.filter(Boolean);
  if (new Set(named).size < named.length) out.push("A disk is attached twice.");
  return out;
}

export function VmDisks({ project, rows, onChange, vm }: { project: string; rows: string[]; onChange: (rows: string[]) => void; vm?: string }) {
  const disks = useDisks(project);
  return (
    <div className="space-y-2">
      {rows.map((r, i) => (
        <div key={i} className="flex items-center gap-2">
          <HardDrive className="size-4 shrink-0 text-muted" strokeWidth={1.75} />
          <Select value={r} onChange={(e) => onChange(rows.map((x, j) => (j === i ? e.target.value : x)))} aria-label={`Data disk ${i + 1}`} className="h-8">
            <option value="">{disks.loading ? "Loading…" : "Choose a disk"}</option>
            {disks.items.map((d) => {
              const name = d.metadata?.name ?? "";
              const owner = rootDiskOwner(d);
              // A data disk this VM holds already stays choosable; a root disk, or a disk another machine holds, does not.
              const u = usedAs(d);
              const used = u && !(vm && u.kind === "VirtualMachine" && u.name === vm && u.role === "data") ? usedAsText(d) : undefined;
              const taken = !!owner || !!used || (rows.includes(name) && r !== name);
              return (
                <option key={name} value={name} disabled={taken}>
                  {name} · {gib(d.status?.size || d.spec?.size)}
                  {used ? ` · ${used}` : owner ? ` · root disk of ${owner}` : ""}
                </option>
              );
            })}
          </Select>
          <Button type="button" variant="ghost" size="icon" aria-label={`Detach data disk ${i + 1}`} onClick={() => onChange(rows.filter((_, j) => j !== i))}>
            <X />
          </Button>
        </div>
      ))}
      <Button type="button" size="sm" onClick={() => onChange([...rows, ""])}>
        <Plus /> Attach disk
      </Button>
    </div>
  );
}
