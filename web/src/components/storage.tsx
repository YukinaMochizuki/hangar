// Pieces the Disks and Images pages share: their states, a disk as YAML,
// and the disk dialogs (grow, delete), each the API's own call with
// its error shown where it was asked for.

import { toJson, type JsonValue } from "@bufbuild/protobuf";
import { Link } from "@tanstack/react-router";
import { DiskPhase, DiskSchema, type Disk } from "@hangar/api/hangar/storage/v1alpha1/disk_pb";
import type { Image } from "@hangar/api/hangar/storage/v1alpha1/image_pb";
import { HardDrive } from "lucide-react";
import { useState } from "react";
import { ConfirmDeleteDialog } from "@/components/delete-dialog";
import { Dialog } from "@/components/dialog";
import { FormEquivalent, FormField, Input } from "@/components/form";
import { Notice } from "@/components/notice";
import { Button, Led, type Tone } from "@/components/ui";
import { errorMessage } from "@/lib/api";
import { machinePath } from "@/lib/machines";
import { toYaml } from "@/lib/manifest";
import { formatNumber, quantityGiB } from "@/lib/quantity";
import {
  diskFault,
  diskImage,
  growFloorGiB,
  imageFloorGiB,
  type ImageState,
  madeAs,
  rootDiskOwner,
  usedAs,
  useDeleteDisk,
  useGrowDisk,
  useMachineOwners,
} from "@/lib/storage";

const diskPhases: Record<number, { label: string; tone: Tone; pulse?: boolean }> = {
  [DiskPhase.PENDING]: { label: "Pending", tone: "pending", pulse: true },
  [DiskPhase.AVAILABLE]: { label: "Available", tone: "off" },
  [DiskPhase.IN_USE]: { label: "In use", tone: "ok" },
};

/** diskPhaseLabel names a disk's phase, or Deleting. */
export function diskPhaseLabel(d: Disk): string {
  if (d.metadata?.deleteTime) return "Deleting";
  return diskPhases[d.status?.phase ?? 0]?.label ?? "Unknown";
}

/** DiskState shows a disk's phase, and the reason of its Ready condition when False. */
export function DiskState({ d, reason = true }: { d: Disk; reason?: boolean }) {
  const fault = diskFault(d);
  const p = fault ? { tone: "fail" as Tone } : (diskPhases[d.status?.phase ?? 0] ?? { tone: "off" as Tone });
  return (
    <>
      <span className="inline-flex items-center gap-2">
        <Led tone={p.tone} pulse={"pulse" in p && p.pulse} />
        {diskPhaseLabel(d)}
      </span>
      {reason && fault && <span className="block text-[11.5px] text-fail">{fault.reason}</span>}
    </>
  );
}

/** ImageStateCell shows an image's state, and the reason when it is at fault. */
export function ImageStateCell({ s }: { s: ImageState }) {
  return (
    <>
      <span className="inline-flex items-center gap-2">
        <Led tone={s.tone} />
        {s.label}
      </span>
      {s.reason && <span className="block text-[11.5px] text-fail">{s.reason}</span>}
    </>
  );
}

/**
 * MachineLink shows the machine a disk is attached to as its Container or VM,
 * linked to that page; the Machine's own name while the owner is not known.
 */
export function MachineLink({ project, machine }: { project: string; machine: string }) {
  const owner = useMachineOwners(project)(machine);
  if (!owner) return <span className="font-mono text-[12.5px]">{machine}</span>;
  return (
    <Link to={machinePath(owner.kind)} params={{ project, name: owner.name }} className="font-mono text-[12.5px] text-accent hover:underline">
      {owner.name}
    </Link>
  );
}

const roleWords = { boot: "Boot disk", data: "Data disk", volume: "Volume" } as const;

/** usedAsText is UsedAs as plain text, for a line too narrow for its link, such as a phone's list row. */
export function usedAsText(d: Disk): string | undefined {
  const u = usedAs(d);
  return u && `${roleWords[u.role]} of ${u.name}`;
}

/**
 * UsedAs says what a disk is used as now, from its status.attachment: a VM's
 * boot or data disk, or a container's volume and where it is mounted, linked
 * to the Container's or VM's page.
 */
export function UsedAs({ d, project }: { d: Disk; project: string }) {
  const u = usedAs(d);
  if (!u) return <span className="text-subtle">Not attached</span>;
  return (
    <span>
      {roleWords[u.role]}
      {u.role === "volume" && u.path && (
        <>
          {" "}
          at <span className="font-mono text-[12.5px]">{u.path}</span>
        </>
      )}{" "}
      of{" "}
      <Link to={machinePath(u.kind)} params={{ project, name: u.name }} className="font-mono text-[12.5px] text-accent hover:underline">
        {u.name}
      </Link>
      {(u.role !== "volume" ? u.device : u.readOnly) && (
        <span className="text-[11.5px] text-subtle"> · {u.role !== "volume" ? u.device : "read-only"}</span>
      )}
    </span>
  );
}

/** MadeAs says what a disk was made as, from the label the New disk page writes; the user's own, it restricts nothing. */
export function MadeAs({ d }: { d: Disk }) {
  const m = madeAs(d);
  return m ? <>{m === "boot" ? "Boot disk" : "Data disk"}</> : <span className="text-subtle">Not recorded</span>;
}

// The name a disk's machine goes by: its Container's or VM's, else its own.
function useMachineName(project: string, machine: string): string {
  return useMachineOwners(project)(machine)?.name ?? machine;
}

/** DiskSource is a disk's source: blank, or a copy of an image, linked. */
export function DiskSource({ d, project, link = true }: { d: Disk; project: string; link?: boolean }) {
  const img = diskImage(d);
  if (!img) return <span className="text-muted">Blank</span>;
  return (
    <span>
      <span className="text-muted">Image </span>
      {link ? (
        <Link to="/projects/$project/images/$name" params={{ project, name: img }} className="font-mono text-[12.5px] text-accent hover:underline">
          {img}
        </Link>
      ) : (
        <span className="font-mono">{img}</span>
      )}
    </span>
  );
}

/** diskYaml is a disk as `hangar apply -f` takes it. */
export function diskYaml(d: Disk): string {
  const json = toJson(DiskSchema, d) as { [k: string]: JsonValue };
  const meta = (json.metadata ?? {}) as { [k: string]: JsonValue };
  const metadata: { [k: string]: JsonValue } = { name: meta.name ?? "" };
  const labels = Object.entries((meta.labels ?? {}) as { [k: string]: JsonValue }).filter(([k]) => !/\.hangarlab\.cloud\//.test(k));
  if (labels.length) metadata.labels = Object.fromEntries(labels);
  // A blank disk's empty source is left out, as a person writes it.
  const spec = Object.fromEntries(
    Object.entries((json.spec ?? {}) as { [k: string]: JsonValue }).filter(([, v]) => !(v && typeof v === "object" && Object.keys(v).length === 0)),
  );
  return toYaml({ apiVersion: "storage.hangarlab.cloud/v1alpha1", kind: "Disk", metadata, spec });
}

/** growFacts says what growing a disk does: it restarts the machine the disk is attached to, if any. */
export function growFacts(machine: string | undefined): string | undefined {
  return machine ? `Growing it restarts ${machine}.` : undefined;
}

/** sizeFacts states a disk's spec.size and what its node holds. */
export function sizeFacts(d: Disk): string {
  const spec = quantityGiB(d.spec?.size);
  const held = quantityGiB(d.status?.size);
  if (!Number.isFinite(held)) return `It is not placed yet; spec.size is ${formatNumber(spec)} GiB.`;
  if (held !== spec) return `spec.size is ${formatNumber(spec)} GiB; its node holds ${formatNumber(held)} GiB.`;
  return `Its node holds ${formatNumber(held)} GiB.`;
}

/**
 * GrowDiskDialog sets a disk's size: larger to grow it, or back down to what
 * its node holds to withdraw a growth not yet granted. Applied at the
 * version read; a stale read applies nothing (lib/storage.ts).
 */
export function GrowDiskDialog({ d, image, open, onClose }: { d: Disk; image?: Image; open: boolean; onClose: () => void }) {
  const name = d.metadata?.name ?? "";
  const project = d.metadata?.project ?? "";
  const spec = quantityGiB(d.spec?.size);
  const held = quantityGiB(d.status?.size);
  const floor = growFloorGiB(d, image);
  const [size, setSize] = useState(spec * 2);
  const grow = useGrowDisk(project, name);
  const machine = useMachineName(project, d.status?.machine ?? "");
  const imageFloor = diskImage(d) ? imageFloorGiB(image) : 1;
  const problem = !Number.isInteger(size)
    ? "The size is not a whole number of GiB."
    : size === spec
      ? `${name} is already set to ${formatNumber(spec)} GiB.`
      : size >= floor
        ? undefined
        : Number.isFinite(held) && held >= imageFloor
          ? `Its node holds ${formatNumber(held)} GiB; a disk never shrinks below that.`
          : imageFloor > 1
            ? `${diskImage(d)} needs ${imageFloor} GiB; ${size} GiB is less.`
            : "The size is below 1 GiB.";
  const close = () => {
    grow.reset();
    onClose();
  };
  return (
    <Dialog open={open} onClose={close} title={`Grow disk ${name}`}>
      {d.status?.machine && <p className="mb-1">{growFacts(machine)}</p>}
      <p>{sizeFacts(d)}</p>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (!problem) grow.mutate({ sizeGi: size, shownGi: spec }, { onSuccess: close });
        }}
      >
        <div className="mt-4 text-foreground">
          <FormField label="Size, GiB" hint={`At least ${floor} GiB.`}>
            <Input type="number" min={floor} value={size} onChange={(e) => setSize(Number(e.target.value))} className="w-32" />
          </FormField>
        </div>
        <FormEquivalent yaml={diskYaml({ ...d, spec: { ...d.spec!, size: `${size}Gi` } })} />
        {problem && <Notice tone="warning" title={problem} className="mt-3" />}
        {grow.error && (
          <Notice tone="error" title="Changing the size failed" className="mt-3">
            {errorMessage(grow.error)}
          </Notice>
        )}
        <div className="mt-5 flex justify-end gap-2">
          <Button type="button" onClick={close}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabled={!!problem || grow.isPending}>
            <HardDrive /> {grow.isPending ? "Applying…" : size > spec ? `Grow to ${formatNumber(size)} GiB` : `Set to ${formatNumber(size)} GiB`}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

/**
 * diskDeleteFacts says what deleting a disk takes with it, and what the API
 * refuses: deleting a disk a live machine has. A disk whose machine is
 * being deleted goes once that machine is gone.
 */
export function diskDeleteFacts(d: Disk, machine: { name: string; deleting: boolean } | undefined): string {
  const name = d.metadata?.name ?? "";
  if (machine && !machine.deleting) return `${name} is attached to ${machine.name}, which is not being deleted.`;
  const owner = rootDiskOwner(d);
  const root = owner ? ` It is the root disk made for ${owner}.` : "";
  if (machine) return `Its data is deleted with it, once ${machine.name}, which is being deleted, is gone.${root}`;
  if (owner) return `Its data is deleted with it.${root}`;
  return "Its data is deleted with it.";
}

/** DeleteDiskDialog asks for the name typed to confirm, then sends DeleteDisk and shows its error. */
export function DeleteDiskDialog({ d, open, onClose, after }: { d: Disk; open: boolean; onClose: () => void; after?: () => void }) {
  const name = d.metadata?.name ?? "";
  const project = d.metadata?.project ?? "";
  const del = useDeleteDisk(project, name);
  const owner = useMachineOwners(project)(d.status?.machine ?? "");
  const machine = d.status?.machine ? { name: owner?.name ?? d.status.machine, deleting: owner?.deleting ?? false } : undefined;
  return (
    <ConfirmDeleteDialog
      open={open}
      onClose={onClose}
      title={`Delete disk ${name}`}
      name={name}
      consequence={diskDeleteFacts(d, machine)}
      cli={`hangar delete disk ${name} -p ${project}`}
      del={del}
      after={after}
    />
  );
}
