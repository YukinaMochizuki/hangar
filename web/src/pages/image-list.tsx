// The platform's catalogue of disk images: data alone. Images are global and
// the platform's; every member reads them, and disks are made from them.

import { Link } from "@tanstack/react-router";
import { Search } from "lucide-react";
import { useState } from "react";
import { Topbar, useProjectName } from "@/components/shell";
import { EmptyState, ErrorNotice } from "@/components/states";
import { ImageStateCell } from "@/components/storage";
import { Card } from "@/components/ui";
import { gib, imageFloorGiB, imageState, useImages } from "@/lib/storage";
import { LoadingTable } from "@/pages/disk-list";

/** osLabel names an image's OS and version for people: "Debian 13". */
export function osLabel(os: string, version: string): string {
  const name = os ? os[0].toUpperCase() + os.slice(1) : "";
  return [name, version].filter(Boolean).join(" ") || "–";
}

/** ImageList is the platform's catalogue of images, read-only. */
export function ImageList() {
  const project = useProjectName();
  const { items, loading, error, refetch } = useImages();
  const [query, setQuery] = useState("");
  const rows = items.filter((i) => !query || i.metadata?.name.includes(query) || i.spec?.os.includes(query));

  return (
    <>
      <Topbar crumbs={["Images"]} />
      <div className="px-4 pb-4 pt-6 md:px-8">
        <h1 className="text-[22px] font-semibold tracking-tight">Images</h1>
        <p className="mt-1 text-[13px] text-muted">The platform's disk images, which disks and virtual machines are made from.</p>
      </div>

      <div className="px-4 pb-10 md:px-8">
        {loading ? (
          <LoadingTable />
        ) : error ? (
          <Card>
            <ErrorNotice error={error} title="The images did not load" onRetry={refetch} />
          </Card>
        ) : items.length === 0 ? (
          <Card>
            <EmptyState title="No images">The platform's catalogue holds no image.</EmptyState>
          </Card>
        ) : (
          <>
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
                    </li>
                  );
                })}
                {rows.length === 0 && <li className="px-4 py-8 text-center text-[13px] text-muted">No image matches.</li>}
              </ul>
            </Card>
            <Card className="hidden overflow-x-auto md:block">
              <table className="w-full min-w-[760px] text-[13px]">
                <thead>
                  <tr className="border-b bg-surface-2/60 text-left text-[11.5px] text-subtle">
                    <th className="w-[24%] px-4 py-2 font-medium">Name</th>
                    <th className="px-4 py-2 font-medium">OS</th>
                    <th className="px-4 py-2 font-medium">State</th>
                    <th className="px-4 py-2 font-medium">Smallest disk</th>
                    <th className="px-4 py-2 font-medium">Default user</th>
                    <th className="px-4 py-2 font-medium">Arch</th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((i) => {
                    const name = i.metadata?.name ?? "";
                    const own = i.status?.diskSize;
                    return (
                      <tr key={name} className="group border-b last:border-0 hover:bg-surface-2/60">
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
                      </tr>
                    );
                  })}
                  {rows.length === 0 && (
                    <tr>
                      <td colSpan={6} className="px-4 py-10 text-center text-muted">
                        No image matches.
                      </td>
                    </tr>
                  )}
                </tbody>
              </table>
            </Card>
            <p className="mt-3 text-[12px] text-subtle">
              {rows.length} of {items.length}
            </p>
          </>
        )}
      </div>
    </>
  );
}
