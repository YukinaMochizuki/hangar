// One disk: what it holds, how large it is, where it lives and which machine
// has it.

import { Link, useNavigate } from "@tanstack/react-router";
import type { Disk } from "@hangar/api/hangar/storage/v1alpha1/disk_pb";
import { HardDrive, Trash2 } from "lucide-react";
import { useState } from "react";
import { PageBanner } from "@/components/notice";
import { DetailSkeleton } from "@/components/page";
import { Topbar, useProjectName } from "@/components/shell";
import { EmptyState, ErrorNotice } from "@/components/states";
import {
  ConditionsCard,
  DeleteDiskDialog,
  DiskSource,
  DiskState,
  diskYaml,
  EquivalentCard,
  GrowDiskDialog,
  MachineLink,
} from "@/components/storage";
import { Badge, Button, Card, CardHeader, Field } from "@/components/ui";
import { date } from "@/lib/machines";
import { useCan } from "@/lib/permissions";
import { diskFault, diskImage, gib, growthBlocked, rootDiskOwner, useDisks, useImages } from "@/lib/storage";
import { DiskSize } from "@/pages/disk-list";

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

function Header({ d }: { d: Disk }) {
  const [growing, setGrowing] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const navigate = useNavigate();
  const project = d.metadata?.project ?? "";
  const images = useImages();
  const image = images.items.find((i) => i.metadata?.name === diskImage(d));
  // Each action shows only once the caller is known to hold its permission.
  const { can } = useCan({ level: "project", name: project }, ["storage.disks.apply", "storage.disks.delete"]);
  const canApply = can("storage.disks.apply");
  const canDelete = can("storage.disks.delete");
  return (
    <div className="border-b px-4 pb-5 pt-6 md:px-8">
      <div className="flex flex-col gap-4 md:flex-row md:items-start md:justify-between md:gap-6">
        <div className="min-w-0">
          <div className="flex items-center gap-2.5">
            <h1 className="truncate text-[22px] font-semibold tracking-tight">{d.metadata?.name}</h1>
            <Badge>Disk</Badge>
          </div>
          <div className="mt-2 flex flex-wrap items-center gap-x-5 gap-y-1 text-[13px] text-muted">
            <span>
              <DiskState d={d} reason={false} />
            </span>
            <span className="tabular">{gib(d.status?.size || d.spec?.size)}</span>
            {d.status?.machine && (
              <span>
                on <MachineLink project={project} machine={d.status.machine} />
              </span>
            )}
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          {canApply && (
            <Button onClick={() => setGrowing(true)} disabled={!!d.metadata?.deleteTime}>
              <HardDrive /> Grow
            </Button>
          )}
          {canDelete && (
            <Button variant="danger" onClick={() => setDeleting(true)} disabled={!!d.metadata?.deleteTime}>
              <Trash2 /> Delete
            </Button>
          )}
        </div>
      </div>
      <GrowDiskDialog key={String(growing)} d={d} image={image} open={growing} onClose={() => setGrowing(false)} />
      <DeleteDiskDialog
        d={d}
        open={deleting}
        onClose={() => setDeleting(false)}
        after={() => void navigate({ to: "/projects/$project/disks", params: { project } })}
      />
    </div>
  );
}

function Summary({ d }: { d: Disk }) {
  const project = d.metadata?.project ?? "";
  const owner = rootDiskOwner(d);
  return (
    <Card>
      <CardHeader title="Summary" />
      <dl className="divide-y px-4">
        <Field label="Holds">
          <DiskSource d={d} project={project} />
        </Field>
        <Field label="Size">
          <DiskSize d={d} />
        </Field>
        <Field label="Attached to">
          {d.status?.machine ? <MachineLink project={project} machine={d.status.machine} /> : <span className="text-subtle">No machine</span>}
        </Field>
        {owner && (
          <Field label="Made for">
            <span>
              <Link to="/projects/$project/vms/$name" params={{ project, name: owner }} className="font-mono text-[12.5px] text-accent hover:underline">
                {owner}
              </Link>
              <span className="text-muted">, as its root disk; deleted with it</span>
            </span>
          </Field>
        )}
        <Field label="Node" mono>
          {d.status?.node || <span className="font-sans text-subtle">Not placed yet</span>}
        </Field>
        <Field label="Labels">
          <Labels labels={d.metadata?.labels ?? {}} />
        </Field>
        <Field label="Created">{date(d.metadata?.createTime)?.toISOString().replace("T", " ").slice(0, 16)} UTC</Field>
        <Field label="UID" mono>
          <span className="text-muted">{d.metadata?.uid}</span>
        </Field>
      </dl>
    </Card>
  );
}

/** DiskDetail is one disk's page, with Grow and Delete. */
export function DiskDetail({ name }: { name: string }) {
  const project = useProjectName();
  const { items, loading, error, refetch } = useDisks(project);
  const d = items.find((x) => x.metadata?.name === name);
  const crumbs = ["Disks", name];
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
            <ErrorNotice error={error} title="The disk did not load" onRetry={refetch} />
          </Card>
        </div>
      </>
    );
  }
  if (!d) {
    return (
      <>
        <Topbar crumbs={crumbs} />
        <EmptyState title="No such disk">
          This project has no disk named <span className="font-mono">{name}</span>.
        </EmptyState>
      </>
    );
  }
  const fault = diskFault(d);
  const blocked = growthBlocked(d);
  const cli = [`hangar get disk ${name} -p ${project}`, `hangar apply -f disk.yaml -p ${project}`, `hangar delete disk ${name} -p ${project}`];
  return (
    <>
      <Topbar crumbs={crumbs} />
      {d.metadata?.deleteTime && (
        <PageBanner tone="warning" title={`${name} is being deleted`}>
          It goes once no machine lists it and its node has released it.
        </PageBanner>
      )}
      {blocked && (
        <PageBanner tone="error" title={`${name} is not growing: ${blocked.reason}`}>
          {blocked.message}
        </PageBanner>
      )}
      {fault && (
        <PageBanner tone="error" title={`${name} is not ready: ${fault.reason}`}>
          {fault.message}
        </PageBanner>
      )}
      <Header d={d} />
      <div className="grid grid-cols-1 gap-5 px-4 py-6 md:px-8 xl:grid-cols-[minmax(0,1fr)_380px]">
        <div className="min-w-0 space-y-5">
          <Summary d={d} />
          <ConditionsCard conditions={d.status?.conditions ?? []} />
        </div>
        <div className="space-y-5">
          <EquivalentCard cli={cli} yaml={diskYaml(d)} />
        </div>
      </div>
    </>
  );
}
