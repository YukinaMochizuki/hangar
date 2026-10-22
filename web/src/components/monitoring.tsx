// The pieces the platform's pages show its monitoring with
// (docs/decisions/monitoring.md): a layer's card and its checks, the
// machines counted, never named, the alerts firing, and a series' chart.
// The Overview and a Node's page are made of them.

import { Link } from "@tanstack/react-router";
import { type ReactNode } from "react";
import { ChartPlaceholder, SeriesChart } from "@/components/series-chart";
import { ErrorNotice, Skeleton, SkeletonRows } from "@/components/states";
import { Card, CardHeader, CopyButton, Led, type Tone } from "@/components/ui";
import { errorMessage } from "@/lib/api";
import { cn } from "@/lib/cn";
import { isUnavailable, since, useMonitoringAlerts, useMonitoringSeries, type Alert, type Health, type Layer, type SeriesName, type SeriesRange } from "@/lib/monitoring";

export const healthTone: Record<Health, Tone> = { ok: "ok", warning: "pending", critical: "fail", info: "off", unknown: "off" };
export const healthWord: Record<Health, string> = { ok: "OK", warning: "Warning", critical: "Critical", info: "Info", unknown: "No data" };
const healthText: Record<Health, string> = { ok: "text-ok", warning: "text-pending", critical: "text-fail", info: "text-muted", unknown: "text-subtle" };

const layerTitle: Record<Layer["layer"], string> = {
  reachability: "Reachability",
  host: "Host",
  services: "Platform services",
  machines: "Machines",
};

export function LayerCard({ layer, className }: { layer: Layer; className?: string }) {
  return (
    <Card className={className}>
      <CardHeader
        className="h-9"
        title={
          <span className="flex min-w-0 items-center gap-2">
            <Led tone={healthTone[layer.health]} />
            {layerTitle[layer.layer]}
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
  // The phases as one row of figures, so the card stands as short as the
  // Reachability card beside it; the failures that need words follow.
  return (
    <>
      <ul className="grid grid-cols-3 border-b sm:grid-cols-6" aria-label="Machines by phase">
        {m.phases.map((p) => {
          const tone = p.count === 0 || (p.phase === "Failed" && platform === 0) ? "off" : (phaseTone[p.phase] ?? "off");
          return (
            <li key={p.phase} className="px-3 py-2 text-center">
              <div className={cn("font-mono text-[17px] font-medium tabular", p.count === 0 && "text-subtle")}>{p.count}</div>
              <div className="mt-0.5 flex items-center justify-center gap-1.5 text-[11.5px] text-muted">
                <Led tone={tone} className="size-1.5" />
                {p.phase}
              </div>
            </li>
          );
        })}
      </ul>
      <ul className="divide-y">
        <CheckRow name="Blocked by the platform" health={platform > 0 ? "critical" : "ok"} value={reasons(m.platformFailures)} />
        <CheckRow name="Ended by their own program" health="info" value={reasons(m.programFailures)} />
        <CheckRow name="In a crash loop" health="info" value={String(m.crashLooping)} />
      </ul>
      <div className="border-t px-4 py-2 text-[11.5px] text-subtle">
        A program&apos;s own failures alert no one.
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
// On the Overview a host's alert names its node, which opens the node's page.
function AlertRow({ a, showNode }: { a: Alert; showNode: boolean }) {
  const started = a.startTime?.toISOString().replace("T", " ").slice(0, 16);
  return (
    <li
      className={cn(
        "grid grid-cols-[80px_minmax(0,1fr)] items-baseline gap-x-3 px-4 py-1.5 text-[13px]",
        showNode ? "md:grid-cols-[80px_220px_120px_minmax(0,1fr)_auto]" : "md:grid-cols-[80px_220px_minmax(0,1fr)_auto]",
      )}
    >
      <span className={cn("flex items-center gap-2 self-center font-medium", a.severity === "critical" ? "text-fail" : "text-pending")}>
        <Led tone={severityTone[a.severity]} />
        {a.severity === "critical" ? "Critical" : "Warning"}
      </span>
      <span className="truncate font-mono text-[12.5px] font-medium">{a.name}</span>
      {showNode && (
        <span className="col-start-2 truncate font-mono text-[12px] md:col-start-auto">
          {a.node ? (
            <Link to="/platform/nodes/$node" params={{ node: a.node }} className="text-accent hover:underline">
              {a.node}
            </Link>
          ) : (
            <span className="font-sans text-subtle">platform</span>
          )}
        </span>
      )}
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

/**
 * The alerts firing: every one, each naming its node, or, given a node, that
 * node's alone.
 */
export function AlertList({ node }: { node?: string }) {
  const { data, loading, error, refetch } = useMonitoringAlerts(true);
  const alerts = (data ?? []).filter((a) => node === undefined || a.node === node);
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
        action={<span className="hidden text-[11.5px] text-subtle sm:block">Critical: Telegram at once; warnings daily</span>}
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
        <AlertLine tone="ok">
          {node === undefined ? "No alerts firing." : `No alerts firing on ${node}.`}
        </AlertLine>
      ) : (
        <ul className="divide-y">
          {alerts.map((a, i) => (
            <AlertRow key={`${a.name}-${i}`} a={a} showNode={node === undefined} />
          ))}
        </ul>
      )}
    </Card>
  );
}

export function Segmented<T extends string>({ label, options, value, onChange, mono }: { label: string; options: T[]; value: T; onChange: (v: T) => void; mono?: boolean }) {
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

export const ranges: SeriesRange[] = ["1h", "24h", "7d"];

/** A node's charts, on its page. */
export const hostCharts: { name: SeriesName; title: string }[] = [
  { name: "HOST_CPU", title: "CPU" },
  { name: "HOST_MEMORY", title: "Memory" },
  { name: "POOL_USED", title: "Pools" },
];

/** The platform's charts, on the Overview. */
export const apiCharts: { name: SeriesName; title: string }[] = [
  { name: "API_REQUEST_RATE", title: "API requests" },
  { name: "API_ERROR_RATE", title: "API errors" },
  { name: "API_LATENCY_P99", title: "API latency p99" },
];

export function Chart({ name, title, range, node }: { name: SeriesName; title: string; range: SeriesRange; node: string }) {
  const { data, loading, error } = useMonitoringSeries(name, range, node);
  if (data) return <SeriesChart title={title} series={data} range={range} />;
  if (loading) return <ChartPlaceholder title={title}>{<Skeleton className="h-16 w-full" />}</ChartPlaceholder>;
  if (error) return <ChartPlaceholder title={title}>{errorMessage(error)}</ChartPlaceholder>;
  return <ChartPlaceholder title={title}>No node to ask.</ChartPlaceholder>;
}

export function SectionHead({ title, action }: { title: string; action?: ReactNode }) {
  return (
    <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
      <h2 className="text-[11px] font-medium uppercase tracking-wider text-subtle">{title}</h2>
      {action}
    </div>
  );
}
