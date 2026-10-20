// The project's file shares: each with its state, the networks it is on and
// its address on each, its size and the machines that use it.

import { Link } from "@tanstack/react-router";
import { Plus } from "lucide-react";
import { useState } from "react";
import { NetworkChips, ShareState, useDeclarations, useSharePerms } from "@/components/file-shares";
import { CappedNames } from "@/components/machine-names";
import { ListCount, ListFrame, NameFilter, PageHead } from "@/components/page";
import { Topbar, useProjectName } from "@/components/shell";
import { NoMatch, tbodyRow, Th, theadRow } from "@/components/table";
import { ErrorNotice, Skeleton } from "@/components/states";
import { buttonVariants, Card } from "@/components/ui";
import { shareGiB, shareName, shareUses, useFileShares } from "@/lib/file-shares";
import { count } from "@/lib/network";
import { formatNumber } from "@/lib/quantity";

const newButton = (project: string) => (
  <Link to="/projects/$project/new/file-share" params={{ project }} className={buttonVariants({ variant: "primary" })}>
    <Plus /> New file share
  </Link>
);

export function FileShareList() {
  const project = useProjectName();
  const shares = useFileShares(project);
  const decls = useDeclarations(project);
  const { canApply } = useSharePerms(project);
  const [query, setQuery] = useState("");
  const rows = shares.items.filter((s) => !query || shareName(s).includes(query));
  return (
    <>
      <Topbar crumbs={["File shares"]} />
      <PageHead
        title="File shares"
        description="Shared file storage, mounted by many machines at once over NFS. A share is reachable from the networks it is on, at an address on each."
        action={canApply ? newButton(project) : undefined}
      />
      <div className="px-4 pb-10 md:px-8">
        <ListFrame
          state={{ ...shares, count: shares.items.length }}
          noun="file shares"
          empty={{
            title: "No file shares",
            text: "A file share holds files that several machines read and write at once, from the networks it is on.",
            action: canApply ? newButton(project) : undefined,
          }}
        >
          <NameFilter value={query} onChange={setQuery} />
          {decls.error != null && (
            <Card className="mb-3">
              <ErrorNotice error={decls.error} title="The project's machines did not load, so which use each share is not known" onRetry={decls.refetch} />
            </Card>
          )}
          <Card className="overflow-x-auto">
            <table className="w-full min-w-[820px] text-[13px]">
              <thead>
                <tr className={theadRow}>
                  <Th className="w-[18%]">Name</Th>
                  <Th>State</Th>
                  <Th>Networks and addresses</Th>
                  <Th>Size</Th>
                  <Th>Used by</Th>
                </tr>
              </thead>
              <tbody>
                {rows.map((s) => {
                  const name = shareName(s);
                  const uses = shareUses(s, decls.items);
                  return (
                    <tr key={name} className={tbodyRow}>
                      <td className="max-w-0 px-4 py-2.5">
                        <Link to="/projects/$project/file-shares/$name" params={{ project, name }} className="block truncate font-medium group-hover:text-accent">
                          {name}
                        </Link>
                      </td>
                      <td className="whitespace-nowrap px-4 py-2.5">
                        <ShareState s={s} />
                      </td>
                      <td className="px-4 py-2">
                        <NetworkChips s={s} />
                      </td>
                      <td className="whitespace-nowrap px-4 py-2.5 font-mono text-[12px] tabular">{formatNumber(shareGiB(s))} GiB</td>
                      <td className="whitespace-nowrap px-4 py-2.5">
                        {decls.loading ? (
                          <Skeleton className="h-3 w-24" />
                        ) : decls.error != null ? (
                          <span className="text-fail">Not loaded</span>
                        ) : uses.length === 0 ? (
                          <span className="text-subtle">None</span>
                        ) : (
                          <span className="tabular">
                            {count(uses.length, "machine")}
                            <span className="ml-2 text-[11.5px] text-subtle">
                              <CappedNames machines={uses} title={<>Machines using <span className="font-mono">{name}</span></>} align="right" />
                            </span>
                          </span>
                        )}
                      </td>
                    </tr>
                  );
                })}
                {rows.length === 0 && <NoMatch noun="file share" colSpan={5} />}
              </tbody>
            </table>
          </Card>
          <ListCount shown={rows.length} total={shares.items.length} />
        </ListFrame>
      </div>
    </>
  );
}
