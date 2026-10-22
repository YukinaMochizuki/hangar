// One disk: what it holds, how large it is, where it lives and which machine
// has it.

import { Link, useNavigate } from "@tanstack/react-router";
import type { Disk } from "@hangar/api/hangar/storage/v1alpha1/disk_pb";
import { useDiskActions, useDiskCan } from "@/components/disk-actions";
import { PageBanner } from "@/components/notice";
import { HeadActions, type ActionSet, type Can } from "@/components/object-actions";
import { DetailSkeleton } from "@/components/page";
import { Topbar, useProjectName } from "@/components/shell";
import { EmptyState, ErrorNotice } from "@/components/states";
import { DiskSource, DiskState, diskYaml, MadeAs, UsedAs } from "@/components/storage";
import { ConditionsCard, EquivalentCard, LabelsField } from "@/components/object";
import { Badge, Card, CardHeader, Field } from "@/components/ui";
import { date } from "@/lib/machines";
import { diskFault, gib, growthBlocked, rootDiskOwner, usedAs, useDisks } from "@/lib/storage";
import { DiskSize } from "@/pages/disk-list";

function Header({ d, set }: { d: Disk; set: ActionSet }) {
  const project = d.metadata?.project ?? "";
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
            {usedAs(d) && (
              <span>
                <UsedAs d={d} project={project} />
              </span>
            )}
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <HeadActions set={set} />
        </div>
      </div>
      {set.dialogs}
    </div>
  );
}

function Summary({ d, set }: { d: Disk; set: ActionSet }) {
  const project = d.metadata?.project ?? "";
  const owner = rootDiskOwner(d);
  return (
    <Card>
      <CardHeader title="Summary" />
      <dl className="divide-y px-4">
        <Field label="Source">
          <DiskSource d={d} project={project} />
        </Field>
        <Field label="Size">
          <DiskSize d={d} />
        </Field>
        <Field label="Used as">
          <UsedAs d={d} project={project} />
        </Field>
        <Field label="Made as">
          <MadeAs d={d} />
        </Field>
        {owner && (
          <Field label="Made for">
            <span>
              <Link to="/projects/$project/vms/$name" params={{ project, name: owner }} className="font-mono text-[12.5px] text-accent hover:underline">
                {owner}
              </Link>
              <span className="text-muted">, root disk; deleted with it</span>
            </span>
          </Field>
        )}
        <Field label="Node" mono>
          {d.status?.node || <span className="font-sans text-subtle">Not placed yet</span>}
        </Field>
        <LabelsField labels={d.metadata?.labels ?? {}} onEdit={set.editLabels?.onSelect} />
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
  // Asked with the list, so the page's controls come with it, and once.
  const { can } = useDiskCan(project);
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
  return <Loaded d={d} crumbs={crumbs} can={can} />;
}

/** The page of a disk that has loaded, its actions made once for its head and its labels. */
function Loaded({ d, crumbs, can }: { d: Disk; crumbs: string[]; can: Can }) {
  const navigate = useNavigate();
  const name = d.metadata?.name ?? "";
  const project = d.metadata?.project ?? "";
  const set = useDiskActions(d, can, () => void navigate({ to: "/projects/$project/disks", params: { project } }));
  const fault = diskFault(d);
  const blocked = growthBlocked(d);
  const cli = [`hangar get disk ${name} -p ${project}`, `hangar apply -f disk.yaml -p ${project}`, `hangar delete disk ${name} -p ${project}`];
  return (
    <>
      <Topbar crumbs={crumbs} />
      {d.metadata?.deleteTime && (
        <PageBanner tone="warning" title={`${name} is being deleted`} />
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
      <Header d={d} set={set} />
      <div className="grid grid-cols-1 gap-5 px-4 py-6 md:px-8 xl:grid-cols-[minmax(0,1fr)_380px]">
        <div className="min-w-0 space-y-5">
          <Summary d={d} set={set} />
          <ConditionsCard conditions={d.status?.conditions ?? []} />
        </div>
        <div className="space-y-5">
          <EquivalentCard cli={cli} yaml={diskYaml(d)} />
        </div>
      </div>
    </>
  );
}
