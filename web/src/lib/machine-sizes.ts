// The sizes machines are offered in. The platform's administrator defines
// them as MachineSize objects; the console only reads them, and has none of
// its own (docs/decisions/machine-sizes.md). A size is a template: a machine
// states its own cpu, memory and disk, and records no size.

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

/** The listed size whose cpu and memory a machine has, if any. */
export function sizeOf(m: { cpu: number; memoryGi: number }, sizes: Size[]): Size | undefined {
  return sizes.find((s) => s.cpu === m.cpu && s.memoryGi === m.memoryGi);
}
