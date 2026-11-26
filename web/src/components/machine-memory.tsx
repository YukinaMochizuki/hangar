// A machine's memory in its tenant's pool, on its page and its list's row
// (docs/decisions/memory-pool.md): Shared marked beside its size, what it
// keeps reserved and what its pool leaves it now, and what its node says:
// that its VM was killed for its pool's memory, or that its balloon does
// not follow.

import { MachinePhase } from "@hangar/api/hangar/compute/v1alpha1/machine_pb";
import { PageBanner } from "@/components/notice";
import { Badge } from "@/components/ui";
import { ago, type MachineView } from "@/lib/machines";
import { balloonStuck, exactGiB, killedRecently, machineMemory, outsidePool, useProjectPool } from "@/lib/memory-pool";
import { useProjects } from "@/lib/projects";
import { formatNumber } from "@/lib/quantity";
import { useMachineUsage } from "@/lib/usage";

/** "Shared" beside a machine's size, for one that keeps less than its memory reserved. */
export function SharedBadge({ m }: { m: MachineView }) {
  return machineMemory(m).shared ? <Badge>Shared</Badge> : null;
}

const running = (m: MachineView) => !m.stopped && m.phase === MachinePhase.RUNNING;

/**
 * The Summary's Memory row of a Shared machine: its limit, what its pool
 * leaves it now while it runs and its node can tell, and what it keeps
 * reserved; then what its node says of its memory.
 */
export function MemoryRow({ m }: { m: MachineView }) {
  const mem = machineMemory(m);
  const stuck = balloonStuck(m);
  // The page's Usage card asks the same, so this is one ask.
  const { data } = useMachineUsage(m.project, m.kind, m.name, running(m));
  const nowBytes = running(m) && !stuck ? data?.sizeNowBytes : undefined;
  const killed = killedRecently(m);
  const pool = useProjectPool(m.project);
  return (
    <>
      <span className="tabular">
        Shared · {formatNumber(mem.memoryGi)} GiB limit
        {nowBytes !== undefined && <> · {formatNumber(nowBytes / 2 ** 30)} GiB available now</>} · {formatNumber(mem.reservedGi)} GiB reserved
      </span>
      {killed && (
        <div className="mt-1 text-[12px] text-fail" role="status">
          Stopped for memory {ago(killed)} ago{running(m) && <span className="text-muted"> and started again.</span>}
        </div>
      )}
      {stuck && (
        <div className="mt-1 text-[12px] text-pending" role="status">
          Not giving memory back
          {stuck.lastTransitionTime && <span className="text-muted">: its guest has not answered requests for memory for {ago(new Date(Number(stuck.lastTransitionTime.seconds) * 1000))}.</span>}
        </div>
      )}
      {pool.pooled && outsidePool(m) && <div className="mt-1 text-[12px] text-muted">Shares memory from its next restart.</div>}
    </>
  );
}

/** The tenant a pooled machine's memory comes from, as people read it, and the plan's memory, from its Machine's pool. */
function usePoolOf(m: MachineView) {
  const { tenantName } = useProjects();
  const p = m.machine?.spec?.memoryPool;
  return { tenant: p ? tenantName(p.name) : "The tenant", planGi: p ? exactGiB(p.size) : undefined };
}

/**
 * The page's banners on a pooled machine's memory: its VM killed for its
 * pool's memory within the last day, unless its Failed banner already says
 * so; and its balloon not following, while so.
 */
export function MemoryBanners({ m }: { m: MachineView }) {
  const { tenant, planGi } = usePoolOf(m);
  const killed = killedRecently(m);
  const stuck = balloonStuck(m);
  const failedForMemory = m.phase === MachinePhase.FAILED;
  return (
    <>
      {killed && !failedForMemory && (
        <PageBanner tone="error" title={`${m.name} was stopped for memory ${ago(killed)} ago`}>
          <span className="block">
            {tenant}'s running machines needed more memory than the plan's {planGi !== undefined ? `${formatNumber(planGi)} GiB` : "memory"}, and a Shared machine is stopped first.
            {running(m) && " It started again."}
          </span>
        </PageBanner>
      )}
      {stuck && !m.stopped && (
        <PageBanner tone="warning" title={`${m.name} is not giving memory back`}>
          <span className="block">
            Its guest stopped answering requests for memory, so the {formatNumber(machineMemory(m).memoryGi)} GiB it may hold cannot go to {tenant}'s other machines. Restarting it asks again.
          </span>
        </PageBanner>
      )}
    </>
  );
}

/** A list row's line under its state: killed for memory within the last day, or its balloon not following. */
export function MemoryStateLine({ m }: { m: MachineView }) {
  const killed = killedRecently(m);
  if (killed && m.phase !== MachinePhase.FAILED) return <span className="block text-[11.5px] text-fail">Stopped for memory {ago(killed)} ago</span>;
  if (balloonStuck(m) && !m.stopped) return <span className="block text-[11.5px] text-pending">Not giving memory back</span>;
  return null;
}
