// The platform's health for its administrators (docs/decisions/monitoring.md):
// the alerts firing, each layer's state now, and how the hosts and the API
// went over a range, from MonitoringService. Machines are counted, never
// named, and no tenant appears.

import { useState, type ReactNode } from "react";
import { Notice } from "@/components/notice";
import { PageHead } from "@/components/page";
import { ChartPlaceholder, SeriesChart } from "@/components/series-chart";
import { Topbar } from "@/components/shell";
import { ErrorNotice, Skeleton, SkeletonRows } from "@/components/states";
import { Card, CardHeader, CopyButton, Led, type Tone } from "@/components/ui";
import { errorMessage } from "@/lib/api";
import { cn } from "@/lib/cn";
import {
  isUnavailable,
  nodeSeries,
  since,
  useMonitoringAlerts,
  useMonitoringOverview,
  useMonitoringSeries,
  type Alert,
  type Health,
  type Layer,
  type NodeLayers,
  type Overview,
  type SeriesName,
  type SeriesRange,
} from "@/lib/monitoring";
import { useMayReadMonitoring } from "@/lib/permissions";

const healthTone: Record<Health, Tone> = { ok: "ok", warning: "pending", critical: "fail", info: "off", unknown: "off" };
const healthWord: Record<Health, string> = { ok: "OK", warning: "Warning", critical: "Critical", info: "Info", unknown: "No data" };
const healthText: Record<Health, string> = { ok: "text-ok", warning: "text-pending", critical: "text-fail", info: "text-muted", unknown: "text-subtle" };

const layerTitle: Record<Layer["layer"], string> = {
  reachability: "Reachability",
  host: "Host",
  services: "Platform services",
  machines: "Machines",
};

// Where each card sits. Rows are not shared across the page's width: on two
// columns Reachability tops the first node's Host and its Services tops
// Machines; on three the short two stack in the first column beside the
// tall two. The first row is as tall as its own cards, the second takes
// what the tall two leave, so a short Reachability leaves no gap above
// Services.
const layerPlace: Record<Layer["layer"], string> = {
  reachability: "md:col-start-1 md:row-start-1",
  host: "md:col-start-1 md:row-start-2 xl:col-start-2 xl:row-start-1 xl:row-span-2",
  services: "md:col-start-2 md:row-start-1 xl:col-start-1 xl:row-start-2",
  machines: "md:col-start-2 md:row-start-2 xl:col-start-3 xl:row-start-1 xl:row-span-2",
};

function LayerCard({ layer, node, className }: { layer: Layer; node?: string; className?: string }) {
  return (
    <Card className={className}>
      <CardHeader
        className="h-9"
        title={
          <span className="flex min-w-0 items-center gap-2">
            <Led tone={healthTone[layer.health]} />
            {layerTitle[layer.layer]}
            {node && <span className="truncate font-mono text-[11.5px] font-normal text-subtle">{node}</span>}
          </span>
        }
        action={<span className={cn("whitespace-nowrap text-[12px] font-medium", healthText[layer.health])}>{healthWord[layer.health]}</span>}
      />
      {layer.machines ? (
        <MachineCounts m={layer.machines} />
      ) : layer.checks.length === 0 ? (
        <p className="px-4 py-1.5 text-[13px] text-subtle">No checks answered.</p>
      ) : (
        <ul className="divide-y">
          {layer.checks.map((c) => (
            <CheckRow key={c.name} name={c.name} health={c.health} value={c.value} />
          ))}
        </ul>
      )}
    </Card>
  );
}

// As dense as the console's densest list, the project picker's rows.
function CheckRow({ name, health, value, tone }: { name: string; health: Health; value: string; tone?: Tone }) {
  return (
    <li className="flex items-center gap-2.5 px-4 py-1.5 text-[13px]">
      <Led tone={tone ?? healthTone[health]} className="size-1.5" />
      <span className="shrink-0 text-muted">{name}</span>
      <span
        className={cn(
          "ml-auto min-w-0 truncate text-right font-mono text-[12px]",
          health === "critical" && "text-fail",
          health === "warning" && "text-pending",
          health === "unknown" && "text-subtle",
        )}
      >
        {value || (health === "unknown" ? "no data" : "")}
      </span>
    </li>
  );
}

// A phase's LED, as a machine's own page shows it, except that Failed is red
// only when the platform caused a failure: a program's own alerts no one.
const phaseTone: Record<string, Tone> = { Running: "ok", Pending: "pending", Creating: "pending", Stopped: "off", Suspended: "off", Failed: "fail" };

function MachineCounts({ m }: { m: NonNullable<Layer["machines"]> }) {
  const platform = m.platformFailures.reduce((n, r) => n + r.count, 0);
  const reasons = (rs: { reason: string; count: number }[]) => rs.map((r) => `${r.count} ${r.reason}`).join(", ") || "none";
  return (
    <>
      <ul className="divide-y">
        {m.phases.map((p) => (
          <CheckRow
            key={p.phase}
            name={p.phase}
            health="ok"
            tone={p.count === 0 || (p.phase === "Failed" && platform === 0) ? "off" : (phaseTone[p.phase] ?? "off")}
            value={String(p.count)}
          />
        ))}
        <CheckRow name="Blocked by the platform" health={platform > 0 ? "critical" : "ok"} value={reasons(m.platformFailures)} />
        <CheckRow name="Ended by their own program" health="info" value={reasons(m.programFailures)} />
        <CheckRow name="In a crash loop" health="info" value={String(m.crashLooping)} />
      </ul>
      <div className="border-t px-4 py-2 text-[11.5px] text-subtle">
        Counted, never named. A program&apos;s own failures and crash loops alert no one.
        <div className="mt-1 flex items-center justify-between gap-2 rounded-md bg-surface-2 py-0.5 pl-2.5 pr-1 font-mono text-[11.5px] text-muted">
          <span className="truncate">sudo hangar --context local get machines</span>
          <CopyButton text="sudo hangar --context local get machines" />
        </div>
      </div>
    </>
  );
}

const severityTone: Record<Alert["severity"], Tone> = { critical: "fail", warning: "pending" };

// One line an alert on a wide screen; on a phone its name and summary stack
// beside the severity, with how long it has fired under them.
function AlertRow({ a }: { a: Alert }) {
  const started = a.startTime?.toISOString().replace("T", " ").slice(0, 16);
  return (
    <li className="grid grid-cols-[80px_minmax(0,1fr)] items-baseline gap-x-3 px-4 py-1.5 text-[13px] md:grid-cols-[80px_220px_minmax(0,1fr)_auto]">
      <span className={cn("flex items-center gap-2 self-center font-medium", a.severity === "critical" ? "text-fail" : "text-pending")}>
        <Led tone={severityTone[a.severity]} />
        {a.severity === "critical" ? "Critical" : "Warning"}
      </span>
      <span className="truncate font-mono text-[12.5px] font-medium">{a.name}</span>
      <span className="col-start-2 text-muted md:col-start-auto md:truncate">{a.summary}</span>
      {a.startTime && (
        <span className="col-start-2 whitespace-nowrap text-[12px] text-subtle md:col-start-auto md:text-right">
          for {since(a.startTime)} <span className="hidden font-mono text-[11px] md:inline">· since {started} UTC</span>
        </span>
      )}
    </li>
  );
}

/** A line inside the alerts card, for when there is no list to show. */
function AlertLine({ tone, children }: { tone: Tone; children: ReactNode }) {
  return (
    <p className="flex items-center gap-2.5 px-4 py-2 text-[13px] text-muted">
      <Led tone={tone} />
      <span className="min-w-0">{children}</span>
    </p>
  );
}

function AlertList() {
  const { data, loading, error, refetch } = useMonitoringAlerts(true);
  const alerts = data ?? [];
  return (
    <Card>
      <CardHeader
        className="h-9"
        title={
          <span className="flex items-center gap-2">
            Firing alerts
            {data && <span className="font-mono text-[11px] text-subtle tabular">{alerts.length}</span>}
          </span>
        }
        action={<span className="hidden text-[11.5px] text-subtle sm:block">Critical goes to Telegram at once; warnings once a day.</span>}
      />
      {loading ? (
        <SkeletonRows rows={1} />
      ) : error && !data ? (
        isUnavailable(error) ? (
          <AlertLine tone="off">
            Alertmanager did not answer: <span className="font-mono text-[12px]">{errorMessage(error)}</span>
          </AlertLine>
        ) : (
          <ErrorNotice error={error} title="The alerts did not load" onRetry={refetch} className="px-4 py-2" />
        )
      ) : alerts.length === 0 ? (
        <AlertLine tone="ok">No alerts are firing — every rule reads within its limits.</AlertLine>
      ) : (
        <ul className="divide-y">
          {alerts.map((a, i) => (
            <AlertRow key={`${a.name}-${i}`} a={a} />
          ))}
        </ul>
      )}
    </Card>
  );
}

function Segmented<T extends string>({ label, options, value, onChange, mono }: { label: string; options: T[]; value: T; onChange: (v: T) => void; mono?: boolean }) {
  return (
    <div role="radiogroup" aria-label={label} className="flex min-w-0 rounded-md bg-surface-2 p-0.5">
      {options.map((o) => (
        <button
          key={o}
          role="radio"
          aria-checked={value === o}
          onClick={() => onChange(o)}
          className={cn(
            "h-6 truncate rounded px-2.5 text-[12px]",
            mono && "font-mono",
            value === o ? "bg-surface text-foreground shadow-sm" : "text-subtle hover:text-foreground",
          )}
        >
          {o}
        </button>
      ))}
    </div>
  );
}

const ranges: SeriesRange[] = ["1h", "24h", "7d"];

const charts: { name: SeriesName; title: string }[] = [
  { name: "HOST_CPU", title: "Host CPU" },
  { name: "HOST_MEMORY", title: "Host memory" },
  { name: "POOL_USED", title: "Pools" },
  { name: "API_REQUEST_RATE", title: "API requests" },
  { name: "API_ERROR_RATE", title: "API errors" },
  { name: "API_LATENCY_P99", title: "API latency p99" },
];

function Chart({ name, title, range, node }: { name: SeriesName; title: string; range: SeriesRange; node: string }) {
  const { data, loading, error } = useMonitoringSeries(name, range, node);
  if (data) return <SeriesChart title={title} series={data} range={range} />;
  if (loading) return <ChartPlaceholder title={title}>{<Skeleton className="h-16 w-full" />}</ChartPlaceholder>;
  if (error) return <ChartPlaceholder title={title}>{errorMessage(error)}</ChartPlaceholder>;
  return <ChartPlaceholder title={title}>No node to ask.</ChartPlaceholder>;
}

function SectionHead({ title, action }: { title: string; action?: ReactNode }) {
  return (
    <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
      <h2 className="text-[11px] font-medium uppercase tracking-wider text-subtle">{title}</h2>
      {action}
    </div>
  );
}

/** The layers' cards: the platform's two and the first node's in one grid, each further node's under it. */
function LayerGrid({ o }: { o: Overview }) {
  const [first, ...rest] = o.nodes;
  return (
    <>
      <div className="grid grid-cols-1 items-start gap-4 md:grid-cols-2 md:grid-rows-[auto_1fr] xl:grid-cols-3">
        {o.reachability && <LayerCard layer={o.reachability} className={layerPlace.reachability} />}
        {first?.host && <LayerCard layer={first.host} node={first.name} className={layerPlace.host} />}
        {first?.services && <LayerCard layer={first.services} node={first.name} className={layerPlace.services} />}
        {o.machines && <LayerCard layer={o.machines} className={layerPlace.machines} />}
      </div>
      {rest.map((n: NodeLayers) => (
        <div key={n.name} className="grid grid-cols-1 items-start gap-4 md:grid-cols-2 xl:grid-cols-3">
          {n.services && <LayerCard layer={n.services} node={n.name} />}
          {n.host && <LayerCard layer={n.host} node={n.name} className="xl:col-span-2" />}
        </div>
      ))}
    </>
  );
}

function OverviewSkeleton() {
  return (
    <div className="grid grid-cols-1 items-start gap-4 md:grid-cols-2 xl:grid-cols-3">
      {[4, 9, 9].map((rows, i) => (
        <Card key={i}>
          <SkeletonRows rows={rows} />
        </Card>
      ))}
    </div>
  );
}

function Head({ o }: { o?: Overview }) {
  const nodes = o?.nodes ?? [];
  const asOf = o?.observeTime?.toISOString().slice(11, 19);
  return (
    <div className="px-4 pb-3 pt-5 md:px-8">
      <h1 className="text-[22px] font-semibold tracking-tight">Monitoring</h1>
      <p className="mt-0.5 text-[13px] text-muted">
        The hosts, the platform&apos;s services and the machines on them, as the host&apos;s Prometheus reads them.
        {o && (
          <span className="font-mono text-[12px] text-subtle">
            {" "}
            · <span className="text-muted">{nodes.length === 1 ? nodes[0].name : `${nodes.length} nodes`}</span>
            {asOf && ` · as of ${asOf} UTC`}
          </span>
        )}
      </p>
    </div>
  );
}

function MonitoringBody({ range, onRange }: { range: SeriesRange; onRange: (r: SeriesRange) => void }) {
  const { data: o, loading, error, refetch } = useMonitoringOverview(true);
  const nodes = o?.nodes.map((n) => n.name) ?? [];
  const [picked, setPicked] = useState("");
  const node = nodes.includes(picked) ? picked : (nodes[0] ?? "");
  const down = !o && isUnavailable(error);

  return (
    <>
      <Head o={o} />
      <div className="space-y-4 px-4 pb-8 md:px-8">
        <AlertList />

        {o ? (
          <LayerGrid o={o} />
        ) : loading ? (
          <OverviewSkeleton />
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
            <SectionHead
              title="Over time"
              action={
                <div className="flex min-w-0 flex-wrap items-center gap-2">
                  {nodes.length > 1 && <Segmented label="Node" options={nodes} value={node} onChange={setPicked} mono />}
                  <Segmented label="Range" options={ranges} value={range} onChange={onRange} mono />
                </div>
              }
            />
            <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
              {charts.map((c) => (
                <Chart
                  key={c.name}
                  name={c.name}
                  title={nodes.length > 1 && nodeSeries.has(c.name) ? `${c.title} · ${node}` : c.title}
                  range={range}
                  node={node}
                />
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
export function Monitoring({ range: initial = "24h" }: { range?: SeriesRange }) {
  const can = useMayReadMonitoring();
  const [range, setRange] = useState<SeriesRange>(initial);
  const top = <Topbar platform crumbs={["Platform", "Monitoring"]} />;
  if (!can.settled) return top;
  if (can.error) {
    return (
      <>
        {top}
        <PageHead title="Monitoring" description="Whether this account may read the platform's monitoring is not known." />
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
        <PageHead title="Monitoring" description="This account's roles do not include reading the platform's monitoring." />
      </>
    );
  }
  return (
    <>
      {top}
      <MonitoringBody range={range} onRange={setRange} />
    </>
  );
}
