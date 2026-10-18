// One image of the platform's catalogue: where its file comes from, what
// nodes found of it, and this project's disks made from it.

import { Link } from "@tanstack/react-router";
import type { Image } from "@hangar/api/hangar/storage/v1alpha1/image_pb";
import { Plus } from "lucide-react";
import { useState } from "react";
import { PageBanner } from "@/components/notice";
import { Topbar, useProjectName } from "@/components/shell";
import { EmptyState, ErrorNotice, Skeleton, SkeletonRows } from "@/components/states";
import { ConditionsCard, DiskState, EquivalentCard, ImageStateCell, NewDiskDialog } from "@/components/storage";
import { Badge, Button, Card, CardHeader, CopyButton, Field } from "@/components/ui";
import { date } from "@/lib/machines";
import { useCan } from "@/lib/permissions";
import { gib, diskImage, imageFloorGiB, imageState, useDisks, useImages } from "@/lib/storage";
import { osLabel } from "@/pages/image-list";

function Summary({ img }: { img: Image }) {
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
          {img.status?.diskSize ? gib(img.status.diskSize) : <span className="text-subtle">Not known until a node has made a disk of it</span>}
        </Field>
        <Field label="Smallest disk">
          {s?.minDiskSize || img.status?.diskSize ? `${imageFloorGiB(img)} GiB` : <span className="text-subtle">Its own size, not known yet</span>}
        </Field>
        <Field label="Default user" mono>
          {s?.defaultUser || <span className="font-sans text-subtle">None; hangar ssh logs in only as a user it is given</span>}
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
      <CardHeader title="Disks of this project made from it" />
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

function DetailSkeleton() {
  return (
    <div aria-busy="true" aria-label="Loading">
      <div className="space-y-3 border-b px-4 pb-5 pt-6 md:px-8">
        <Skeleton className="h-7 w-48" />
        <Skeleton className="h-3 w-64" />
      </div>
      <div className="grid grid-cols-1 gap-5 px-4 py-6 md:px-8 xl:grid-cols-[minmax(0,1fr)_380px]">
        <Card>
          <SkeletonRows rows={6} />
        </Card>
        <Card>
          <SkeletonRows rows={2} />
        </Card>
      </div>
    </div>
  );
}

/** ImageDetail is one image's page, with New disk from it. */
export function ImageDetail({ name }: { name: string }) {
  const project = useProjectName();
  const { items, loading, error, refetch } = useImages();
  const disks = useDisks(project);
  const [creating, setCreating] = useState(false);
  // New disk from it shows only once the caller is known to hold storage.disks.apply.
  const canCreate = useCan({ level: "project", name: project }, ["storage.disks.apply"]).can("storage.disks.apply");
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
          {s.message} No disk is made from it until its spec changes.
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
          {canCreate && (
            <Button variant="primary" disabled={s.tone === "fail"} onClick={() => setCreating(true)}>
              <Plus /> New disk from it
            </Button>
          )}
        </div>
      </div>
      <div className="grid grid-cols-1 gap-5 px-4 py-6 md:px-8 xl:grid-cols-[minmax(0,1fr)_380px]">
        <div className="min-w-0 space-y-5">
          <Summary img={img} />
          <ProjectDisks img={img} />
          <ConditionsCard conditions={img.status?.conditions ?? []} />
        </div>
        <div className="space-y-5">
          <EquivalentCard cli={cli} yaml={diskYaml} />
        </div>
      </div>
      <NewDiskDialog
        key={String(creating)}
        open={creating}
        onClose={() => setCreating(false)}
        project={project}
        images={items}
        disks={disks}
        initialImage={name}
      />
    </>
  );
}
