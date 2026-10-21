// The platform's Overview for its administrators (docs/decisions/monitoring.md):
// the alerts firing, the platform's own layers now, each node's state in a
// row that opens the node's page, and how the API went over a range, from
// MonitoringService. A node's host and services, and its charts, are on its
// page (pages/nodes.tsx). Machines are counted, never named, and no tenant
// appears.

import { Link } from "@tanstack/react-router";
import { ChevronRight } from "lucide-react";
import { useState } from "react";
import { AlertList, apiCharts, Chart, healthTone, healthWord, LayerCard, ranges, SectionHead, Segmented } from "@/components/monitoring";
import { Notice } from "@/components/notice";
import { PageHead } from "@/components/page";
import { Topbar } from "@/components/shell";
import { ErrorNotice, SkeletonRows } from "@/components/states";
import { NoMatch, tbodyRow, Th, theadRow } from "@/components/table";
import { Card, Led } from "@/components/ui";
import { errorMessage } from "@/lib/api";
import { cn } from "@/lib/cn";
import { isUnavailable, useMonitoringAlerts, useMonitoringOverview, type Health, type NodeLayers, type Overview, type SeriesRange } from "@/lib/monitoring";
import { nodePerms, useMachinesPerNode, useNodes, type NodeView } from "@/lib/nodes";
import { useMayReadMonitoring, usePlatformCan } from "@/lib/permissions";
import { Cordoned, Readiness } from "@/pages/nodes";

/** A layer's state as a row's cell: its LED and its word. */
function HealthCell({ health }: { health?: Health }) {
  if (!health) return <span className="text-subtle">—</span>;
  return (
    <span className="flex items-center gap-2">
      <Led tone={healthTone[health]} />
      <span className={cn(health === "unknown" && "text-subtle")}>{healthWord[health]}</span>
    </span>
  );
}

/**
 * Each node in a row: its host's and its services' state as Prometheus
 * reads them, what the Node says of itself once NodeService answered, and
 * its alerts and machines, counted. A row opens the node's page.
 */
function NodeRows({ o }: { o: Overview }) {
  // Each source is asked only once the caller is known to hold its permission.
  const p = usePlatformCan([nodePerms.list, nodePerms.machines]);
  const nodes = useNodes(p.can(nodePerms.list));
  const machines = useMachinesPerNode(p.can(nodePerms.machines));
  const alerts = useMonitoringAlerts(true).data;
  const failed = [nodes.error && `the nodes: ${errorMessage(nodes.error)}`, machines.error && `the machines: ${errorMessage(machines.error)}`].filter(Boolean);
  const views = new Map<string, NodeView>(nodes.items.map((n) => [n.name, n]));
  return (
    <Card className="overflow-x-auto">
      <table className="w-full min-w-[720px] text-[13px]">
        <thead>
          <tr className={theadRow}>
            <Th className="w-[26%]">Node</Th>
            <Th>Host</Th>
            <Th>Platform services</Th>
            <Th>State</Th>
            <Th>Alerts</Th>
            <Th>Machines</Th>
            <Th className="w-8 px-0" />
          </tr>
        </thead>
        <tbody>
          {o.nodes.map((n: NodeLayers) => {
            const v = views.get(n.name);
            const firing = alerts?.filter((a) => a.node === n.name).length;
            return (
              <tr key={n.name} className={cn(tbodyRow, "cursor-pointer")}>
                <td className="max-w-0 px-4 py-2.5">
                  <Link to="/platform/nodes/$node" params={{ node: n.name }} className="block">
                    <span className="block truncate font-mono font-medium group-hover:text-accent">{n.name}</span>
                    {v?.host?.cpuModel && <span className="block truncate text-[11.5px] text-subtle">{v.host.cpuModel}</span>}
                  </Link>
                </td>
                <td className="whitespace-nowrap px-4 py-2.5">
                  <HealthCell health={n.host?.health} />
                </td>
                <td className="whitespace-nowrap px-4 py-2.5">
                  <HealthCell health={n.services?.health} />
                </td>
                <td className="whitespace-nowrap px-4 py-2.5">
                  {v ? (
                    <span className="flex items-center gap-2">
                      <Readiness n={v} />
                      {v.cordoned && <Cordoned />}
                    </span>
                  ) : (
                    <span className="text-subtle">—</span>
                  )}
                </td>
                <td className="whitespace-nowrap px-4 py-2.5 tabular">
                  {firing === undefined ? <span className="text-subtle">—</span> : <span className={cn(firing > 0 ? "font-medium text-fail" : "text-muted")}>{firing}</span>}
                </td>
                <td className="whitespace-nowrap px-4 py-2.5 tabular text-muted">{machines.count(n.name) ?? <span className="text-subtle">—</span>}</td>
                <td className="px-2 py-2.5 text-subtle">
                  <ChevronRight className="size-4" />
                </td>
              </tr>
            );
          })}
          {o.nodes.length === 0 && <NoMatch noun="node" colSpan={7} />}
        </tbody>
      </table>
      {failed.length > 0 && <p className="border-t px-4 py-2 text-[12px] text-fail">Did not load {failed.join("; ")}.</p>}
    </Card>
  );
}

function Head({ o }: { o?: Overview }) {
  const nodes = o?.nodes ?? [];
  const asOf = o?.observeTime?.toISOString().slice(11, 19);
  return (
    <div className="px-4 pb-3 pt-5 md:px-8">
      <h1 className="text-[22px] font-semibold tracking-tight">Overview</h1>
      <p className="mt-0.5 text-[13px] text-muted">
        The platform&apos;s reachability, its machines and each node, as the hosts&apos; Prometheus reads them.
        {o && (
          <span className="font-mono text-[12px] text-subtle">
            {" "}
            · <span className="text-muted">{nodes.length === 1 ? "1 node" : `${nodes.length} nodes`}</span>
            {asOf && ` · as of ${asOf} UTC`}
          </span>
        )}
      </p>
    </div>
  );
}

function OverviewBody({ range, onRange }: { range: SeriesRange; onRange: (r: SeriesRange) => void }) {
  const { data: o, loading, error, refetch } = useMonitoringOverview(true);
  const down = !o && isUnavailable(error);

  return (
    <>
      <Head o={o} />
      <div className="space-y-4 px-4 pb-8 md:px-8">
        <AlertList />

        {o ? (
          <>
            <div className="grid grid-cols-1 items-start gap-4 md:grid-cols-2">
              {o.reachability && <LayerCard layer={o.reachability} />}
              {o.machines && <LayerCard layer={o.machines} />}
            </div>
            <section>
              <SectionHead
                title="Nodes"
                action={
                  <Link to="/platform/nodes" className="text-[12px] text-muted hover:text-foreground">
                    All nodes →
                  </Link>
                }
              />
              <NodeRows o={o} />
            </section>
          </>
        ) : loading ? (
          <Card>
            <SkeletonRows rows={4} />
          </Card>
        ) : down ? (
          <Notice tone="warning" title="Prometheus did not answer, so no layer can be read.">
            <span className="font-mono text-[12px]">{errorMessage(error)}</span>
          </Notice>
        ) : (
          <Card>
            <ErrorNotice error={error} title="The overview did not load" onRetry={refetch} />
          </Card>
        )}

        {!down && (
          <section>
            <SectionHead title="Over time" action={<Segmented label="Range" options={ranges} value={range} onChange={onRange} mono />} />
            <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
              {apiCharts.map((c) => (
                <Chart key={c.name} name={c.name} title={c.title} range={range} node="" />
              ))}
            </div>
          </section>
        )}
      </div>
    </>
  );
}

/**
 * The page, once TestPermissions says the caller holds
 * platform.monitoring.read: nothing while the answer is unknown, the
 * refusal when it says not, the failure when asking failed.
 */
export function PlatformOverview({ range: initial = "24h" }: { range?: SeriesRange }) {
  const can = useMayReadMonitoring();
  const [range, setRange] = useState<SeriesRange>(initial);
  const top = <Topbar platform crumbs={["Platform", "Overview"]} />;
  if (!can.settled) return top;
  if (can.error) {
    return (
      <>
        {top}
        <PageHead title="Overview" description="Whether this account may read the platform's monitoring is not known." />
        <div className="px-4 md:px-8">
          <Card>
            <ErrorNotice error={can.error} title="TestPermissions failed" />
          </Card>
        </div>
      </>
    );
  }
  if (!can.may) {
    return (
      <>
        {top}
        <PageHead title="Overview" description="This account's roles do not include reading the platform's monitoring." />
      </>
    );
  }
  return (
    <>
      {top}
      <OverviewBody range={range} onRange={setRange} />
    </>
  );
}
