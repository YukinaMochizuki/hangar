import { Link } from "@tanstack/react-router";
import { ArrowRight, Plus } from "lucide-react";
import { ChassisArt } from "@/components/chassis-art";
import { Meter } from "@/components/meter";
import { PageBanner } from "@/components/notice";
import { Topbar } from "@/components/shell";
import { Button, Card, CardHeader, CopyButton, SizeBadge, StatusDot, phaseLabel } from "@/components/ui";
import { cn } from "@/lib/cn";
import {
  activity,
  currentProject,
  footprintOn,
  hostOf,
  machines,
  quota,
  sizes,
  tenant,
} from "@/mock/data";

function QuotaTiles() {
  return (
    <div className="grid grid-cols-2 gap-4 lg:grid-cols-4">
      {quota.compute.map((q) => (
        <Card key={q.key} className="px-4 py-3.5">
          <div className="text-[12px] text-muted">{q.label}</div>
          <div className="mt-1 flex items-baseline gap-1.5">
            <span className="font-mono text-[22px] font-medium tabular tracking-tight">{q.used}</span>
            <span className="font-mono text-[13px] text-subtle tabular">
              / {q.limit}
              {q.unit && ` ${q.unit}`}
            </span>
          </div>
          <Meter used={q.used} limit={q.limit} className="mt-3" />
        </Card>
      ))}
    </div>
  );
}

function MachineTable() {
  return (
    <Card>
      <CardHeader
        title="Machines"
        action={
          <Link to="/containers" className="flex items-center gap-1 text-[12px] text-muted hover:text-foreground">
            View all <ArrowRight className="size-3" />
          </Link>
        }
      />
      <ul className="divide-y">
        {machines.map((m) => (
          <li key={m.name}>
            <Link
              to={m.kind === "Container" ? "/containers/$name" : "/vms/$name"}
              params={{ name: m.name }}
              className="grid grid-cols-[minmax(0,1fr)_auto] items-center gap-4 px-4 py-2.5 text-[13px] hover:bg-surface-2 md:grid-cols-[minmax(0,1fr)_88px_150px_96px]"
            >
              <span className="flex min-w-0 items-center gap-2.5">
                <StatusDot phase={m.phase} />
                <span className="shrink-0 font-medium">{m.name}</span>
                <span className="hidden truncate font-mono text-[11.5px] text-subtle md:block">{m.image}</span>
              </span>
              <span className="text-muted">{phaseLabel(m.phase)}</span>
              <span className="hidden items-center gap-2 text-muted md:flex">
                <SizeBadge size={sizes[m.size].label} />
                <span className="tabular">
                  {sizes[m.size].cpu} vCPU · {sizes[m.size].memoryGi} GiB
                </span>
              </span>
              <span className="hidden text-right text-[12px] text-subtle md:block">
                {m.kind === "Container" ? "Container" : "VM"}
              </span>
            </Link>
          </li>
        ))}
      </ul>
    </Card>
  );
}

function HostCard() {
  const node = machines[0].node;
  const host = hostOf(node)!;
  const fp = footprintOn(node);
  return (
    <Card className="overflow-hidden">
      <CardHeader title="Your machines run on" action={<span className="font-mono text-[12px] text-muted">{host.node}</span>} />
      <div className="bg-[radial-gradient(ellipse_at_50%_0%,var(--accent-soft),transparent_70%)] px-4 md:px-8 pb-2 pt-5">
        {/* The host itself, not any one machine: its LEDs show it serving. */}
        <ChassisArt chassis={host.chassis} status="running" />
      </div>
      <div className="border-t px-4 py-3">
        <div className="text-[14px] font-medium">{host.cpuModel}</div>
        <div className="text-[12.5px] text-muted">
          {host.region} · {host.site}
        </div>
      </div>
      <div className="grid grid-cols-3 border-t text-center">
        {[
          [`${fp.running}`, "running"],
          [`${fp.cpu}`, "vCPU"],
          [`${fp.memoryGi} GiB`, "memory"],
        ].map(([v, l], i) => (
          <div key={l} className={cn("py-2.5", i > 0 && "border-l")}>
            <div className="font-mono text-[14px] font-medium tabular">{v}</div>
            <div className="text-[11px] text-muted">{l}</div>
          </div>
        ))}
      </div>
    </Card>
  );
}

const toneDot = { ok: "bg-ok", pending: "bg-pending", fail: "bg-fail", neutral: "bg-off" } as const;

function Activity() {
  return (
    <Card>
      <CardHeader title="Activity" />
      <ul className="divide-y">
        {activity.map((a, i) => (
          <li key={i} className="flex items-center gap-3 px-4 py-2.5 text-[13px]">
            <span className={cn("size-1.5 shrink-0 rounded-full", toneDot[a.tone])} />
            <span className="min-w-0 flex-1 truncate">
              <span className="hidden font-mono text-[12px] text-muted sm:inline">{a.actor}</span> {a.verb}{" "}
              <span className="font-mono text-[12px]">{a.object}</span>
            </span>
            <span className="shrink-0 text-[12px] text-subtle">{a.ago} ago</span>
          </li>
        ))}
      </ul>
    </Card>
  );
}

function OtherQuota() {
  return (
    <Card>
      <CardHeader title="Other limits" />
      <ul className="space-y-3 px-4 py-3.5">
        {quota.other.map((q) => (
          <li key={q.key} className="text-[13px]">
            <div className="flex justify-between">
              <span className="text-muted">{q.label}</span>
              <span className="font-mono text-[12px] tabular">
                {q.used} / {q.limit}
              </span>
            </div>
            <Meter used={q.used} limit={q.limit} className="mt-1.5" />
          </li>
        ))}
      </ul>
    </Card>
  );
}

function FromTheCli() {
  const lines = [`hangar login`, `hangar get containers -p ${currentProject.name}`];
  return (
    <Card className="overflow-hidden">
      <CardHeader title="From the CLI" action={<CopyButton text={lines.join("\n")} />} />
      <pre className="px-4 py-3 font-mono text-[12px] leading-6">
        {lines.map((l) => (
          <div key={l}>
            <span className="select-none text-accent">$ </span>
            {l}
          </div>
        ))}
      </pre>
    </Card>
  );
}

export function Overview({ banner }: { banner?: "host" }) {
  return (
    <>
      <Topbar crumbs={[tenant.name, currentProject.displayName, "Overview"]} />
      {banner === "host" && (
        <PageBanner tone="warning" title="sgp02-01 stopped reporting 4 minutes ago">
          The 5 machines on it may be running, but their status is not current.
        </PageBanner>
      )}
      <div className="flex items-end justify-between gap-6 px-4 md:px-8 pb-5 pt-6">
        <div>
          <h1 className="text-[22px] font-semibold tracking-tight">{currentProject.displayName}</h1>
          <div className="mt-1 flex items-center gap-1 font-mono text-[12px] text-muted">
            {currentProject.name}
            <CopyButton text={currentProject.name} />
          </div>
        </div>
        <Link to="/new">
          <Button variant="primary">
            <Plus /> New machine
          </Button>
        </Link>
      </div>
      <div className="space-y-5 px-4 md:px-8 pb-10">
        <QuotaTiles />
        <div className="grid grid-cols-1 gap-5 xl:grid-cols-[minmax(0,1fr)_380px]">
          <div className="min-w-0 space-y-5">
            <MachineTable />
            <Activity />
          </div>
          <div className="space-y-5">
            <HostCard />
            <OtherQuota />
            <FromTheCli />
          </div>
        </div>
      </div>
    </>
  );
}
