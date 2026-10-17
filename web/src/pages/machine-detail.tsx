// One container or virtual machine: where it is in its lifecycle, what it
// is, how it is reached, what it wrote, and the host it runs on.

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { MachineCapabilityService, MachineLogStream, type MachineLogRecord } from "@hangar/api/hangar/compute/v1alpha1/machine_capability_pb";
import { MachinePhase } from "@hangar/api/hangar/compute/v1alpha1/machine_pb";
import { Protocol, RestartPolicy } from "@hangar/api/hangar/compute/v1alpha1/container_pb";
import { ConditionStatus, type Condition } from "@hangar/api/hangar/meta/v1/meta_pb";
import { PublicEndpointService } from "@hangar/api/hangar/network/v1alpha1/public_endpoint_pb";
import { ArrowUpRight, ChevronDown, ChevronRight, ChevronUp, Lock } from "lucide-react";
import { useEffect, useState } from "react";
import { artStatus } from "@/components/chassis-art";
import { MachineActions } from "@/components/machine-actions";
import { PageBanner } from "@/components/notice";
import { RunsOnPanel } from "@/components/runs-on-panel";
import { Topbar, useProjectName } from "@/components/shell";
import { EmptyState, ErrorNotice, Skeleton, SkeletonRows } from "@/components/states";
import { Taxiway, type Stop, type StopState } from "@/components/taxiway";
import { Badge, Card, CardHeader, CopyButton, Field, SizeBadge, Status, StatusDot } from "@/components/ui";
import { cn } from "@/lib/cn";
import { manifestOf, toYaml } from "@/lib/manifest";
import { ago, date, isLive, refetchInterval, sizeOf, uptime, useMachines, type MachineKind, type MachineView } from "@/lib/machines";
import { useReducedMotion } from "@/lib/motion";
import { formatNumber } from "@/lib/quantity";
import { runsOnState } from "@/lib/runs-on";
import { bare } from "@/pages/machine-list";

const restartPolicies: Record<number, string> = {
  [RestartPolicy.UNSPECIFIED]: "Default",
  [RestartPolicy.ALWAYS]: "Always",
  [RestartPolicy.ON_FAILURE]: "On failure",
  [RestartPolicy.NEVER]: "Never",
};

/** The node's word on the machine: its Ready condition, else the object's. */
function readyOf(m: MachineView): Condition | undefined {
  return m.machine?.status?.conditions.find((c) => c.type === "Ready") ?? m.conditions.find((c) => c.type === "Ready");
}

function sizeText(m: MachineView) {
  return `${formatNumber(m.cpu)} vCPU · ${formatNumber(m.memoryGi)} GiB${Number.isFinite(m.diskGi) ? ` · ${formatNumber(m.diskGi)} GiB disk` : ""}`;
}

function Header({ m }: { m: MachineView }) {
  const size = sizeOf(m);
  const nic = m.interfaces[0];
  return (
    <div className="border-b px-4 pb-5 pt-6 md:px-8">
      <div className="flex flex-col gap-4 md:flex-row md:items-start md:justify-between md:gap-6">
        <div className="min-w-0">
          <div className="flex items-center gap-2.5">
            <h1 className="truncate text-[22px] font-semibold tracking-tight">{m.name}</h1>
            <Badge>{m.kind === "Container" ? "Container" : "Virtual machine"}</Badge>
          </div>
          <div className="mt-2 flex flex-wrap items-center gap-x-5 gap-y-1 text-[13px] text-muted">
            {m.deleting ? <span>Deleting</span> : <Status phase={m.phase} />}
            {m.phase === MachinePhase.RUNNING && m.booted && <span>up {uptime(m.booted)}</span>}
            <span className="flex items-center gap-1.5">
              {size && <SizeBadge size={size.label} />}
              <span className="tabular">{sizeText(m)}</span>
            </span>
            {nic?.ipv4 && <span className="font-mono text-[12px]">{bare(nic.ipv4)}</span>}
          </div>
        </div>
        <MachineActions m={m} />
      </div>
    </div>
  );
}

/** Where the machine is on its way from scheduled to running. */
function lifecycle(m: MachineView): Stop[] {
  const labels = m.kind === "Container" ? ["Scheduled", "Image pulled", "Booted", "Running"] : ["Scheduled", "Disk ready", "Booted", "Running"];
  const ready = readyOf(m);
  const reason = ready?.reason ?? "";
  const at = (i: number, state: StopState, note?: string): Stop[] =>
    labels.map((label, j) => (j < i ? { label, state: "done" } : j === i ? { label, state, note } : { label, state: "todo" }));
  if (m.stopped || m.phase === MachinePhase.STOPPED) {
    return [...labels.slice(0, 3).map((label) => ({ label, state: "done" as const })), { label: "Stopped", state: "idle" }];
  }
  switch (m.phase) {
    case MachinePhase.RUNNING:
      return labels.map((label, j) => ({
        label,
        state: "done",
        note: j === 0 ? m.node : j === labels.length - 1 && m.booted ? `up ${uptime(m.booted)}` : undefined,
      }));
    case MachinePhase.CREATING:
      return at(reason === "Booting" ? 2 : 1, "current", reason || undefined);
    case MachinePhase.FAILED:
      return at(/^(ImagePull|Disk)/.test(reason) ? 1 : reason === "BootFailed" ? 2 : 3, "failed", reason || undefined);
    default:
      // Placed, but its node has not reported on it yet.
      if (m.node) return at(1, "current", "node has not reported");
      return at(0, "current", "no host yet");
  }
}

/**
 * The lifecycle while a machine is on its way, folded to one line once it
 * runs. A machine already running opens folded; one that arrives while
 * watched pulls its earlier stops into Running a moment after it gets
 * there, then folds. With reduced motion asked for, it folds at once.
 */
function Lifecycle({ m }: { m: MachineView }) {
  const running = m.phase === MachinePhase.RUNNING && !m.stopped;
  const still = useReducedMotion();
  const [folded, setFolded] = useState(running);
  const [gather, setGather] = useState(false);
  // Set when the machine reaches Running while the page is open.
  const [arriving, setArriving] = useState(false);
  const [was, setWas] = useState(running);
  if (was !== running) {
    setWas(running);
    setGather(false);
    if (!running) {
      setFolded(false);
      setArriving(false);
    } else if (still) setFolded(true);
    else setArriving(true);
  }
  useEffect(() => {
    if (!arriving) return;
    const ts = [
      setTimeout(() => setGather(true), 1400),
      setTimeout(() => {
        setFolded(true);
        setArriving(false);
      }, 1400 + 760),
    ];
    return () => ts.forEach(clearTimeout);
  }, [arriving]);
  const fold = "grid transition-[grid-template-rows,opacity] duration-500 ease-in-out motion-reduce:transition-none";

  return (
    <Card className="overflow-hidden">
      <div className={cn(fold, folded ? "grid-rows-[0fr] opacity-0" : "grid-rows-[1fr] opacity-100")}>
        <div className="min-h-0 overflow-hidden">
          <div className="relative px-5 pb-4 pt-5">
            <Taxiway stops={lifecycle(m)} gather={gather} />
            {running && !gather && (
              <button
                onClick={() => setFolded(true)}
                className="absolute right-3 top-3 rounded p-1 text-subtle hover:bg-surface-2 hover:text-foreground"
                aria-label="Fold the lifecycle"
              >
                <ChevronUp className="size-3.5" />
              </button>
            )}
          </div>
        </div>
      </div>
      <div className={cn(fold, folded ? "grid-rows-[1fr] opacity-100" : "grid-rows-[0fr] opacity-0")}>
        <div className="min-h-0 overflow-hidden">
          <div className="flex items-center gap-3 px-5 py-3 text-[13px]">
            {/* The end of the taxi line, arrived. */}
            <span className="flex items-center">
              <span className="h-0.5 w-5 rounded-l bg-accent" />
              <span className="size-3 rounded-full bg-accent" />
            </span>
            <span className="font-medium">Running</span>
            <span className="min-w-0 truncate text-muted">
              {m.booted && <>up {uptime(m.booted)} · </>}on <span className="font-mono text-[12px]">{m.node}</span>
            </span>
            <button
              onClick={() => {
                setGather(false);
                setFolded(false);
              }}
              className="ml-auto flex shrink-0 items-center gap-1 rounded px-1.5 py-1 text-[12px] text-subtle hover:bg-surface-2 hover:text-foreground"
            >
              Boot <ChevronDown className="size-3.5" />
            </button>
          </div>
        </div>
      </div>
    </Card>
  );
}

function Labels({ labels }: { labels: Record<string, string> }) {
  const entries = Object.entries(labels);
  return (
    <span className="flex flex-wrap gap-1.5">
      {entries.length === 0 && <span className="text-subtle">None</span>}
      {entries.map(([k, v]) => (
        <span key={k} className="rounded bg-surface-2 px-1.5 py-0.5 font-mono text-[11.5px]">
          {k}=<span className="text-muted">{v}</span>
        </span>
      ))}
    </span>
  );
}

function Summary({ m }: { m: MachineView }) {
  const size = sizeOf(m);
  const root = m.vm?.spec?.rootDisk?.source;
  return (
    <Card>
      <CardHeader title="Summary" />
      <dl className="divide-y px-4">
        <Field label="Image" mono>
          {m.image || <span className="text-subtle">none</span>}
          {m.imageDigest && <div className="mt-0.5 text-subtle">{m.imageDigest.slice(0, 19)}…</div>}
        </Field>
        <Field label="Size">
          <span className="flex items-center gap-2">
            {size && <SizeBadge size={size.label} />}
            <span className="tabular">{sizeText(m)}</span>
          </span>
        </Field>
        {m.container && <Field label="Restart policy">{restartPolicies[m.container.spec?.restartPolicy ?? 0]}</Field>}
        {m.vm && (
          <Field label="Root disk" mono>
            {m.vm.status?.rootDisk || (root?.case === "disk" ? root.value : "–")}
          </Field>
        )}
        {m.vm && <Field label="SSH keys">{m.vm.spec?.sshAuthorizedKeys.length ?? 0}</Field>}
        <Field label="Run state">{m.stopped ? "Stopped" : "Running"}</Field>
        <Field label="Labels">
          <Labels labels={m.labels} />
        </Field>
        <Field label="Created">{m.created?.toISOString().replace("T", " ").slice(0, 16)} UTC</Field>
        <Field label="UID" mono>
          <span className="text-muted">{m.uid}</span>
        </Field>
      </dl>
    </Card>
  );
}

function Networking({ m }: { m: MachineView }) {
  const endpoints = useQuery(
    PublicEndpointService.method.listPublicEndpoints,
    { project: m.project, pageSize: 1000 },
    { enabled: m.kind === "Container", refetchInterval },
  );
  const ports = m.container?.spec?.ports ?? [];
  const urlOf = (port: string) =>
    endpoints.data?.items.find((e) => e.metadata?.name === `${m.name}-${port}`)?.status?.url;
  return (
    <Card>
      <CardHeader title="Networking" />
      {m.interfaces.length === 0 ? (
        <div className="px-4 py-3 text-[13px] text-muted">No interfaces yet: addresses come once the machine is made.</div>
      ) : (
        <>
        {/* On a phone, each interface is a card of its own: the table's
            addresses do not fit across. */}
        <ul className="divide-y md:hidden">
          {m.interfaces.map((i) => (
            <li key={i.name} className="px-4 py-3 text-[13px]">
              <div className="flex items-baseline justify-between gap-3">
                <span className="font-mono text-[12.5px] font-medium">{i.name}</span>
                <span className="text-muted">{i.network}</span>
              </div>
              <dl className="mt-2 space-y-1.5">
                <div>
                  <dt className="text-[11.5px] text-subtle">IPv4</dt>
                  <dd className="font-mono text-[12.5px]">{i.ipv4 ? bare(i.ipv4) : <span className="text-subtle">–</span>}</dd>
                </div>
                <div>
                  <dt className="text-[11.5px] text-subtle">Public IPv6</dt>
                  <dd className="flex items-center gap-1 font-mono text-[12.5px] break-all">
                    {i.ipv6 ? (
                      <>
                        {bare(i.ipv6)}
                        <CopyButton text={bare(i.ipv6)} className="shrink-0" />
                      </>
                    ) : (
                      <span className="text-subtle">–</span>
                    )}
                  </dd>
                </div>
                <div>
                  <dt className="text-[11.5px] text-subtle">Security groups</dt>
                  <dd className="mt-0.5 flex flex-wrap gap-1">
                    {i.securityGroups.map((g) => (
                      <Badge key={g}>{g}</Badge>
                    ))}
                  </dd>
                </div>
              </dl>
            </li>
          ))}
        </ul>
        <div className="hidden overflow-x-auto md:block">
          <table className="w-full min-w-[640px] text-[13px]">
            <thead>
              <tr className="border-b text-left text-[11.5px] text-subtle">
                <th className="px-4 py-2 font-medium">Interface</th>
                <th className="px-4 py-2 font-medium">Network</th>
                <th className="px-4 py-2 font-medium">IPv4</th>
                <th className="px-4 py-2 font-medium">Public IPv6</th>
                <th className="px-4 py-2 font-medium">Security groups</th>
              </tr>
            </thead>
            <tbody>
              {m.interfaces.map((i) => (
                <tr key={i.name} className="border-b last:border-0">
                  <td className="px-4 py-2.5 font-mono text-[12.5px]">{i.name}</td>
                  <td className="px-4 py-2.5">{i.network}</td>
                  <td className="px-4 py-2.5 font-mono text-[12.5px]">{i.ipv4 ? bare(i.ipv4) : <span className="text-subtle">–</span>}</td>
                  <td className="px-4 py-2.5 font-mono text-[12.5px]">
                    {i.ipv6 ? (
                      <span className="flex items-center gap-1">
                        {bare(i.ipv6)}
                        <CopyButton text={bare(i.ipv6)} />
                      </span>
                    ) : (
                      <span className="text-subtle">–</span>
                    )}
                  </td>
                  <td className="px-4 py-2.5">
                    <span className="flex flex-wrap gap-1">
                      {i.securityGroups.map((g) => (
                        <Badge key={g}>{g}</Badge>
                      ))}
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        </>
      )}
      {ports.length > 0 && (
        <div className="border-t px-4 py-3">
          <div className="mb-2 text-[11.5px] font-medium text-subtle">Ports</div>
          <ul className="space-y-1.5">
            {ports.map((p) => {
              const url = p.public ? urlOf(p.name) : undefined;
              return (
                <li key={p.name} className="flex items-center gap-3 text-[13px]">
                  <span className="w-24 font-mono text-[12.5px]">
                    {p.port}/{p.protocol === Protocol.UDP ? "udp" : "tcp"}
                  </span>
                  <span className="hidden w-20 truncate text-muted sm:block">{p.name}</span>
                  {url ? (
                    <a href={url} target="_blank" rel="noreferrer" className="flex min-w-0 items-center gap-1 text-accent hover:underline">
                      <Lock className="size-3 shrink-0" />
                      <span className="truncate font-mono text-[12.5px]">{url}</span>
                      <ArrowUpRight className="size-3 shrink-0" />
                    </a>
                  ) : (
                    <span className="text-subtle">{p.public ? "public endpoint not ready" : "not public"}</span>
                  )}
                </li>
              );
            })}
          </ul>
        </div>
      )}
    </Card>
  );
}

const statusWord: Record<number, string> = {
  [ConditionStatus.TRUE]: "True",
  [ConditionStatus.FALSE]: "False",
  [ConditionStatus.UNKNOWN]: "Unknown",
};

/** The raw conditions, folded away: the lifecycle says the same in plain words. */
function Conditions({ m }: { m: MachineView }) {
  const rows = [
    ...(m.container?.status?.conditions ?? m.vm?.status?.conditions ?? []).map((c) => ({ c, of: m.kind })),
    ...(m.machine?.status?.conditions ?? []).map((c) => ({ c, of: "Machine" })),
  ];
  return (
    <Card className="overflow-hidden">
      <details className="group">
        <summary className="flex h-11 cursor-pointer list-none items-center gap-2 px-4 text-[13px] font-medium [&::-webkit-details-marker]:hidden">
          <ChevronRight className="size-3.5 text-subtle transition-transform group-open:rotate-90 motion-reduce:transition-none" />
          Conditions
          <span className="font-mono text-[11.5px] font-normal text-subtle">{rows.length}</span>
        </summary>
        <ul className="divide-y border-t">
          {rows.map(({ c, of }) => (
            <li key={`${of}/${c.type}`} className="flex items-center gap-3 px-4 py-2.5 text-[13px]">
              <span
                className={cn(
                  "size-1.5 shrink-0 rounded-full",
                  c.status === ConditionStatus.TRUE ? "bg-ok" : c.status === ConditionStatus.UNKNOWN ? "bg-pending" : "bg-subtle",
                )}
              />
              <span className="w-20 shrink-0 text-[12px] text-subtle">{of}</span>
              <span className="w-20 shrink-0 font-medium">{c.type}</span>
              <span className="w-16 shrink-0 font-mono text-[12px] text-muted">{statusWord[c.status] ?? "?"}</span>
              <span className="shrink-0 font-mono text-[12px] text-muted">{c.reason}</span>
              <span className="hidden min-w-0 flex-1 truncate text-muted md:block">{c.message}</span>
              <span className="flex-1 md:hidden" />
              <span className="shrink-0 text-[12px] text-subtle">{ago(date(c.lastTransitionTime))} ago</span>
            </li>
          ))}
        </ul>
      </details>
    </Card>
  );
}

const decoder = new TextDecoder();
// How much of the log the page asks for when it opens.
const logTail = 50n;
const keptLines = 200;

type LogLine = { key: number; time?: Date; stream: MachineLogStream; text: string };

function toLines(r: MachineLogRecord, next: () => number): LogLine[] {
  const time = date(r.time);
  if (r.content.case === "exit") {
    const e = r.content.value;
    return [{ key: next(), time, stream: r.stream, text: `boot ${r.boot} ended: exit ${e.code}${e.signal ? `, signal ${e.signal}` : ""}` }];
  }
  if (r.content.case !== "data") return [];
  return decoder
    .decode(r.content.value)
    .replace(/\n$/, "")
    .split("\n")
    .map((text) => ({ key: next(), time, stream: r.stream, text }));
}

/**
 * The last of what the machine wrote, followed while the page is open, over
 * the same server stream `hangar logs -f` reads.
 */
function Logs({ m }: { m: MachineView }) {
  const [attempt, setAttempt] = useState(0);
  return <LogStream key={attempt} m={m} onRetry={() => setAttempt(attempt + 1)} />;
}

function LogStream({ m, onRetry }: { m: MachineView; onRetry: () => void }) {
  const transport = useTransport();
  const [lines, setLines] = useState<LogLine[]>([]);
  const [state, setState] = useState<"loading" | "following" | "ended" | "error">("loading");
  const [error, setError] = useState<unknown>(null);
  useEffect(() => {
    const abort = new AbortController();
    let n = 0;
    const next = () => n++;
    (async () => {
      try {
        const client = createClient(MachineCapabilityService, transport);
        const stream = client.logs(
          { project: m.project, kind: m.kind, name: m.name, follow: true, tailLines: logTail },
          { signal: abort.signal },
        );
        setState("following");
        for await (const res of stream) {
          const add = res.records.flatMap((r) => toLines(r, next));
          setLines((old) => [...old, ...add].slice(-keptLines));
        }
        setState("ended");
      } catch (err) {
        if (abort.signal.aborted) return;
        setError(err);
        setState("error");
      }
    })();
    return () => abort.abort();
  }, [transport, m.project, m.kind, m.name]);

  return (
    <Card className="overflow-hidden">
      <CardHeader
        title="Recent logs"
        action={
          state === "following" ? (
            <span className="flex items-center gap-2 text-[12px] text-muted">
              <span className="size-1.5 animate-pulse rounded-full bg-ok motion-reduce:animate-none" /> Following
            </span>
          ) : state === "ended" ? (
            <span className="text-[12px] text-subtle">Stream ended</span>
          ) : null
        }
      />
      {state === "error" ? (
        <ErrorNotice error={error} title="The log did not load" onRetry={onRetry} />
      ) : state === "loading" ? (
        <SkeletonRows rows={3} />
      ) : lines.length === 0 ? (
        <div className="px-4 py-3 text-[13px] text-muted">No output recorded.</div>
      ) : (
        <pre className="max-h-80 overflow-auto bg-surface-2 px-4 py-3 font-mono text-[12px] leading-6 text-muted">
          {lines.map((l) => (
            <div key={l.key} className={cn(l.stream === MachineLogStream.STDERR && "text-fail", l.stream === MachineLogStream.EXIT && "text-pending")}>
              <span className="select-none text-subtle">{l.time?.toISOString().slice(11, 19) ?? "--:--:--"}</span> {l.text}
            </div>
          ))}
        </pre>
      )}
    </Card>
  );
}

function RunsOn({ m }: { m: MachineView }) {
  const all = useMachines(m.project);
  const state = runsOnState(m);
  const mine = all.items.filter((x) => x.node === m.node);
  const live = mine.filter((x) => isLive(x) && !x.deleting);
  const sum = (xs: MachineView[], f: (x: MachineView) => number) => xs.reduce((s, x) => s + (Number.isFinite(f(x)) ? f(x) : 0), 0);
  return (
    <RunsOnPanel
      state={state}
      status={artStatus(m.phase)}
      share={
        state.kind === "placed"
          ? {
              node: state.host.node,
            values: [
              [`${live.length}/${mine.length}`, "running"],
              [formatNumber(sum(live, (x) => x.cpu)), "vCPU"],
              [formatNumber(sum(live, (x) => x.memoryGi)), "GiB mem"],
              [formatNumber(sum(mine, (x) => x.diskGi)), "GiB disk"],
            ],
          }
          : undefined
      }
    />
  );
}

function Equivalent({ m }: { m: MachineView }) {
  const [tab, setTab] = useState<"cli" | "yaml">("cli");
  const kind = m.kind === "Container" ? "container" : "vm";
  const cli = [
    `hangar get ${kind} ${m.name} -p ${m.project}`,
    `hangar logs ${kind} ${m.name} -p ${m.project} -f`,
    m.kind === "Container" ? `hangar exec -it ${kind} ${m.name} -p ${m.project} -- sh` : `hangar ssh ${kind} ${m.name} -p ${m.project}`,
    m.stopped ? `hangar start ${kind} ${m.name} -p ${m.project}` : `hangar stop ${kind} ${m.name} -p ${m.project}`,
  ];
  const obj = m.container ?? m.vm;
  const yaml = obj ? toYaml(manifestOf(obj, m.kind)) : "";
  const text = tab === "cli" ? cli.join("\n") : yaml;
  return (
    <Card className="overflow-hidden">
      <div className="flex h-11 items-center justify-between border-b px-4">
        <div role="tablist" className="flex gap-1 text-[12px]">
          {(["cli", "yaml"] as const).map((t) => (
            <button
              key={t}
              role="tab"
              aria-selected={tab === t}
              onClick={() => setTab(t)}
              className={cn("rounded px-2 py-1 font-medium", tab === t ? "bg-surface-2 text-foreground" : "text-muted hover:text-foreground")}
            >
              {t === "cli" ? "CLI" : "YAML"}
            </button>
          ))}
        </div>
        <CopyButton text={text} />
      </div>
      <pre className="max-h-96 overflow-auto whitespace-pre-wrap px-4 py-3 font-mono text-[12px] leading-5">
        {tab === "cli"
          ? cli.map((c) => (
              <div key={c} className="-indent-3.5 py-0.5 pl-3.5">
                <span className="select-none text-accent">$ </span>
                {c}
              </div>
            ))
          : yaml}
      </pre>
    </Card>
  );
}

function DetailSkeleton() {
  return (
    <div aria-busy="true" aria-label="Loading">
      <div className="space-y-3 border-b px-4 pb-5 pt-6 md:px-8">
        <Skeleton className="h-7 w-48" />
        <Skeleton className="h-3 w-80" />
      </div>
      <div className="grid grid-cols-1 gap-5 px-4 py-6 md:px-8 xl:grid-cols-[minmax(0,1fr)_380px]">
        <div className="space-y-5">
          <Card>
            <SkeletonRows rows={2} />
          </Card>
          <Card>
            <SkeletonRows rows={5} />
          </Card>
        </div>
        <Card>
          <SkeletonRows rows={4} />
        </Card>
      </div>
    </div>
  );
}

export function MachineDetail({ kind, name }: { kind: MachineKind; name: string }) {
  const project = useProjectName();
  const { items, loading, error, refetch } = useMachines(project, kind);
  const m = items.find((x) => x.name === name);
  const list = kind === "Container" ? "Containers" : "Virtual machines";
  const noun = kind === "Container" ? "container" : "virtual machine";

  const crumbs = [
    list,
    <span key="name" className="flex items-center gap-2">
      {m && <StatusDot phase={m.phase} />}
      {name}
    </span>,
  ];
  if (loading) {
    return (
      <>
        <Topbar crumbs={crumbs} />
        <DetailSkeleton />
      </>
    );
  }
  if (error) {
    return (
      <>
        <Topbar crumbs={crumbs} />
        <div className="px-4 py-6 md:px-8">
          <Card>
            <ErrorNotice error={error} title={`The ${noun} did not load`} onRetry={refetch} />
          </Card>
        </div>
      </>
    );
  }
  if (!m) {
    return (
      <>
        <Topbar crumbs={crumbs} />
        <EmptyState title={`No such ${noun}`}>
          This project has no {noun} named <span className="font-mono">{name}</span>.
        </EmptyState>
      </>
    );
  }
  const ready = readyOf(m);
  return (
    <>
      <Topbar crumbs={crumbs} />
      {m.deleting && (
        <PageBanner tone="warning" title={`${m.name} is being deleted`}>
          It goes once its machine and disks are gone.
        </PageBanner>
      )}
      {m.phase === MachinePhase.FAILED && ready?.message && (
        <PageBanner tone="error" title={`${m.name} failed: ${ready.reason}`}>
          {ready.message}
        </PageBanner>
      )}
      <Header m={m} />
      <div className="grid grid-cols-1 gap-5 px-4 py-6 md:px-8 xl:grid-cols-[minmax(0,1fr)_380px]">
        <div className="min-w-0 space-y-5">
          <Lifecycle m={m} />
          <Summary m={m} />
          <Networking m={m} />
          <Conditions m={m} />
          <Logs m={m} />
        </div>
        <div className="space-y-5">
          <RunsOn m={m} />
          <Equivalent m={m} />
        </div>
      </div>
    </>
  );
}

