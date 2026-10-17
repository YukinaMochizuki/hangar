import {
  ArrowUpRight,
  ChevronDown,
  ChevronRight,
  ChevronUp,
  Lock,
  MoreHorizontal,
  Play,
  RotateCw,
  Square,
  SquareTerminal,
} from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { artStatus, ChassisArt } from "@/components/chassis-art";
import { Topbar } from "@/components/shell";
import { Taxiway, type Stop, type StopState } from "@/components/taxiway";
import { Badge, Button, Card, CardHeader, CopyButton, Field, SizeBadge, Status, StatusDot } from "@/components/ui";
import { cn } from "@/lib/cn";
import {
  currentProject,
  footprintOn,
  hostOf,
  logTail,
  MachinePhase,
  sizes,
  tenant,
  type MachineView,
} from "@/mock/data";

const tabs = ["Overview", "Logs", "Shell", "Networking", "Settings", "YAML"];

function Header({ m }: { m: MachineView }) {
  const stopped = m.phase === MachinePhase.STOPPED;
  return (
    <div className="border-b px-4 md:px-8 pb-0 pt-6">
      <div className="flex flex-col gap-4 md:flex-row md:items-start md:justify-between md:gap-6">
        <div className="min-w-0">
          <div className="flex items-center gap-2.5">
            <h1 className="truncate text-[22px] font-semibold tracking-tight">{m.name}</h1>
            <Badge>{m.kind === "Container" ? "Container" : "Virtual machine"}</Badge>
          </div>
          <div className="mt-2 flex flex-wrap items-center gap-x-5 gap-y-1 text-[13px] text-muted">
            <Status phase={m.phase} />
            {m.uptime && <span>up {m.uptime}</span>}
            <span className="flex items-center gap-1.5">
              <SizeBadge size={sizes[m.size].label} />
              <span className="tabular">
                {sizes[m.size].cpu} vCPU · {sizes[m.size].memoryGi} GiB · {sizes[m.size].diskGi} GiB disk
              </span>
            </span>
            <span className="font-mono text-[12px]">{m.interfaces[0]?.ipv4}</span>
          </div>
        </div>
        <div className="flex shrink-0 flex-wrap items-center gap-2">
          <Button>
            <SquareTerminal /> Open shell
          </Button>
          <Button>
            <RotateCw /> Restart
          </Button>
          {stopped ? (
            <Button variant="primary">
              <Play /> Start
            </Button>
          ) : (
            <Button>
              <Square /> Stop
            </Button>
          )}
          <Button size="icon" variant="ghost" aria-label="More">
            <MoreHorizontal />
          </Button>
        </div>
      </div>
      <nav className="mt-5 flex gap-5 overflow-x-auto whitespace-nowrap text-[13px]">
        {tabs.map((t, i) => (
          <span
            key={t}
            className={cn(
              "-mb-px border-b-2 pb-2.5",
              i === 0 ? "border-accent font-medium text-foreground" : "border-transparent text-muted",
            )}
          >
            {t}
          </span>
        ))}
      </nav>
    </div>
  );
}

function Summary({ m }: { m: MachineView }) {
  return (
    <Card>
      <CardHeader title="Summary" />
      <dl className="divide-y px-4">
        <Field label="Image" mono>
          {m.image}
          {m.imageDigest && <div className="mt-0.5 text-subtle">{m.imageDigest.slice(0, 19)}…</div>}
        </Field>
        <Field label="Size">
          <span className="flex items-center gap-2">
            <SizeBadge size={sizes[m.size].label} />
            <span className="tabular">
              {sizes[m.size].cpu} vCPU · {sizes[m.size].memoryGi} GiB memory · {sizes[m.size].diskGi} GiB root disk
            </span>
          </span>
        </Field>
        <Field label="Restart policy">{m.restartPolicy}</Field>
        <Field label="Labels">
          <span className="flex flex-wrap gap-1.5">
            {Object.keys(m.labels).length === 0 && <span className="text-subtle">None</span>}
            {Object.entries(m.labels).map(([k, v]) => (
              <span key={k} className="rounded bg-surface-2 px-1.5 py-0.5 font-mono text-[11.5px]">
                {k}=<span className="text-muted">{v}</span>
              </span>
            ))}
          </span>
        </Field>
        <Field label="Created">{m.created}</Field>
        <Field label="UID" mono>
          <span className="text-muted">{m.uid}</span>
        </Field>
      </dl>
    </Card>
  );
}

function Networking({ m }: { m: MachineView }) {
  return (
    <Card>
      <CardHeader title="Networking" action={<Button size="sm" variant="ghost">Manage</Button>} />
      <div className="overflow-x-auto">
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
              <td className="px-4 py-2.5 font-mono text-[12.5px]">{i.ipv4}</td>
              <td className="px-4 py-2.5 font-mono text-[12.5px]">
                {i.ipv6 ? (
                  <span className="flex items-center gap-1">
                    {i.ipv6}
                    <CopyButton text={i.ipv6} />
                  </span>
                ) : (
                  <span className="text-subtle">—</span>
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
      {m.ports.length > 0 && (
        <div className="border-t px-4 py-3">
          <div className="mb-2 text-[11.5px] font-medium text-subtle">Ports</div>
          <ul className="space-y-1.5">
            {m.ports.map((p) => (
              <li key={p.name} className="flex items-center gap-3 text-[13px]">
                <span className="w-24 font-mono text-[12.5px]">
                  {p.port}/{p.protocol.toLowerCase()}
                </span>
                <span className="hidden w-20 text-muted sm:block">{p.name}</span>
                {p.publicHost ? (
                  <a className="flex min-w-0 items-center gap-1 text-accent hover:underline">
                    <Lock className="size-3 shrink-0" />
                    <span className="truncate font-mono text-[12.5px]">https://{p.publicHost}</span>
                    <ArrowUpRight className="size-3 shrink-0" />
                  </a>
                ) : (
                  <span className="text-subtle">Not public</span>
                )}
              </li>
            ))}
          </ul>
        </div>
      )}
    </Card>
  );
}

/** The raw conditions, folded away: the lifecycle above says the same thing in plain words. */
function Conditions({ m }: { m: MachineView }) {
  return (
    <Card className="overflow-hidden">
      <details className="group">
        <summary className="flex h-11 cursor-pointer list-none items-center gap-2 px-4 text-[13px] font-medium [&::-webkit-details-marker]:hidden">
          <ChevronRight className="size-3.5 text-subtle transition-transform group-open:rotate-90" />
          Conditions
          <span className="font-mono text-[11.5px] font-normal text-subtle">{m.conditions.length}</span>
        </summary>
      <ul className="divide-y border-t">
        {m.conditions.map((c) => (
          <li key={c.type} className="flex items-center gap-3 px-4 py-2.5 text-[13px]">
            <span
              className={cn(
                "size-1.5 rounded-full",
                c.status === "True"
                  ? "bg-ok"
                  : c.status === "Unknown"
                    ? "bg-pending"
                    : c.type === "Ready" && m.phase === MachinePhase.FAILED
                      ? "bg-fail"
                      : "bg-subtle",
              )}
            />
            <span className="w-24 font-medium">{c.type}</span>
            <span className="w-14 font-mono text-[12px] text-muted">{c.status}</span>
            <span className="font-mono text-[12px] text-muted">{c.reason}</span>
            <span className="hidden min-w-0 flex-1 truncate text-muted md:block">{c.message}</span>
            <span className="flex-1 md:hidden" />
            <span className="text-[12px] text-subtle">{c.ago} ago</span>
          </li>
        ))}
      </ul>
      </details>
    </Card>
  );
}

function Logs() {
  return (
    <Card className="overflow-hidden">
      <CardHeader
        title="Recent logs"
        action={
          <span className="flex items-center gap-2 text-[12px] text-muted">
            <span className="size-1.5 animate-pulse rounded-full bg-ok" /> Following
          </span>
        }
      />
      <pre className="overflow-x-auto bg-surface-2 px-4 py-3 font-mono text-[12px] leading-6 text-muted">
        {logTail.map((l) => {
          const [ts, ...rest] = l.split("  ");
          return (
            <div key={l}>
              <span className="text-subtle">{ts.slice(11, 19)}</span> {rest.join("  ")}
            </div>
          );
        })}
      </pre>
    </Card>
  );
}

function RunsOn({ m }: { m: MachineView }) {
  const host = hostOf(m.node);
  if (!host) return null;
  const fp = footprintOn(m.node);
  return (
    <Card className="overflow-hidden">
      <CardHeader title="Runs on" action={<span className="font-mono text-[12px] text-muted">{host.node}</span>} />
      <div className="relative border-b bg-[radial-gradient(ellipse_at_50%_0%,var(--accent-soft),transparent_70%)] px-6 pb-3 pt-6">
        <ChassisArt chassis={host.chassis} status={artStatus(m.phase)} />
      </div>
      <div className="px-4 pt-3">
        <div className="text-[15px] font-medium">{host.cpuModel}</div>
        <div className="text-[12.5px] text-muted">{host.cpuDetail}</div>
      </div>
      <dl className="px-4 pb-1 pt-2">
        <Field label="Region">
          {host.region} <span className="text-muted">· {host.site}</span>
        </Field>
        <Field label="Memory">{host.memory}</Field>
        <Field label="Storage">{host.storage}</Field>
        <Field label="Network">{host.network}</Field>
      </dl>
      <div className="mx-4 mb-4 rounded-md border bg-surface-2 px-3 py-2.5">
        <div className="text-[11.5px] font-medium text-subtle">This project on {host.node}</div>
        <div className="mt-1.5 grid grid-cols-4 gap-2 text-center">
          {[
            [`${fp.running}/${fp.machines}`, "running"],
            [String(fp.cpu), "vCPU"],
            [`${fp.memoryGi}`, "GiB mem"],
            [`${fp.diskGi}`, "GiB disk"],
          ].map(([v, l]) => (
            <div key={l}>
              <div className="font-mono text-[15px] font-medium tabular">{v}</div>
              <div className="text-[11px] text-muted">{l}</div>
            </div>
          ))}
        </div>
      </div>
    </Card>
  );
}

function Equivalent({ m }: { m: MachineView }) {
  const [tab, setTab] = useState<"cli" | "yaml">("cli");
  const kind = m.kind === "Container" ? "container" : "vm";
  const cli = [
    `hangar get ${kind} ${m.name} -p ${m.project}`,
    `hangar logs ${kind} ${m.name} -p ${m.project} -f`,
    m.kind === "Container"
      ? `hangar exec -it ${kind} ${m.name} -p ${m.project} -- sh`
      : `hangar ssh ${kind} ${m.name} -p ${m.project}`,
  ];
  const yaml = `apiVersion: compute.hangarlab.cloud/v1alpha1
kind: ${m.kind}
metadata:
  name: ${m.name}
  project: ${m.project}
spec:
  image: ${m.image}
  resources: { cpu: "${sizes[m.size].cpu}", memory: ${sizes[m.size].memoryGi}Gi }`;
  const text = tab === "cli" ? cli.join("\n") : yaml;
  return (
    <Card className="overflow-hidden">
      <div className="flex h-11 items-center justify-between border-b px-4">
        <div className="flex gap-1 text-[12px]">
          {(["cli", "yaml"] as const).map((t) => (
            <button
              key={t}
              onClick={() => setTab(t)}
              className={cn(
                "rounded px-2 py-1 font-medium",
                tab === t ? "bg-surface-2 text-foreground" : "text-muted hover:text-foreground",
              )}
            >
              {t === "cli" ? "CLI" : "YAML"}
            </button>
          ))}
        </div>
        <CopyButton text={text} />
      </div>
      <pre className="overflow-x-auto whitespace-pre-wrap px-4 py-3 font-mono text-[12px] leading-5">
        {tab === "cli"
          ? cli.map((c) => (
              <div key={c} className="py-0.5 pl-3.5 -indent-3.5">
                <span className="select-none text-accent">$ </span>
                {c}
              </div>
            ))
          : yaml}
      </pre>
    </Card>
  );
}

/** Where the machine is on its way from scheduled to running. */
function lifecycle(m: MachineView): Stop[] {
  const labels =
    m.kind === "Container"
      ? ["Scheduled", "Image pulled", "Booted", "Running"]
      : ["Scheduled", "Disk ready", "Booted", "Running"];
  const ready = m.conditions.find((c) => c.type === "Ready");
  const at = (i: number, state: StopState, note?: string): Stop[] =>
    labels.map((label, j) =>
      j < i ? { label, state: "done" } : j === i ? { label, state, note } : { label, state: "todo" },
    );
  switch (m.phase) {
    case MachinePhase.RUNNING:
      return labels.map((label, j) => ({
        label,
        state: "done",
        note: j === 0 ? m.node : j === labels.length - 1 && m.uptime ? `up ${m.uptime}` : undefined,
      }));
    case MachinePhase.CREATING:
      return at(ready?.reason === "PullingImage" ? 1 : 2, "current", ready?.reason);
    case MachinePhase.PENDING:
      return at(0, "current", "Waiting for a host");
    case MachinePhase.FAILED:
      return at(3, "failed", ready?.reason);
    default:
      return [...labels.slice(0, 3).map((label) => ({ label, state: "done" as const })), { label: "Stopped", state: "idle" }];
  }
}

/**
 * The lifecycle while a machine is on its way, folded to one line once it
 * runs: before Running its stops are what matters, after it they are history.
 * A machine already running opens folded; one that arrives while watched
 * pulls its earlier stops into Running a moment after it gets there, then
 * folds.
 */
function Lifecycle({ m }: { m: MachineView }) {
  const running = m.phase === MachinePhase.RUNNING;
  const [folded, setFolded] = useState(running);
  const [gather, setGather] = useState(false);
  const wasRunning = useRef(running);
  useEffect(() => {
    if (running === wasRunning.current) return;
    wasRunning.current = running;
    if (!running) {
      setGather(false);
      setFolded(false);
      return;
    }
    const ts = [setTimeout(() => setGather(true), 1400), setTimeout(() => setFolded(true), 1400 + 760)];
    return () => ts.forEach(clearTimeout);
  }, [running]);
  const unfold = () => {
    setGather(false);
    setFolded(false);
  };

  return (
    <Card className="overflow-hidden">
      <div
        className={cn(
          "grid transition-[grid-template-rows,opacity] duration-500 ease-in-out",
          folded ? "grid-rows-[0fr] opacity-0" : "grid-rows-[1fr] opacity-100",
        )}
      >
        <div className="min-h-0 overflow-hidden">
          <div className="relative px-5 pb-4 pt-5">
            <Taxiway stops={lifecycle(m)} gather={gather} />
            {running && !gather && (
              <button
                onClick={() => setFolded(true)}
                className="absolute right-3 top-3 rounded p-1 text-subtle hover:bg-surface-2 hover:text-foreground"
                aria-label="Fold"
              >
                <ChevronUp className="size-3.5" />
              </button>
            )}
          </div>
        </div>
      </div>
      <div
        className={cn(
          "grid transition-[grid-template-rows,opacity] duration-500 ease-in-out",
          folded ? "grid-rows-[1fr] opacity-100" : "grid-rows-[0fr] opacity-0",
        )}
      >
        <div className="min-h-0 overflow-hidden">
          <div className="flex items-center gap-3 px-5 py-3 text-[13px]">
            {/* The end of the taxi line, arrived. */}
            <span className="flex items-center">
              <span className="h-0.5 w-5 rounded-l bg-accent" />
              <span className="size-3 rounded-full bg-accent" />
            </span>
            <span className="font-medium">Running</span>
            <span className="min-w-0 truncate text-muted">
              {m.uptime && <>up {m.uptime} · </>}on <span className="font-mono text-[12px]">{m.node}</span>
            </span>
            <button
              onClick={unfold}
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

/** Plays a boot on a mock machine: pulling, booting, then running. */
function useSimulatedBoot(m: MachineView, simulate: boolean): MachineView {
  const [step, setStep] = useState(0);
  useEffect(() => {
    if (!simulate) return;
    const ts = [setTimeout(() => setStep(1), 1800), setTimeout(() => setStep(2), 3600)];
    return () => ts.forEach(clearTimeout);
  }, [simulate]);
  if (!simulate) return m;
  const ready = (reason: string, message?: string) => [
    { type: "Scheduled", status: "True" as const, reason: "Placed", ago: "14s" },
    { type: "Ready", status: step === 2 ? ("True" as const) : ("False" as const), reason, message, ago: "2s" },
  ];
  if (step === 0) return { ...m, phase: MachinePhase.CREATING, uptime: undefined, conditions: ready("PullingImage", `Pulling ${m.image}`) };
  if (step === 1) return { ...m, phase: MachinePhase.CREATING, uptime: undefined, conditions: ready("Booting") };
  return { ...m, phase: MachinePhase.RUNNING, uptime: "0m", conditions: ready("Running") };
}

export function MachineDetail({ m: base, simulate = false }: { m: MachineView; simulate?: boolean }) {
  const m = useSimulatedBoot(base, simulate);
  return (
    <>
      <Topbar
        crumbs={[
          tenant.name,
          currentProject.displayName,
          m.kind === "Container" ? "Containers" : "Virtual machines",
          <span className="flex items-center gap-2">
            <StatusDot phase={m.phase} />
            {m.name}
          </span>,
        ]}
      />
      <Header m={m} />
      <div className="grid grid-cols-1 gap-5 px-4 md:px-8 py-6 xl:grid-cols-[minmax(0,1fr)_380px]">
        <div className="min-w-0 space-y-5">
          <Lifecycle m={m} />
          <Summary m={m} />
          <Networking m={m} />
          <Conditions m={m} />
          <Logs />
        </div>
        <div className="space-y-5">
          <RunsOn m={m} />
          <Equivalent m={m} />
        </div>
      </div>
    </>
  );
}
