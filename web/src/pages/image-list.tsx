// The platform's catalogue of disk images: data alone. Images are global and
// the platform's; every member reads them, and disks are made from them.

import { Link } from "@tanstack/react-router";
import { Search } from "lucide-react";
import { useState } from "react";
import type { Image } from "@hangar/api/hangar/storage/v1alpha1/image_pb";
import { useImageActions, useImageCan } from "@/components/image-actions";
import { ObjectRowMenu, type Can } from "@/components/object-actions";
import { ListFrame, PageHead } from "@/components/page";
import { Topbar, useProjectName } from "@/components/shell";
import { ImageStateCell } from "@/components/storage";
import { NoMatch, tbodyRow, Th, theadRow } from "@/components/table";
import { Card } from "@/components/ui";
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

/** ImageList is the platform's catalogue of images, read-only. */
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
      <PageHead title="Images" description="The platform's disk images, which disks and virtual machines are made from." />

      <div className="px-4 pb-10 md:px-8">
        <ListFrame
          state={{ loading, error, refetch, count: items.length }}
          noun="images"
          empty={{ title: "No images", text: "The platform's catalogue holds no image." }}
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
