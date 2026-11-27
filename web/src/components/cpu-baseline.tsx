// A burstable machine's mark (docs/decisions/cpu-credits.md): "Burstable 20%"
// beside its size, in its page's head and its list's row.

import { Badge } from "@/components/ui";
import type { MachineView } from "@/lib/machines";

/** "Burstable 20%" beside a machine's size, as "Shared" is for its memory; nothing for a Standard one. */
export function BurstableBadge({ m }: { m: Pick<MachineView, "baseline"> }) {
  return m.baseline === 100 ? null : <Badge>Burstable {m.baseline}%</Badge>;
}
