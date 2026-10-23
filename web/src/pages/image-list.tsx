// The images disks are made from: the project's own custom images, captured
// from its stopped disks, then the platform's catalogue, which is global and
// read-only to tenants. Both are data alone, with each row's actions.

import { Link } from "@tanstack/react-router";
import { Plus, Search } from "lucide-react";
import { useState } from "react";
import type { CustomImage } from "@hangar/api/hangar/storage/v1alpha1/custom_image_pb";
import type { Image } from "@hangar/api/hangar/storage/v1alpha1/image_pb";
import { customImagePerms, useCustomImageActions, useCustomImageCan } from "@/components/custom-image-actions";
import { useImageActions, useImageCan } from "@/components/image-actions";
import { ObjectRowMenu, type Can } from "@/components/object-actions";
import { ListFrame, PageHead } from "@/components/page";
import { Topbar, useProjectName } from "@/components/shell";
import { ImageStateCell } from "@/components/storage";
import { NoMatch, tbodyRow, Th, theadRow } from "@/components/table";
import { buttonVariants, Card } from "@/components/ui";
import { customImageFloorGiB, customImageState, sourceDisk, useCustomImages } from "@/lib/custom-images";
import { gib, imageFloorGiB, imageState, useImages } from "@/lib/storage";

/** osLabel names an image's OS and version for people: "Debian 13". */
export function osLabel(os: string, version: string): string {
  const name = os ? os[0].toUpperCase() + os.slice(1) : "";
  return [name, version].filter(Boolean).join(" ") || "–";
}

/** An image's row menu: the actions of its page. */
function ImageRowMenu({ img, project, can }: { img: Image; project: string; can: Can }) {
  return <ObjectRowMenu name={img.metadata?.name ?? ""} set={useImageActions(img, project, can)} />;
}

/** A custom image's row menu: the actions of its page. */
function CustomImageRowMenu({ ci, project, can }: { ci: CustomImage; project: string; can: Can }) {
  return <ObjectRowMenu name={ci.metadata?.name ?? ""} set={useCustomImageActions(ci, project, can)} />;
}

/** SectionHead names one of the page's two lists. */
function SectionHead({ title, text, action }: { title: string; text: string; action?: React.ReactNode }) {
  return (
    <div className="mb-3 flex flex-wrap items-end justify-between gap-3">
      <div>
        <h2 className="text-[15px] font-semibold tracking-tight">{title}</h2>
        <p className="mt-0.5 text-[12.5px] text-muted">{text}</p>
      </div>
      {action}
    </div>
  );
}

/** CustomImages is the project's own images, with New image for whoever may capture one. */
function CustomImages({ project }: { project: string }) {
  const { items, loading, error, refetch } = useCustomImages(project);
  const perm = useCustomImageCan(project);
  const newButton = perm.can(customImagePerms.apply) && (
    <Link to="/projects/$project/new/custom-image" params={{ project }} search={{}} className={buttonVariants({ variant: "primary" })}>
      <Plus /> New image
    </Link>
  );
  return (
    <section className="mb-10">
      <SectionHead title="Custom images" text="This project's own, captured from its stopped disks" action={items.length > 0 && newButton} />
      <ListFrame
        state={{ loading, error, refetch, count: items.length }}
        noun="custom images"
        empty={{
          title: "No custom images",
          text: "Set up a virtual machine, stop it, and capture its boot disk: new machines can then start from that image.",
          action: newButton || undefined,
        }}
      >
        <Card className="md:hidden">
          <ul className="divide-y">
            {items.map((ci) => {
              const name = ci.metadata?.name ?? "";
              return (
                <li key={name} className="flex items-center gap-3 px-4 py-2.5 text-[13px]">
                  <Link to="/projects/$project/custom-images/$name" params={{ project, name }} className="min-w-0 flex-1">
                    <span className="block truncate font-medium">{name}</span>
                    <span className="block truncate text-[11.5px] text-subtle">
                      {osLabel(ci.spec?.os ?? "", ci.spec?.version ?? "")}
                      {ci.status?.usedSize ? ` · ${gib(ci.status.usedSize)} used` : ""}
                    </span>
                  </Link>
                  <span className="shrink-0 text-right">
                    <ImageStateCell s={customImageState(ci)} />
                  </span>
                  <CustomImageRowMenu ci={ci} project={project} can={perm.can} />
                </li>
              );
            })}
          </ul>
        </Card>
        <Card className="hidden overflow-x-auto md:block">
          <table className="w-full min-w-[760px] text-[13px]">
            <thead>
              <tr className={theadRow}>
                <Th className="w-[24%]">Name</Th>
                <Th>OS</Th>
                <Th>State</Th>
                <Th>Captured from</Th>
                <Th>Smallest disk</Th>
                <Th>Space used</Th>
                <Th className="w-10 px-0" />
              </tr>
            </thead>
            <tbody>
              {items.map((ci) => {
                const name = ci.metadata?.name ?? "";
                const disk = sourceDisk(ci);
                return (
                  <tr key={name} className={tbodyRow}>
                    <td className="max-w-0 px-4 py-2.5">
                      <Link to="/projects/$project/custom-images/$name" params={{ project, name }} className="block truncate font-mono text-[12.5px] font-medium group-hover:text-accent">
                        {name}
                      </Link>
                    </td>
                    <td className="whitespace-nowrap px-4 py-2.5">{osLabel(ci.spec?.os ?? "", ci.spec?.version ?? "")}</td>
                    <td className="whitespace-nowrap px-4 py-2.5">
                      <ImageStateCell s={customImageState(ci)} />
                    </td>
                    <td className="whitespace-nowrap px-4 py-2.5 font-mono text-[12px] text-muted">{disk ?? "–"}</td>
                    <td className="whitespace-nowrap px-4 py-2.5 tabular">
                      {ci.status?.diskSize ? `${customImageFloorGiB(ci)} GiB` : <span className="text-subtle">not known yet</span>}
                    </td>
                    <td className="whitespace-nowrap px-4 py-2.5 tabular">
                      {ci.status?.usedSize ? gib(ci.status.usedSize) : <span className="text-subtle">–</span>}
                    </td>
                    <td className="px-2 py-2.5">
                      <CustomImageRowMenu ci={ci} project={project} can={perm.can} />
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </Card>
      </ListFrame>
    </section>
  );
}

/** ImageList is the project's custom images, then the platform's catalogue, read-only. */
export function ImageList() {
  const project = useProjectName();
  const { items, loading, error, refetch } = useImages();
  // One answer for every row's menu.
  const can = useImageCan(project);
  const [query, setQuery] = useState("");
  const rows = items.filter((i) => !query || i.metadata?.name.includes(query) || i.spec?.os.includes(query));

  return (
    <>
      <Topbar crumbs={["Images"]} />
      <PageHead title="Images" description="Operating systems to boot disks from" />

      <div className="px-4 pb-10 md:px-8">
        <CustomImages project={project} />
        <SectionHead title="Platform images" text="Distributions' own cloud images, kept by the platform" />
        <ListFrame
          state={{ loading, error, refetch, count: items.length }}
          noun="images"
          empty={{ title: "No images", text: "The platform's operating system images, from which virtual machines and boot disks are made." }}
        >
          <div className="mb-3">
            <label className="flex h-8 w-full items-center gap-2 rounded-md border bg-surface px-2.5 text-[13px] focus-within:border-border-strong sm:w-64">
              <Search className="size-3.5 text-subtle" />
              <input
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                placeholder="Name or OS"
                aria-label="Filter by name or OS"
                className="min-w-0 flex-1 bg-transparent outline-none placeholder:text-subtle"
              />
            </label>
          </div>

          <Card className="md:hidden">
            <ul className="divide-y">
              {rows.map((i) => {
                const name = i.metadata?.name ?? "";
                return (
                  <li key={name} className="flex items-center gap-3 px-4 py-2.5 text-[13px]">
                    <Link to="/projects/$project/images/$name" params={{ project, name }} className="min-w-0 flex-1">
                      <span className="block truncate font-medium">{name}</span>
                      <span className="block truncate text-[11.5px] text-subtle">
                        {osLabel(i.spec?.os ?? "", i.spec?.version ?? "")}
                        {i.status?.diskSize ? ` · ${gib(i.status.diskSize)}` : ""}
                      </span>
                    </Link>
                    <span className="shrink-0 text-right">
                      <ImageStateCell s={imageState(i)} />
                    </span>
                    <ImageRowMenu img={i} project={project} can={can} />
                  </li>
                );
              })}
              {rows.length === 0 && <NoMatch noun="image" />}
            </ul>
          </Card>
          <Card className="hidden overflow-x-auto md:block">
            <table className="w-full min-w-[760px] text-[13px]">
              <thead>
                <tr className={theadRow}>
                  <Th className="w-[24%]">Name</Th>
                  <Th>OS</Th>
                  <Th>State</Th>
                  <Th>Smallest disk</Th>
                  <Th>Default user</Th>
                  <Th>Arch</Th>
                  <Th className="w-10 px-0" />
                </tr>
              </thead>
              <tbody>
                {rows.map((i) => {
                  const name = i.metadata?.name ?? "";
                  const own = i.status?.diskSize;
                  return (
                    <tr key={name} className={tbodyRow}>
                      <td className="max-w-0 px-4 py-2.5">
                        <Link to="/projects/$project/images/$name" params={{ project, name }} className="block truncate font-mono text-[12.5px] font-medium group-hover:text-accent">
                          {name}
                        </Link>
                      </td>
                      <td className="whitespace-nowrap px-4 py-2.5">{osLabel(i.spec?.os ?? "", i.spec?.version ?? "")}</td>
                      <td className="whitespace-nowrap px-4 py-2.5">
                        <ImageStateCell s={imageState(i)} />
                      </td>
                      <td className="whitespace-nowrap px-4 py-2.5 tabular">
                        {i.spec?.minDiskSize || own ? `${imageFloorGiB(i)} GiB` : <span className="text-subtle">not known yet</span>}
                      </td>
                      <td className="whitespace-nowrap px-4 py-2.5 font-mono text-[12px]">
                        {i.spec?.defaultUser || <span className="font-sans text-subtle">none</span>}
                      </td>
                      <td className="whitespace-nowrap px-4 py-2.5 font-mono text-[12px] text-muted">{i.spec?.arch || "amd64"}</td>
                      <td className="px-2 py-2.5">
                        <ImageRowMenu img={i} project={project} can={can} />
                      </td>
                    </tr>
                  );
                })}
                {rows.length === 0 && <NoMatch noun="image" colSpan={7} />}
              </tbody>
            </table>
          </Card>
          <p className="mt-3 text-[12px] text-subtle">
            {rows.length} of {items.length}
          </p>
        </ListFrame>
      </div>
    </>
  );
}
