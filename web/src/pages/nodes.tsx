// The platform's Nodes for its administrators: every host in a list, and one
// host's page with what it is (its placement, drawn as its chassis), how it
// stands (its conditions, and its host and services as Prometheus reads
// them), and its alerts and charts. Read only: a Node is system-managed, so
// no one cordons it here (docs/decisions/apply-semantics.md). Machines on a
// node are counted, never named (docs/decisions/monitoring.md).

import { Link } from "@tanstack/react-router";
import { useState, type ReactNode } from "react";
import { ChassisArt, type ArtStatus } from "@/components/chassis-art";
import { AlertList, Chart, hostCharts, LayerCard, ranges, SectionHead, Segmented } from "@/components/monitoring";
import { Notice } from "@/components/notice";
import { ConditionsCard } from "@/components/object";
import { DetailHead, DetailSkeleton, LoadingTable, PageHead } from "@/components/page";
import { Topbar } from "@/components/shell";
import { EmptyState, ErrorNotice, SkeletonRows } from "@/components/states";
import { tbodyRow, Th, theadRow } from "@/components/table";
import { Badge, Card, CardHeader, Field, StateText, type Tone } from "@/components/ui";
import { errorMessage } from "@/lib/api";
import { cn } from "@/lib/cn";
import { ago } from "@/lib/machines";
import { isUnavailable, monitoringPermission, useMonitoringOverview, type SeriesRange } from "@/lib/monitoring";
import { capacities, formatAmount, nodePerms, useMachinesPerNode, useNodes, type Capacity, type NodeView, type Readiness as ReadinessKind, type Stopped } from "@/lib/nodes";
import { usePlatformCan } from "@/lib/permissions";

const readyTone: Record<ReadinessKind, Tone> = { ready: "ok", notReady: "fail", unknown: "pending" };
const readyWord: Record<ReadinessKind, string> = { ready: "Ready", notReady: "Not ready", unknown: "Not reporting" };
const artStatus: Record<ReadinessKind, ArtStatus> = { ready: "running", notReady: "failed", unknown: "pending" };

/** Whether the node reports, and why not when it does not. */
export function Readiness({ n }: { n: NodeView }) {
  return (
    <StateText tone={readyTone[n.readiness]} pulse={n.readiness === "unknown"} label={readyWord[n.readiness]} reason={n.reason} reasonAt="beside" className="flex" />
  );
}

export function Cordoned() {
  return <Badge className="border-pending/40 text-pending">Cordoned</Badge>;
}

/**
 * One resource of a host, read at a glance: how much the host has, in bold
 * above; a bar of it, the part the host keeps for itself in a light tone
 * and what the machines placed on it hold in a dark one; and what is left
 * for another machine under it, or for vCPUs, which are shared, how many
 * are assigned. What machines hold turns the pending colour from 80% of
 * what the host offers them, as a quota's bar does. Wide, as on a node's
 * page, it also says what the host keeps.
 */
function CapacityCell({ c, wide }: { c: Capacity; wide?: boolean }) {
  if (!Number.isFinite(c.total) || c.total <= 0) return <span className="text-subtle">not reported</span>;
  const room = c.total - c.reserved;
  const share = c.held === undefined || room <= 0 ? undefined : c.held / room;
  const cpu = c.kind === "cpu";
  // vCPUs may be assigned past what is offered; that is shared, not a fault.
  const tone = share === undefined ? "" : !cpu && share >= 1 ? "bg-fail" : share >= 0.8 ? "bg-pending" : "bg-foreground/70";
  const width = (v: number) => `${(Math.min(Math.max(v, 0), c.total) / c.total) * 100}%`;
  return (
    <div className={wide ? "w-full" : "w-28"}>
      <div className="flex items-baseline justify-between gap-2">
        <span className="font-medium tabular">{formatAmount(c.kind, c.total)}</span>
        {share !== undefined && (
          <span className="text-[11.5px] text-subtle tabular">
            {Math.round(share * 100)}% {cpu ? "assigned" : "held"}
          </span>
        )}
      </div>
      <div className="mt-1.5 flex h-1 overflow-hidden rounded-full bg-surface-2">
        {c.reserved > 0 && <div className="h-full bg-border-strong" style={{ width: width(c.reserved) }} />}
        {c.held !== undefined && <div className={cn("h-full", tone)} style={{ width: width(Math.min(c.held, room)) }} />}
      </div>
      <div className="mt-1 text-[11.5px] text-muted tabular">
        {c.held === undefined ? (
          <span className="text-subtle">machines not known</span>
        ) : cpu ? (
          `${formatAmount(c.kind, c.held)} assigned`
        ) : (
          `${formatAmount(c.kind, c.free ?? 0)} free`
        )}
        {wide && c.reserved > 0 && <span className="text-subtle"> · {formatAmount(c.kind, c.reserved)} kept for the host</span>}
      </div>
    </div>
  );
}

const capacityLabel: Record<Capacity["kind"], string> = { cpu: "vCPU", memory: "Memory", disk: "Disk" };

/** What a Nodes page may read beside the Nodes: the monitoring, which counts what is placed on each. */
type May = { monitoring: boolean };

/**
 * A platform page that needs compute.nodes.list: nothing while unknown, the
 * refusal or the failure. What else it may read is asked in the same call,
 * and handed to the page, which asks only for what it may.
 */
function Gate({ title, crumbs, children }: { title: string; crumbs: ReactNode[]; children: (may: May) => ReactNode }) {
  const p = usePlatformCan([nodePerms.list, monitoringPermission]);
  const top = <Topbar platform crumbs={crumbs} />;
  if (!p.settled) return top;
  if (p.error) {
    return (
      <>
        {top}
        <PageHead title={title} description="Whether this account may read the platform's nodes is not known." />
        <div className="px-4 md:px-8">
          <Card>
            <ErrorNotice error={p.error} title="TestPermissions failed" />
          </Card>
        </div>
      </>
    );
  }
  if (!p.can(nodePerms.list)) {
    return (
      <>
        {top}
        <PageHead title={title} description="This account's roles do not include reading the platform's nodes." />
      </>
    );
  }
  return (
    <>
      {top}
      {children({ monitoring: p.can(monitoringPermission) })}
    </>
  );
}

/** Every Node, by name, each row opening the node's page. */
export function NodeList() {
  return (
    <Gate title="Nodes" crumbs={["Platform", "Nodes"]}>
      {(may) => <NodeListBody may={may} />}
    </Gate>
  );
}

function NodeListBody({ may }: { may: May }) {
  const nodes = useNodes();
  const machines = useMachinesPerNode(may.monitoring);
  return (
    <>
      <PageHead title="Nodes" description="Hosts that run machines" />
      <div className="px-4 pb-8 md:px-8">
        {nodes.loading ? (
          <LoadingTable />
        ) : nodes.error ? (
          <Card>
            <ErrorNotice error={nodes.error} title="The nodes did not load" onRetry={nodes.refetch} />
          </Card>
        ) : nodes.items.length === 0 ? (
          <Card>
            <EmptyState title="No nodes yet" />
          </Card>
        ) : (
          <Card className="overflow-x-auto">
            <table className="w-full min-w-[860px] text-[13px]">
              <thead>
                <tr className={theadRow}>
                  <Th>Node</Th>
                  <Th>State</Th>
                  <Th>vCPU</Th>
                  <Th>Memory</Th>
                  <Th>Disk</Th>
                  <Th>Machines</Th>
                  <Th className="whitespace-nowrap">Last report</Th>
                </tr>
              </thead>
              <tbody>
                {nodes.items.map((n) => (
                  <tr key={n.name} className={tbodyRow}>
                    <td className="px-4 py-2.5">
                      <Link to="/platform/nodes/$node" params={{ node: n.name }} className="block whitespace-nowrap">
                        <span className="block font-mono font-medium group-hover:text-accent">{n.name}</span>
                        <span className="block text-[11.5px] text-subtle">
                          {[n.host?.cpuModel, [n.host?.region, n.host?.site].filter(Boolean).join(" · ")].filter(Boolean).join(" — ") || "hardware not reported"}
                        </span>
                      </Link>
                    </td>
                    <td className="whitespace-nowrap px-4 py-2.5">
                      <span className="flex items-center gap-2">
                        <Readiness n={n} />
                        {n.cordoned && <Cordoned />}
                      </span>
                    </td>
                    {capacities(n, machines.held(n.name)).map((c) => (
                      <td key={c.kind} className="whitespace-nowrap px-3 py-3">
                        <CapacityCell c={c} />
                      </td>
                    ))}
                    <td className="whitespace-nowrap px-4 py-2.5 tabular text-muted">
                      {machines.count(n.name) ?? "—"}
                      <StoppedCount stopped={machines.stopped(n.name)} />
                    </td>
                    <td className="whitespace-nowrap px-4 py-2.5 text-muted">{n.heartbeat ? `${ago(n.heartbeat)} ago` : <span className="text-subtle">never</span>}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </Card>
        )}
      </div>
    </>
  );
}

/** One Node's page. */
export function NodeDetail({ name, range }: { name: string; range?: SeriesRange }) {
  return (
    <Gate title={name} crumbs={["Platform", <Link key="n" to="/platform/nodes">Nodes</Link>, name]}>
      {(may) => <NodeDetailBody name={name} initial={range ?? "24h"} may={may} />}
    </Gate>
  );
}

function NodeDetailBody({ name, initial, may }: { name: string; initial: SeriesRange; may: May }) {
  const nodes = useNodes();
  const n = nodes.items.find((x) => x.name === name);
  if (nodes.loading) return <DetailSkeleton />;
  if (nodes.error) {
    return (
      <div className="px-4 py-6 md:px-8">
        <Card>
          <ErrorNotice error={nodes.error} title="The node did not load" onRetry={nodes.refetch} />
        </Card>
      </div>
    );
  }
  if (!n) {
    return (
      <div className="px-4 md:px-8">
        <EmptyState title="No such node">
          The platform has no node named <span className="font-mono">{name}</span>.
        </EmptyState>
      </div>
    );
  }
  return <NodePage n={n} initial={initial} may={may} />;
}

/** How many of a node's machines it has stopped, after their count, when any. */
function StoppedCount({ stopped }: { stopped: Stopped | undefined }) {
  if (!stopped?.count) return null;
  return <span className="text-subtle"> · {stopped.count} stopped</span>;
}

function NodePage({ n, initial, may }: { n: NodeView; initial: SeriesRange; may: May }) {
  const perNode = useMachinesPerNode(may.monitoring);
  const machines = perNode.count(n.name);
  const stopped = perNode.stopped(n.name);
  const [range, setRange] = useState<SeriesRange>(initial);
  const o = useMonitoringOverview(may.monitoring);
  return (
    <>
      <DetailHead
        name={n.name}
        kind="Node"
        facts={
          <>
            <Readiness n={n} />
            {n.cordoned && (
              <span className="flex items-center gap-2">
                <Cordoned /> <span className="text-[12px]">No new machines are placed here.</span>
              </span>
            )}
            {machines !== undefined && (
              <span className="tabular">
                {machines === 1 ? "1 machine" : `${machines} machines`}
                <StoppedCount stopped={stopped} />
              </span>
            )}
            {n.heartbeat && <span>reported {ago(n.heartbeat)} ago</span>}
          </>
        }
      />
      <div className="grid grid-cols-1 gap-5 px-4 py-6 md:px-8 xl:grid-cols-[minmax(0,1fr)_380px]">
        <div className="min-w-0 space-y-5">
          {may.monitoring ? (
            <NodeMonitoring n={n} o={o} range={range} onRange={setRange} />
          ) : (
            <Notice tone="info" title="This account's roles do not include reading the platform's monitoring, so the node's alerts, layers and charts are not shown." />
          )}
          <ConditionsCard conditions={n.conditions} />
        </div>
        <div className="min-w-0 space-y-5">
          <HostCard n={n} />
          <Card>
            <CardHeader title="Resources" />
            <ul className="divide-y">
              {capacities(n, perNode.held(n.name)).map((c) => (
                <li key={c.kind} className="grid grid-cols-[64px_1fr] items-start gap-3 px-4 py-3 text-[13px]">
                  <span className="text-muted">{capacityLabel[c.kind]}</span>
                  <div>
                    <CapacityCell c={c} wide />
                    {c.kind === "memory" && !!stopped?.count && (
                      <div className="mt-0.5 text-[11.5px] text-subtle tabular">
                        {formatAmount("memory", stopped.memory)} more if its {stopped.count === 1 ? "stopped machine starts" : `${stopped.count} stopped machines start`}
                      </div>
                    )}
                  </div>
                </li>
              ))}
            </ul>
          </Card>
          <Card>
            <CardHeader title="Agent" />
            <dl className="px-4 py-1">
              <Field label="Version" mono>
                {n.agentVersion || <span className="font-sans text-subtle">not reported</span>}
              </Field>
              <Field label="IPv6 prefix" mono>
                {n.ipv6Prefix || <span className="font-sans text-subtle">none</span>}
              </Field>
              <Field label="Last report">
                {n.heartbeat ? (
                  <>
                    {ago(n.heartbeat)} ago <span className="font-mono text-[11.5px] text-subtle">{n.heartbeat.toISOString().replace("T", " ").slice(0, 19)} UTC</span>
                  </>
                ) : (
                  <span className="text-subtle">never</span>
                )}
              </Field>
            </dl>
          </Card>
        </div>
      </div>
    </>
  );
}

/** What Prometheus and Alertmanager say of the node: its alerts, its host and services, and its charts. */
function NodeMonitoring({ n, o, range, onRange }: { n: NodeView; o: ReturnType<typeof useMonitoringOverview>; range: SeriesRange; onRange: (r: SeriesRange) => void }) {
  const layers = o.data?.nodes.find((x) => x.name === n.name);
  const down = !o.data && isUnavailable(o.error);
  return (
    <>
      <AlertList node={n.name} />
      {layers ? (
        <div className="grid grid-cols-1 items-start gap-4 lg:grid-cols-2">
          {layers.host && <LayerCard layer={layers.host} />}
          {layers.services && <LayerCard layer={layers.services} />}
        </div>
      ) : o.loading ? (
        <Card>
          <SkeletonRows rows={4} />
        </Card>
      ) : down ? (
        <Notice tone="warning" title="Prometheus did not answer, so the host's layers cannot be read.">
          <span className="font-mono text-[12px]">{errorMessage(o.error)}</span>
        </Notice>
      ) : o.error ? (
        <Card>
          <ErrorNotice error={o.error} title="The node's layers did not load" onRetry={o.refetch} />
        </Card>
      ) : (
        <Notice tone="info" title={`Prometheus has no series of ${n.name}.`} />
      )}
      {!down && (
        <section>
          <SectionHead title="Over time" action={<Segmented label="Range" options={ranges} value={range} onChange={onRange} mono />} />
          {/* CPU and memory side by side, the pools under them across both. */}
          <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
            {hostCharts.map((c, i) => (
              <div key={c.name} className={cn(i === hostCharts.length - 1 && "md:col-span-2")}>
                <Chart name={c.name} title={c.title} range={range} node={n.name} />
              </div>
            ))}
          </div>
        </section>
      )}
    </>
  );
}

/** The host's chassis and hardware, as its placement says. */
function HostCard({ n }: { n: NodeView }) {
  const h = n.host;
  return (
    <Card className="overflow-hidden">
      <div className="relative border-b bg-[radial-gradient(ellipse_at_50%_0%,var(--accent-soft),transparent_70%)] px-6 pb-3 pt-6">
        <ChassisArt chassis={h?.art ?? "unknown"} status={artStatus[n.readiness]} />
      </div>
      <div className="px-4 pt-3">
        <div className="text-[15px] font-medium">{h?.cpuModel || n.name}</div>
        {h?.cpuGeneration && <div className="text-[12.5px] text-muted">{h.cpuGeneration}</div>}
      </div>
      <dl className={cn("px-4 pb-1 pt-2")}>
        {h ? (
          <>
            {(h.region || h.site) && (
              <Field label="Region">
                {h.region}
                {h.site && <span className="text-muted"> · {h.site}</span>}
              </Field>
            )}
            {h.memory && <Field label="Memory">{h.memory}</Field>}
            {h.storage && <Field label="Storage">{h.storage}</Field>}
            {h.network && <Field label="Network">{h.network}</Field>}
            {h.chassis && (
              <Field label="Chassis" mono>
                {h.chassis}
              </Field>
            )}
          </>
        ) : (
          <p className="pb-2 text-[13px] text-subtle">The node has not reported its hardware.</p>
        )}
      </dl>
    </Card>
  );
}
