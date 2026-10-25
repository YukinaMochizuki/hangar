// One container or virtual machine: where it is in its lifecycle, what it
// is, how it is reached, what it wrote, and the host it runs on.

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { MachineCapabilityService, MachineLogStream, type MachineLogRecord } from "@hangar/api/hangar/compute/v1alpha1/machine_capability_pb";
import { MachinePhase } from "@hangar/api/hangar/compute/v1alpha1/machine_pb";
import { Protocol, RestartPolicy } from "@hangar/api/hangar/compute/v1alpha1/container_pb";
import type { Condition } from "@hangar/api/hangar/meta/v1/meta_pb";
import { PublicEndpointService } from "@hangar/api/hangar/network/v1alpha1/public_endpoint_pb";
import { Link, useNavigate } from "@tanstack/react-router";
import { ArrowDown, ArrowUpRight, ChevronDown, ChevronUp, Lock, SquareChevronRight, TerminalSquare } from "lucide-react";
import { StaleBanner } from "@/components/credentials";
import { HostKeys } from "@/components/host-key";
import { TerminalCard, useMayTerminal } from "@/pages/machine-terminal";
import { consoleBlock, useMayConsole } from "@/pages/machine-console";
import type { MenuAction } from "@/components/menu";
import { useEffect, useLayoutEffect, useRef, useState, type CSSProperties, type ReactNode } from "react";
import { parseAnsi, type AnsiColor, type AnsiSpan, type AnsiStyle } from "@/lib/ansi";
import { artStatus } from "@/components/chassis-art";
import { MachineActions, useMachineActions } from "@/components/machine-actions";
import { useMachineCan } from "@/lib/permissions";
import type { Can } from "@/components/object-actions";
import { ConditionRow, ConditionsFold, EquivalentCard, LabelsField } from "@/components/object";
import { Notice, PageBanner } from "@/components/notice";
import { RunsOnPanel } from "@/components/runs-on-panel";
import { createdKey, useJustCreated } from "@/lib/just-created";
import { DetailSkeleton } from "@/components/page";
import { Topbar, useProjectName } from "@/components/shell";
import { EmptyState, ErrorNotice, SkeletonRows } from "@/components/states";
import { Taxiway, type Stop, type StopState } from "@/components/taxiway";
import { Badge, buttonVariants, Card, CardHeader, CopyButton, Field, SizeBadge, Status, StatusDot } from "@/components/ui";
import { cn } from "@/lib/cn";
import { manifestOf, toYaml } from "@/lib/manifest";
import { date, isLive, refetchInterval, uptime, useMachines, type MachineKind, type MachineView } from "@/lib/machines";
import { sizeOf, useMachineSizes } from "@/lib/machine-sizes";
import { useReducedMotion } from "@/lib/motion";
import { formatNumber } from "@/lib/quantity";
import { runsOnState } from "@/lib/runs-on";
import { bare } from "@/pages/machine-list";
import { VmFileSharesCard } from "@/components/file-shares";
import { MachinePortForwards } from "@/components/port-forwards";

const restartPolicies: Record<number, string> = {
  [RestartPolicy.UNSPECIFIED]: "Always (default)",
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

/**
 * What a VM's head adds to its "..." menu: its serial console, slower than
 * the terminal and kept for what SSH cannot reach.
 */
function useMoreActions(m: MachineView): MenuAction[] {
  const navigate = useNavigate();
  const may = useMayConsole(m.project);
  if (m.kind !== "VirtualMachine" || !may) return [];
  return [
    {
      label: "Serial console",
      icon: SquareChevronRight,
      disabled: !!consoleBlock(m),
      onSelect: () => void navigate({ to: "/projects/$project/vms/$name/console", params: { project: m.project, name: m.name } }),
    },
  ];
}

function Header({ m, terminal, set }: { m: MachineView; terminal: boolean; set: ReturnType<typeof useMachineActions> }) {
  const { sizes } = useMachineSizes();
  const size = sizeOf(m, sizes);
  const more = useMoreActions(m);
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
        <span className="flex items-center gap-2">
          {terminal && (
            <Link
              to={m.kind === "Container" ? "/projects/$project/containers/$name/terminal" : "/projects/$project/vms/$name/terminal"}
              params={{ project: m.project, name: m.name }}
              className={buttonVariants({ size: "sm" })}
            >
              <TerminalSquare /> Terminal
            </Link>
          )}
          <MachineActions set={set} more={more} />
        </span>
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
  // Held back before the scheduler sees it: not scheduled. Why is the Runs on panel's to say.
  if (runsOnState(m).kind === "heldBack") return at(0, "held", "Held back");
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

function Summary({ m, onEditLabels }: { m: MachineView; onEditLabels?: () => void }) {
  const { sizes } = useMachineSizes();
  const size = sizeOf(m, sizes);
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
        {m.vm?.spec?.platform?.serialNumber && (
          <Field label="Serial number" mono>
            {m.vm.spec.platform.serialNumber}
          </Field>
        )}
        {m.vm?.spec?.tpm?.enabled && <Field label="TPM">TPM 2.0</Field>}
        {m.vm && (
          <Field label="Host key">
            <HostKeys vm={m.vm} project={m.project} />
          </Field>
        )}
        <Field label="Run state">{m.stopped ? "Stopped" : "Running"}</Field>
        <LabelsField labels={m.labels} onEdit={onEditLabels} />
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
        <div className="px-4 py-3 text-[13px] text-muted">No interfaces yet.</div>
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

/** The raw conditions, folded away: the lifecycle says the same in plain words. */
function Conditions({ m }: { m: MachineView }) {
  const rows = [
    ...(m.container?.status?.conditions ?? m.vm?.status?.conditions ?? []).map((c) => ({ c, of: m.kind })),
    ...(m.machine?.status?.conditions ?? []).map((c) => ({ c, of: "Machine" })),
  ];
  return (
    <ConditionsFold count={rows.length}>
      {rows.map(({ c, of }) => (
        <ConditionRow key={`${of}/${c.type}`} c={c} of={of} />
      ))}
    </ConditionsFold>
  );
}

const decoder = new TextDecoder();
// How much of the log the page asks for when it opens.
const logTail = 50n;
const keptLines = 200;

export type LogLine = {
  key: number;
  time?: Date;
  stream: MachineLogStream;
  // What was written, its escape sequences gone, and in spans of the
  // styles they asked for.
  text: string;
  spans: AnsiSpan[];
  boot: number;
  // Set on the line that ends a boot: how its process ended.
  exit?: { code: number; signal: number };
};

export function toLines(r: MachineLogRecord, next: () => number): LogLine[] {
  const time = date(r.time);
  const boot = Number(r.boot);
  if (r.content.case === "exit") {
    const e = r.content.value;
    return [{ key: next(), time, stream: r.stream, text: "", spans: [], boot, exit: { code: e.code, signal: e.signal } }];
  }
  if (r.content.case !== "data") return [];
  // A style set on one line of a record goes on to the next, as in a terminal.
  let style: AnsiStyle = {};
  return decoder
    .decode(r.content.value)
    .replace(/\n$/, "")
    .split("\n")
    .map((raw) => {
      const { spans, end } = parseAnsi(raw, style);
      style = end;
      return { key: next(), time, stream: r.stream, text: spans.map((s) => s.text).join(""), spans, boot };
    });
}

/** A span's style, as CSS; the 16 colours are the page's (--ansi-N in styles.css). */
function spanStyle(s: AnsiStyle): CSSProperties | undefined {
  const color = (c: AnsiColor | undefined) => (c === undefined ? undefined : typeof c === "number" ? `var(--ansi-${c})` : c);
  let fg = color(s.fg);
  let bg = color(s.bg);
  if (s.inverse) [fg, bg] = [bg ?? "var(--surface-2)", fg ?? "var(--foreground)"];
  if (!fg && !bg && !s.bold && !s.dim && !s.italic && !s.underline) return undefined;
  return {
    color: fg ?? (s.bold ? "var(--foreground)" : undefined),
    backgroundColor: bg,
    fontWeight: s.bold ? 600 : undefined,
    opacity: s.dim ? 0.7 : undefined,
    fontStyle: s.italic ? "italic" : undefined,
    textDecoration: s.underline ? "underline" : undefined,
  };
}

/** What the machine does when its process ends, if this page can tell. */
function restartOf(m: MachineView): RestartPolicy | undefined {
  const p = m.machine?.spec?.restartPolicy ?? (m.container ? m.container.spec?.restartPolicy ?? RestartPolicy.UNSPECIFIED : undefined);
  return p === RestartPolicy.UNSPECIFIED && m.container ? RestartPolicy.ALWAYS : p || undefined;
}

const restartWords: Record<number, string> = {
  [RestartPolicy.ALWAYS]: "always",
  [RestartPolicy.ON_FAILURE]: "on failure",
  [RestartPolicy.NEVER]: "never",
};

/**
 * The line that ends a boot, said in words: how the process ended, and what
 * came of it. A later boot in the log says it booted again; for the last
 * boot, the restart policy says what follows.
 */
export function bootEnd(l: LogLine, lastBoot: number, m: MachineView): string {
  const e = l.exit!;
  const how = e.signal ? `the process was ended by signal ${e.signal} (exit ${e.code})` : `the process exited with code ${e.code}`;
  const policy = restartOf(m);
  const word = policy ? restartWords[policy] : undefined;
  let then = "";
  if (l.boot < lastBoot) then = word ? `booted again (restart: ${word})` : "booted again";
  else if (m.stopped) then = "stopped";
  else if (policy === RestartPolicy.ALWAYS || (policy === RestartPolicy.ON_FAILURE && (e.code !== 0 || e.signal))) then = `booting again (restart: ${word})`;
  else if (word) then = `stays off (restart: ${word})`;
  return [`boot ${l.boot} ended`, how, then].filter(Boolean).join(" · ");
}

// A container whose boots ended this often within this window is starting
// over, not running.
const loopEnds = 3;
const loopWindowMs = 10 * 60_000;

/** How many of the container's boots ended lately, when it looks caught in a loop of them. */
export function restartLoop(lines: LogLine[], m: MachineView, now: number): number {
  if (!m.container || m.stopped) return 0;
  const ends = lines.filter((l) => l.exit && l.time && now - l.time.getTime() < loopWindowMs).length;
  return ends >= loopEnds ? ends : 0;
}

const stamp = (t?: Date) => t?.toISOString().slice(11, 19) ?? "--:--:--";

// How near the bottom, in pixels, the log counts as scrolled to its end.
const atEndSlack = 8;

/**
 * The last of what the machine wrote, followed while the page is open, over
 * the same server stream `hangar logs -f` reads.
 */
function Logs({ m }: { m: MachineView }) {
  const [attempt, setAttempt] = useState(0);
  // A machine on no node has never booted, so it has no log to ask for.
  if (!m.node) {
    return (
      <Card className="overflow-hidden">
        <CardHeader title="Recent logs" />
        <div className="px-4 py-3 text-[13px] text-muted">No logs: {m.name} has not started.</div>
      </Card>
    );
  }
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
  // The clock the restart loop is judged by, so that boots that ended long
  // ago stop counting while the page is open.
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 30_000);
    return () => clearInterval(t);
  }, []);
  const lastBoot = lines.reduce((b, l) => Math.max(b, l.boot), 0);
  const loop = restartLoop(lines, m, now);
  // Scrolled to its end, the log stays there as lines come, as a terminal
  // does; scrolled up, it stays where it was read, and a button goes back.
  // It opens at its end.
  const pre = useRef<HTMLPreElement>(null);
  const [atEnd, setAtEnd] = useState(true);
  // The last line there was when the person scrolled up from the end.
  const [seen, setSeen] = useState(-1);
  const lastKey = lines.at(-1)?.key ?? -1;
  useLayoutEffect(() => {
    const el = pre.current;
    if (el && atEnd) el.scrollTop = el.scrollHeight;
  }, [lines, atEnd]);
  const onScroll = () => {
    const el = pre.current;
    if (!el) return;
    const end = el.scrollHeight - el.scrollTop - el.clientHeight <= atEndSlack;
    if (atEnd && !end) setSeen(lastKey);
    setAtEnd(end);
  };
  const toEnd = () => {
    const el = pre.current;
    if (el) el.scrollTop = el.scrollHeight;
    setAtEnd(true);
  };

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
        <>
          {loop > 0 && (
            <Notice tone="warning" title="This container keeps starting over" className="m-3">
              Its process ended {loop} times in {loopWindowMs / 60_000} minutes. A command that exits at once, such as a shell with no terminal, does this.
            </Notice>
          )}
          <div className="relative">
            <pre ref={pre} onScroll={onScroll} className="max-h-80 overflow-auto bg-surface-2 px-4 py-3 font-mono text-[12px] leading-6 text-muted">
              {lines.map((l) =>
                l.exit ? (
                  <div key={l.key} className="flex items-center gap-2 text-pending">
                    <span className="select-none text-subtle">{stamp(l.time)}</span>
                    <span aria-hidden className="h-px w-3 shrink-0 bg-pending/40" />
                    <span className="min-w-0 whitespace-pre-wrap">{bootEnd(l, lastBoot, m)}</span>
                    <span aria-hidden className="h-px min-w-3 flex-1 bg-pending/40" />
                  </div>
                ) : (
                  <div key={l.key} className={cn("whitespace-pre-wrap break-words", l.stream === MachineLogStream.STDERR && "text-fail")}>
                    <span className="select-none text-subtle">{stamp(l.time)}</span>{" "}
                    {l.spans.map((s, i) => (
                      <span key={i} style={spanStyle(s.style)}>
                        {s.text}
                      </span>
                    ))}
                  </div>
                ),
              )}
            </pre>
            {!atEnd && (
              <button
                type="button"
                onClick={toEnd}
                className="absolute bottom-3 right-5 flex h-7 items-center gap-1.5 rounded-full border bg-surface px-3 text-[12px] text-muted shadow-sm hover:text-foreground"
              >
                <ArrowDown className="size-3.5" />
                {lastKey > seen ? "New lines" : "Latest"}
              </button>
            )}
          </div>
        </>
      )}
    </Card>
  );
}

function RunsOn({ m }: { m: MachineView }) {
  const all = useMachines(m.project);
  // A machine this console just created plays its arrival once, on the page
  // the create form opened; the mark is taken so a later visit plays nothing.
  const fresh = useJustCreated(createdKey(m.project, m.kind, m.name));
  const state = runsOnState(m);
  const mine = all.items.filter((x) => x.node === m.node);
  const live = mine.filter((x) => isLive(x) && !x.deleting);
  const sum = (xs: MachineView[], f: (x: MachineView) => number) => xs.reduce((s, x) => s + (Number.isFinite(f(x)) ? f(x) : 0), 0);
  return (
    <RunsOnPanel
      state={state}
      status={artStatus(m.phase)}
      fresh={fresh}
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
  const kind = m.kind === "Container" ? "container" : "vm";
  const cli = [
    `hangar get ${kind} ${m.name} -p ${m.project}`,
    `hangar logs ${kind} ${m.name} -p ${m.project} -f`,
    m.kind === "Container" ? `hangar exec -it ${kind} ${m.name} -p ${m.project} -- sh` : `hangar ssh ${kind} ${m.name} -p ${m.project}`,
    m.stopped ? `hangar start ${kind} ${m.name} -p ${m.project}` : `hangar stop ${kind} ${m.name} -p ${m.project}`,
  ];
  const obj = m.container ?? m.vm;
  const yaml = obj ? toYaml(manifestOf(obj, m.kind)) : "";
  return <EquivalentCard cli={cli} yaml={yaml} />;
}

export function MachineDetail({ kind, name }: { kind: MachineKind; name: string }) {
  const project = useProjectName();
  const { held, loading, error, refetch } = useMachines(project, kind);
  const mayTerminal = useMayTerminal(project);
  // Asked with the list, so the page's controls come with it, and once.
  const { can } = useMachineCan(project);
  // Asking again failing keeps the page on its last answer, so a terminal open on it is not ended.
  const m = held.find((x) => x.name === name);
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
  if (error && !m) {
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
  return <Loaded m={m} crumbs={crumbs} stale={error} refetch={refetch} terminal={mayTerminal[m.kind]} can={can} />;
}

/** The page of a machine that has loaded, its actions made once for its head and its labels. */
function Loaded({
  m,
  crumbs,
  stale,
  refetch,
  terminal,
  can,
}: {
  m: MachineView;
  crumbs: ReactNode[];
  stale: unknown;
  refetch: () => void;
  terminal: boolean;
  can: Can;
}) {
  const navigate = useNavigate();
  const noun = m.kind === "Container" ? "container" : "virtual machine";
  const list = m.kind === "Container" ? "/projects/$project/containers" : "/projects/$project/vms";
  const set = useMachineActions(m, can, () => void navigate({ to: list, params: { project: m.project } }));
  const ready = readyOf(m);
  return (
    <>
      <Topbar crumbs={crumbs} />
      {stale ? <StaleBanner what={`${noun}s`} error={stale} onRetry={refetch} /> : null}
      {m.deleting && (
        <PageBanner tone="warning" title={`${m.name} is being deleted`} />
      )}
      {m.phase === MachinePhase.FAILED && ready?.message && (
        <PageBanner tone="error" title={`${m.name} failed: ${ready.reason}`}>
          {ready.message}
        </PageBanner>
      )}
      <Header m={m} terminal={terminal} set={set} />
      <div className="grid grid-cols-1 gap-5 px-4 py-6 md:px-8 xl:grid-cols-[minmax(0,1fr)_380px]">
        <div className="min-w-0 space-y-5">
          <Lifecycle m={m} />
          {terminal && <TerminalCard m={m} />}
          <Summary m={m} onEditLabels={set.editLabels?.onSelect} />
          <Networking m={m} />
          <MachinePortForwards m={m} />
          {m.kind === "VirtualMachine" && <VmFileSharesCard m={m} />}
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

