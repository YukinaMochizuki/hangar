// The sizes machines are offered in. The platform's administrator defines
// them as MachineSize objects; the console only reads them, and has none of
// its own (docs/decisions/machine-sizes.md). A size is a preset: a machine
// states its own cpu, memory and disk, records no size, and may have any
// shape the shape rules below allow.

import { useQuery } from "@connectrpc/connect-query";
import { MachineSizeService, type MachineSize } from "@hangar/api/hangar/compute/v1alpha1/machine_size_pb";
import { quantityCPU, quantityGiB } from "@/lib/quantity";

export type Size = {
  /** The MachineSize's name, which `?size=` takes. */
  name: string;
  /** Its display name. */
  label: string;
  order: number;
  cpu: number;
  memoryGi: number;
  diskGi: number;
  /** The quantities as the size writes them, which a machine is sent. */
  resources: { cpu: string; memory: string; disk: string };
};

// The API's largest page: a platform defines far fewer sizes than this.
const pageSize = 1000;

// Sizes change rarely, and only by the platform's administrator.
const staleTime = 60_000;

export function sizeView(ms: MachineSize): Size {
  const spec = ms.spec;
  const name = ms.metadata?.name ?? "";
  return {
    name,
    label: spec?.displayName || name,
    order: spec?.order ?? 0,
    cpu: quantityCPU(spec?.cpu),
    memoryGi: quantityGiB(spec?.memory),
    diskGi: quantityGiB(spec?.disk),
    resources: { cpu: spec?.cpu ?? "", memory: spec?.memory ?? "", disk: spec?.disk ?? "" },
  };
}

/** Sizes by their order, smallest first, and by name where orders tie. */
export function sortSizes(items: MachineSize[]): Size[] {
  return items.map(sizeView).sort((a, b) => a.order - b.order || a.name.localeCompare(b.name));
}

/** The platform's machine sizes, sorted by order. */
export function useMachineSizes() {
  const q = useQuery(MachineSizeService.method.listMachineSizes, { pageSize }, { staleTime });
  return {
    sizes: q.data ? sortSizes(q.data.items) : [],
    loading: q.isPending,
    error: q.error,
    refetch: () => void q.refetch(),
  };
}

// The shape rules, as the apiserver checks them (checkShape in
// internal/apiserver/machinesizes.go): memory in 256 MiB steps, 512 MiB to
// 8 GiB of it per vCPU though one vCPU is always allowed, and a root disk of
// at least 2 GiB.
export const memoryStepMiB = 256;
export const minDiskGi = 2;
export const minMiBPerCPU = 512;
export const maxMiBPerCPU = 8192;

/** The fewest and most vCPUs a machine of memMiB may have. */
export function cpuRange(memMiB: number): { lo: number; hi: number } {
  return { lo: Math.max(1, Math.ceil(memMiB / maxMiBPerCPU)), hi: Math.max(1, Math.floor(memMiB / minMiBPerCPU)) };
}

/** Memory as a person reads it, in GiB as the form takes it: 0.25 GiB, 1.5 GiB. */
export function memoryLabel(memMiB: number): string {
  return `${+(memMiB / 1024).toFixed(2)} GiB`;
}

/** Memory as a machine is sent it: whole Gi in Gi, else Mi. */
export function memoryQuantity(memMiB: number): string {
  return memMiB % 1024 === 0 ? `${memMiB / 1024}Gi` : `${memMiB}Mi`;
}

/** Why cpu vCPUs and memMiB of memory break the shape rules, or undefined. */
export function shapeProblem(cpu: number, memMiB: number): string | undefined {
  if (!Number.isInteger(memMiB) || memMiB < memoryStepMiB || memMiB % memoryStepMiB !== 0) {
    return `Memory is a whole number of ${memoryStepMiB} MiB steps, at least one.`;
  }
  if (!Number.isInteger(cpu) || cpu < 1) return "The vCPUs are a whole number, at least 1.";
  const { lo, hi } = cpuRange(memMiB);
  if (cpu < lo || cpu > hi) {
    const span = lo === hi ? `${lo} vCPU${lo === 1 ? "" : "s"}` : `${lo} to ${hi} vCPUs`;
    return `${memoryLabel(memMiB)} of memory goes with ${span}, not ${cpu}.`;
  }
  return undefined;
}

/**
 * Whether a size keeps to the shape rules. The apiserver refuses a size that
 * does not, but one stored before the rules may: it is not offered, as a
 * machine of it would be refused, though lists still name machines by it.
 */
export function withinRules(z: Size): boolean {
  return shapeProblem(z.cpu, z.memoryGi * 1024) === undefined && z.diskGi >= minDiskGi;
}

// The memory a Custom shape's slider stops at, in GiB: the shapes people
// ask for, denser where a step matters more.
const commonGi = [0.25, 0.5, 1, 1.5, 2, 3, 4, 6, 8, 12, 16, 24, 32, 48, 64, 96, 128, 192, 256];

// The slider's end with no size to go by.
const fallbackMaxGi = 16;

/** The memory slider's end and the vCPU slider's: twice the largest listed size's, but never less than what is asked to fit. */
export function sliderLimits(sizes: Size[], fit: { memMiB: number; cpu: number }[] = []): { memMiB: number; cpu: number } {
  const memGi = sizes.length > 0 ? 2 * Math.max(...sizes.map((z) => z.memoryGi)) : fallbackMaxGi;
  const cpu = sizes.length > 0 ? 2 * Math.max(...sizes.map((z) => z.cpu)) : 2 * cpuRange(memGi * 1024).lo;
  const memMiB = Math.max(memGi * 1024, ...fit.map((f) => f.memMiB));
  return { memMiB, cpu: Math.max(cpu, cpuRange(memMiB).lo, ...fit.map((f) => f.cpu)) };
}

/**
 * The memory slider's stops, in MiB, smallest first: the common shapes up to
 * maxMiB, and each of extra (a machine's own memory, off the common ones)
 * that keeps to the rules.
 */
export function memoryStops(maxMiB: number, extra: number[] = []): number[] {
  const own = extra.filter((m) => Number.isInteger(m) && m >= memoryStepMiB && m % memoryStepMiB === 0);
  return [...new Set([...commonGi.map((g) => g * 1024).filter((m) => m <= maxMiB), ...own])].sort((a, b) => a - b);
}

/** cpu moved into the range memMiB allows. */
export function clampCPU(cpu: number, memMiB: number): number {
  const { lo, hi } = cpuRange(memMiB);
  return Math.min(Math.max(Math.round(cpu) || lo, lo), hi);
}

/** The listed size whose cpu and memory a machine has, if any. */
export function sizeOf(m: { cpu: number; memoryGi: number }, sizes: Size[]): Size | undefined {
  return sizes.find((s) => s.cpu === m.cpu && s.memoryGi === m.memoryGi);
}
