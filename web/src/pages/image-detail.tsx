// One image of the platform's catalogue: where its file comes from, what
// nodes found of it, and this project's disks made from it.

import { Link } from "@tanstack/react-router";
import type { Image } from "@hangar/api/hangar/storage/v1alpha1/image_pb";
import { PageBanner } from "@/components/notice";
import { DetailSkeleton } from "@/components/page";
import { Topbar, useProjectName } from "@/components/shell";
import { EmptyState, ErrorNotice, SkeletonRows } from "@/components/states";
import { DiskState, ImageStateCell } from "@/components/storage";
import { useImageActions, useImageCan } from "@/components/image-actions";
import type { Can } from "@/components/object-actions";
import { ConditionsCard, EquivalentCard, LabelsField } from "@/components/object";
import { HeadActions } from "@/components/object-actions";
import { Badge, Card, CardHeader, CopyButton, Field } from "@/components/ui";
import { date } from "@/lib/machines";
import { gib, diskImage, imageFloorGiB, imageState, useDisks, useImages } from "@/lib/storage";
import { osLabel } from "@/pages/image-list";

function Summary({ img, onEditLabels }: { img: Image; onEditLabels?: () => void }) {
  const s = img.spec;
  return (
    <Card>
      <CardHeader title="Summary" />
      <dl className="divide-y px-4">
        <Field label="OS">{osLabel(s?.os ?? "", s?.version ?? "")}</Field>
        <Field label="Arch" mono>
          {s?.arch || "amd64"}
        </Field>
        <Field label="Own size">
          {img.status?.diskSize ? gib(img.status.diskSize) : <span className="text-subtle">Not known yet</span>}
        </Field>
        <Field label="Smallest disk">
          {s?.minDiskSize || img.status?.diskSize ? `${imageFloorGiB(img)} GiB` : <span className="text-subtle">Not known yet</span>}
        </Field>
        <Field label="Default user" mono>
          {s?.defaultUser || <span className="font-sans text-subtle">None</span>}
        </Field>
        <Field label="URL" mono>
          <span className="flex items-start gap-1">
            <span className="min-w-0 break-all">{s?.url}</span>
            <CopyButton text={s?.url ?? ""} className="-mt-0.5 shrink-0" />
          </span>
        </Field>
        <Field label="SHA-256" mono>
          <span className="flex items-start gap-1">
            <span className="min-w-0 break-all text-muted">{s?.sha256}</span>
            <CopyButton text={s?.sha256 ?? ""} className="-mt-0.5 shrink-0" />
          </span>
        </Field>
        <LabelsField labels={img.metadata?.labels ?? {}} onEdit={onEditLabels} />
        <Field label="Created">{date(img.metadata?.createTime)?.toISOString().replace("T", " ").slice(0, 16)} UTC</Field>
      </dl>
    </Card>
  );
}

function ProjectDisks({ img }: { img: Image }) {
  const project = useProjectName();
  const { items, loading } = useDisks(project);
  const mine = items.filter((d) => diskImage(d) === img.metadata?.name);
  return (
    <Card>
      <CardHeader title="Disks from this image" />
      {loading ? (
        <SkeletonRows rows={2} />
      ) : mine.length === 0 ? (
        <div className="px-4 py-3 text-[13px] text-muted">None.</div>
      ) : (
        <ul className="divide-y">
          {mine.map((d) => {
            const name = d.metadata?.name ?? "";
            return (
              <li key={name} className="flex items-center gap-3 px-4 py-2.5 text-[13px]">
                <Link to="/projects/$project/disks/$name" params={{ project, name }} className="min-w-0 flex-1 truncate font-medium hover:text-accent">
                  {name}
                </Link>
                <span className="tabular text-muted">{gib(d.status?.size || d.spec?.size)}</span>
                <span className="w-24 shrink-0">
                  <DiskState d={d} />
                </span>
              </li>
            );
          })}
        </ul>
      )}
    </Card>
  );
}

/** ImageDetail is one image's page, with New disk from it. */
export function ImageDetail({ name }: { name: string }) {
  const project = useProjectName();
  const { items, loading, error, refetch } = useImages();
  // Asked with the list, so the page's controls come with it, and once.
  const can = useImageCan(project);
  const img = items.find((x) => x.metadata?.name === name);
  const crumbs = ["Images", name];
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
            <ErrorNotice error={error} title="The image did not load" onRetry={refetch} />
          </Card>
        </div>
      </>
    );
  }
  if (!img) {
    return (
      <>
        <Topbar crumbs={crumbs} />
        <EmptyState title="No such image">
          The platform has no image named <span className="font-mono">{name}</span>.
        </EmptyState>
      </>
    );
  }
  return <Loaded img={img} project={project} crumbs={crumbs} can={can} />;
}

/** The page of an image that has loaded, its actions made once for its head and its labels. */
function Loaded({ img, project, crumbs, can }: { img: Image; project: string; crumbs: string[]; can: Can }) {
  const name = img.metadata?.name ?? "";
  const set = useImageActions(img, project, can);
  const s = imageState(img);
  const floor = imageFloorGiB(img);
  const cli = [`hangar get image ${name}`, `hangar get image ${name} -o yaml`];
  const diskYaml = [
    "apiVersion: storage.hangarlab.cloud/v1alpha1",
    "kind: Disk",
    "metadata:",
    "  name: NAME",
    "spec:",
    `  size: ${Math.max(floor, 10)}Gi`,
    "  source:",
    "    image:",
    `      name: ${name}`,
  ].join("\n");
  return (
    <>
      <Topbar crumbs={crumbs} />
      {s.tone === "fail" && (
        <PageBanner tone="error" title={`${name} is at fault: ${s.reason}`}>
          {s.message} No disk can be made from it.
        </PageBanner>
      )}
      <div className="border-b px-4 pb-5 pt-6 md:px-8">
        <div className="flex flex-col gap-4 md:flex-row md:items-start md:justify-between md:gap-6">
          <div className="min-w-0">
            <div className="flex items-center gap-2.5">
              <h1 className="truncate font-mono text-[22px] font-semibold tracking-tight">{name}</h1>
              <Badge>Image</Badge>
            </div>
            <div className="mt-2 flex flex-wrap items-center gap-x-5 gap-y-1 text-[13px] text-muted">
              <span className="inline-flex items-center gap-2">
                <ImageStateCell s={{ ...s, reason: undefined }} />
              </span>
              <span>{osLabel(img.spec?.os ?? "", img.spec?.version ?? "")}</span>
              {img.status?.diskSize && <span className="tabular">{gib(img.status.diskSize)}</span>}
            </div>
          </div>
          {set.actions.length > 0 && (
            <div className="flex flex-wrap items-center gap-2">
              <HeadActions set={set} />
            </div>
          )}
        </div>
      </div>
      <div className="grid grid-cols-1 gap-5 px-4 py-6 md:px-8 xl:grid-cols-[minmax(0,1fr)_380px]">
        <div className="min-w-0 space-y-5">
          <Summary img={img} onEditLabels={set.editLabels?.onSelect} />
          <ProjectDisks img={img} />
          <ConditionsCard conditions={img.status?.conditions ?? []} />
        </div>
        <div className="space-y-5">
          <EquivalentCard cli={cli} yaml={diskYaml} />
        </div>
      </div>
      {set.dialogs}
    </>
  );
}
