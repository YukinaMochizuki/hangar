// What a machine's Runs on panel says, from what the console already reads:
// the container's or VM's placement and run state, and its Machine's
// Scheduled condition (docs/decisions/machine-placement.md).

import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import type { MachineView } from "@/lib/machines";
import { hostFor, type HostInfo } from "@/lib/placement";

export type RunsOnState =
  /** Asked to run, not placed yet, and the scheduler has not said it cannot place it. */
  | { kind: "scheduling" }
  /** The scheduler found no node for it, and says why. */
  | { kind: "unschedulable"; message: string }
  /** Held back before it can be scheduled, such as by a file share on no node yet, and why. */
  | { kind: "heldBack"; message: string }
  /** Asked to stay stopped, and never placed. */
  | { kind: "stopped" }
  /** On a host, as its placement describes it. */
  | { kind: "placed"; host: HostInfo };

/**
 * The panel's state. A placement wins: a machine on a host shows it, whatever
 * else holds. The scheduler's word is its Machine's Scheduled condition,
 * False when it finds no node (reason NoNodeAvailable, with why as its
 * message); the container or VM repeats it in its Ready condition, with the
 * reason Unschedulable, which counts when the Machine is not known. A
 * container's Ready reason MachineHeldBack, such as for a file share on no
 * node yet, holds it back before the scheduler sees it.
 */
export function runsOnState(m: Pick<MachineView, "container" | "vm" | "machine" | "node" | "stopped">): RunsOnState {
  const host = hostFor(m);
  if (host) return { kind: "placed", host };
  if (m.stopped) return { kind: "stopped" };
  const ready = (m.container?.status?.conditions ?? m.vm?.status?.conditions ?? []).find((c) => c.type === "Ready");
  if (m.container && ready?.status === ConditionStatus.FALSE && ready.reason === "MachineHeldBack") {
    return { kind: "heldBack", message: ready.message.replace(/^failed_precondition: /, "") };
  }
  const scheduled = m.machine?.status?.conditions.find((c) => c.type === "Scheduled");
  if (scheduled?.status === ConditionStatus.FALSE) return { kind: "unschedulable", message: scheduled.message };
  if (!m.machine && ready?.reason === "Unschedulable") return { kind: "unschedulable", message: ready.message };
  return { kind: "scheduling" };
}
