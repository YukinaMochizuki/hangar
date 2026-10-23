// One of the project's custom images: what it was captured or uploaded from,
// how far it is on its way (a capture; an upload, then its conversion), the
// space it takes, and the project's disks made from it.

import { Link, useNavigate } from "@tanstack/react-router";
import type { CustomImage } from "@hangar/api/hangar/storage/v1alpha1/custom_image_pb";
import { ConditionStatus } from "@hangar/api/hangar/meta/v1/meta_pb";
import { useCustomImageActions, useCustomImageCan } from "@/components/custom-image-actions";
import { Bar, bytes, UploadCard } from "@/components/image-upload";
import { PageBanner } from "@/components/notice";
import { ConditionsCard, EquivalentCard, LabelsField } from "@/components/object";
import { HeadActions, type Can } from "@/components/object-actions";
import { DetailSkeleton } from "@/components/page";
import { Topbar, useProjectName } from "@/components/shell";
import { EmptyState, ErrorNotice, SkeletonRows } from "@/components/states";
import { DiskState, ImageStateCell } from "@/components/storage";
import { Taxiway, type Stop } from "@/components/taxiway";
import { Badge, Card, CardHeader, CopyButton, Field } from "@/components/ui";
import { customImageFloorGiB, customImageState, sourceDisk, uploadedFraction, uploadSource, useCustomImages, type CustomImageStep } from "@/lib/custom-images";
import { date } from "@/lib/machines";
import { gib, useDisks } from "@/lib/storage";
import { osLabel } from "@/pages/image-list";

/**
 * uploadStops is an uploaded image's way to Ready, as the taxi line a
 * machine's page draws (components/taxiway.tsx): Upload, with how much the
 * node holds; Convert; Ready. A failure stops the line at the step it
 * happened in: the upload's own (expired, a file that does not match its
 * sha256) or the conversion's.
 */
export function uploadStops(ci: CustomImage, step: CustomImageStep, reason?: string): Stop[] {
  const pct = `${Math.floor(uploadedFraction(ci) * 100)}%`;
  const uploaded = ci.status?.conditions.find((c) => c.type === "Uploaded")?.status === ConditionStatus.TRUE;
  const size = bytes(uploadSource(ci)?.size ?? 0);
  switch (step) {
    case "awaiting-upload":
      return [
        { label: "Upload", state: "current", note: `${pct} of ${size}` },
        { label: "Convert", state: "todo" },
        { label: "Ready", state: "todo" },
      ];
    case "converting":
      return [
        { label: "Upload", state: "done", note: size },
        { label: "Convert", state: "current", note: "qcow2 or raw to disk" },
        { label: "Ready", state: "todo" },
      ];
    case "failed":
      return uploaded
        ? [
            { label: "Upload", state: "done", note: size },
            { label: "Convert", state: "failed", note: reason },
            { label: "Ready", state: "todo" },
          ]
        : [
            { label: "Upload", state: "failed", note: reason },
            { label: "Convert", state: "todo" },
            { label: "Ready", state: "todo" },
          ];
    default:
      return [
        { label: "Upload", state: "done", note: size },
        { label: "Convert", state: "done" },
        { label: "Ready", state: "done" },
      ];
  }
}

function Summary({ ci, project, onEditLabels }: { ci: CustomImage; project: string; onEditLabels?: () => void }) {
  const s = ci.spec;
  const disk = sourceDisk(ci);
  const upload = uploadSource(ci);
  const disks = useDisks(project);
  const sourceThere = disks.items.some((d) => d.metadata?.name === disk);
  return (
    <Card>
      <CardHeader title="Summary" />
      <dl className="divide-y px-4">
        <Field label="OS">{osLabel(s?.os ?? "", s?.version ?? "")}</Field>
        {upload ? (
          <>
            <Field label="Uploaded file">{bytes(upload.size)}</Field>
            <Field label="Disk it holds">
              {ci.status?.virtualSize ? (
                <span>
                  {bytes(Number(ci.status.virtualSize))} <span className="text-subtle">· counts toward the project's disk quota until it is ready</span>
                </span>
              ) : (
                <span className="text-subtle">Known once the file is read</span>
              )}
            </Field>
            <Field label="SHA-256" mono>
              {ci.status?.sha256 ? (
                <span className="flex items-start gap-1">
                  <span className="min-w-0 break-all text-muted">{ci.status.sha256}</span>
                  <CopyButton text={ci.status.sha256} className="-mt-0.5 shrink-0" />
                </span>
              ) : (
                <span className="font-sans text-subtle">{upload.sha256 ? `Declared ${upload.sha256.slice(0, 12)}…; checked once the file is whole` : "Known once the file is whole"}</span>
              )}
            </Field>
          </>
        ) : (
        <Field label="Captured from">
          {disk && sourceThere ? (
            <Link to="/projects/$project/disks/$name" params={{ project, name: disk }} className="font-mono text-[12.5px] text-accent hover:underline">
              {disk}
            </Link>
          ) : (
            <span>
              <span className="font-mono text-[12.5px]">{disk}</span>
              {!disks.loading && <span className="text-subtle"> · deleted since; the image does not need it</span>}
            </span>
          )}
        </Field>
        )}
        <Field label="Smallest disk">
          {ci.status?.diskSize ? `${customImageFloorGiB(ci)} GiB` : <span className="text-subtle">Not known yet</span>}
        </Field>
        <Field label="Space used">
          {ci.status?.usedSize ? (
            <span>
              {gib(ci.status.usedSize)} <span className="text-subtle">· counts toward the project's disk quota</span>
            </span>
          ) : (
            <span className="text-subtle">Known once {upload ? "converted" : "captured"}</span>
          )}
        </Field>
        <Field label="Default user" mono>
          {s?.defaultUser || <span className="font-sans text-subtle">None</span>}
        </Field>
        <Field label="Node" mono>
          {ci.status?.node || <span className="font-sans text-subtle">Not known yet</span>}
        </Field>
        <LabelsField labels={ci.metadata?.labels ?? {}} onEdit={onEditLabels} />
        <Field label="Created">{date(ci.metadata?.createTime)?.toISOString().replace("T", " ").slice(0, 16)} UTC</Field>
      </dl>
    </Card>
  );
}

function ProjectDisks({ ci, project }: { ci: CustomImage; project: string }) {
  const { items, loading } = useDisks(project);
  const name = ci.metadata?.name;
  const mine = items.filter((d) => {
    const src = d.spec?.source?.source;
    return src?.case === "image" && src.value.name === name && src.value.project === project;
  });
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
            const dn = d.metadata?.name ?? "";
            return (
              <li key={dn} className="flex items-center gap-3 px-4 py-2.5 text-[13px]">
                <Link to="/projects/$project/disks/$name" params={{ project, name: dn }} className="min-w-0 flex-1 truncate font-medium hover:text-accent">
                  {dn}
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

/** CustomImageDetail is one custom image's page. */
export function CustomImageDetail({ name }: { name: string }) {
  const project = useProjectName();
  const { items, loading, error, refetch } = useCustomImages(project);
  const perm = useCustomImageCan(project);
  const ci = items.find((x) => x.metadata?.name === name);
  const crumbs = [
    <Link key="list" to="/projects/$project/images" params={{ project }} className="hover:text-foreground">
      Images
    </Link>,
    name,
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
            <ErrorNotice error={error} title="The image did not load" onRetry={refetch} />
          </Card>
        </div>
      </>
    );
  }
  if (!ci) {
    return (
      <>
        <Topbar crumbs={crumbs} />
        <EmptyState title="No such image">
          This project has no custom image named <span className="font-mono">{name}</span>.
        </EmptyState>
      </>
    );
  }
  return <Loaded ci={ci} project={project} crumbs={crumbs} can={perm.can} />;
}

function Loaded({ ci, project, crumbs, can }: { ci: CustomImage; project: string; crumbs: React.ReactNode[]; can: Can }) {
  const name = ci.metadata?.name ?? "";
  const navigate = useNavigate();
  const set = useCustomImageActions(ci, project, can, () => void navigate({ to: "/projects/$project/images", params: { project } }));
  const s = customImageState(ci);
  const upload = uploadSource(ci);
  const floor = customImageFloorGiB(ci);
  const cli = [`hangar get customimage ${name} -p ${project}`, `hangar get customimage ${name} -p ${project} -o yaml`];
  const diskYaml = [
    "apiVersion: storage.hangarlab.cloud/v1alpha1",
    "kind: Disk",
    "metadata:",
    "  name: NAME",
    "spec:",
    `  size: ${floor}Gi`,
    "  source:",
    "    image:",
    `      name: ${name}`,
    `      project: ${project}`,
  ].join("\n");
  return (
    <>
      <Topbar crumbs={crumbs} />
      {s.tone === "fail" && (
        <PageBanner tone="error" title={`${name}: ${s.reason}`}>
          {s.message && (/[.!?]$/.test(s.message) ? `${s.message} ` : `${s.message}. `)}No disk can be made from it; delete it and {upload ? "upload the file again" : "capture again"}.
        </PageBanner>
      )}
      {s.capturing && (
        <PageBanner tone="info" title={`Capturing ${name}`}>
          Its node is copying {sourceDisk(ci)}. Disks can be made from it once it is Ready.
        </PageBanner>
      )}
      <div className="border-b px-4 pb-5 pt-6 md:px-8">
        <div className="flex flex-col gap-4 md:flex-row md:items-start md:justify-between md:gap-6">
          <div className="min-w-0">
            <div className="flex items-center gap-2.5">
              <h1 className="truncate font-mono text-[22px] font-semibold tracking-tight">{name}</h1>
              <Badge>Custom image</Badge>
            </div>
            <div className="mt-2 flex flex-wrap items-center gap-x-5 gap-y-1 text-[13px] text-muted">
              <span className="inline-flex items-center gap-2">
                <ImageStateCell s={{ ...s, reason: undefined }} />
              </span>
              <span>{osLabel(ci.spec?.os ?? "", ci.spec?.version ?? "")}</span>
              {ci.status?.usedSize && <span className="tabular">{gib(ci.status.usedSize)} used</span>}
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
          {upload && (s.step === "awaiting-upload" || s.step === "converting" || s.step === "failed") && (
            <Card>
              <div className="px-5 pb-4 pt-5">
                <Taxiway stops={uploadStops(ci, s.step, s.reason)} />
              </div>
            </Card>
          )}
          <UploadCard ci={ci} project={project} />
          {s.step === "converting" && (
            <Card>
              <CardHeader title="Conversion" />
              <div className="space-y-2.5 px-4 py-4">
                <Bar indeterminate />
                <p className="text-[12.5px] text-muted">
                  The file is being turned into a disk image. This takes a few minutes for most images; disks can be made from it once it is Ready.
                </p>
              </div>
            </Card>
          )}
          <Summary ci={ci} project={project} onEditLabels={set.editLabels?.onSelect} />
          <ProjectDisks ci={ci} project={project} />
          <ConditionsCard conditions={ci.status?.conditions ?? []} />
        </div>
        <div className="space-y-5">
          <EquivalentCard cli={cli} yaml={diskYaml} />
        </div>
      </div>
      {set.dialogs}
    </>
  );
}
