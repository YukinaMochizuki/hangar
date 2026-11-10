// A file share's files: one directory at a time, as the share's server
// lists it, read-only. Names, types, sizes and modification times; never
// what a file holds or where a link points (docs/decisions/file-shares.md).

import { Link, useNavigate } from "@tanstack/react-router";
import { ChevronRight, File, FileQuestion, Folder, HardDrive, Link2, RefreshCw } from "lucide-react";
import { Notice } from "@/components/notice";
import { DetailFrame } from "@/components/page";
import { Topbar } from "@/components/shell";
import { ErrorNotice, SkeletonRows } from "@/components/states";
import { tbodyRow, Th, theadRow } from "@/components/table";
import { Badge, Button, Card } from "@/components/ui";
import { cn } from "@/lib/cn";
import { useFileShares } from "@/lib/file-shares";
import { nameOf } from "@/lib/network";
import { FileShareEntryType } from "@hangar/api/hangar/storage/v1alpha1/file_share_files_pb";
import {
  decodePath,
  displayName,
  encodePath,
  filesFailure,
  formatSize,
  typeWord,
  useShareFiles,
  type FileShareEntry,
  type FilesFailure,
  type SharePath,
} from "@/lib/share-files";

/** Where a listing stands. */
export type FilesState =
  | { kind: "loading" }
  | { kind: "failed"; failure: FilesFailure }
  | { kind: "listed"; entries: FileShareEntry[]; more: boolean; loadingMore: boolean; moreError?: unknown };

const icons: Record<FileShareEntryType, typeof File> = {
  [FileShareEntryType.UNSPECIFIED]: FileQuestion,
  [FileShareEntryType.REGULAR]: File,
  [FileShareEntryType.DIRECTORY]: Folder,
  [FileShareEntryType.SYMLINK]: Link2,
  [FileShareEntryType.OTHER]: FileQuestion,
};

/** A modification time as the console writes times: UTC, to the minute. */
function modified(e: FileShareEntry): string {
  if (!e.modifyTime) return "–";
  const d = new Date(Number(e.modifyTime.seconds) * 1000);
  return d.toISOString().replace("T", " ").slice(0, 16);
}

/** The page of a share's directory the URL names: path as encodePath wrote it. */
export function FileShareFiles({ project, name, path: encoded }: { project: string; name: string; path?: string }) {
  const navigate = useNavigate();
  // The share itself, so a share that is gone or going is told apart from
  // a directory of it that is.
  const shares = useFileShares(project);
  const share = shares.items.find((x) => nameOf(x) === name);
  const path = decodePath(encoded);
  const open = (p: SharePath) =>
    void navigate({ to: "/projects/$project/file-shares/$name/files", params: { project, name }, search: p.length > 0 ? { path: encodePath(p) } : {} });
  const none = () => {};
  if (shares.loading || shares.error || !share) {
    const crumbs = [
      <Link key="list" to="/projects/$project/file-shares" params={{ project }} className="hover:text-foreground">
        File shares
      </Link>,
      name,
    ];
    return (
      <DetailFrame crumbs={crumbs} state={shares} found={!!share} noun="file share" name={name}>
        {null}
      </DetailFrame>
    );
  }
  if (share.metadata?.deleteTime) {
    return <FilesView project={project} share={name} path={path ?? []} state={{ kind: "failed", failure: { kind: "deleting" } }} onOpen={open} onLoadMore={none} onRetry={none} />;
  }
  // A path no listing's link writes is one that does not exist.
  if (!path) return <FilesView project={project} share={name} path={[]} state={{ kind: "failed", failure: { kind: "notFound" } }} onOpen={open} onLoadMore={none} onRetry={() => open([])} />;
  return <Listed key={encodePath(path)} project={project} name={name} path={path} onOpen={open} />;
}

function Listed({ project, name, path, onOpen }: { project: string; name: string; path: SharePath; onOpen: (p: SharePath) => void }) {
  const files = useShareFiles(project, name, path);
  const state: FilesState = files.loading
    ? { kind: "loading" }
    : files.error
      ? { kind: "failed", failure: filesFailure(files.error) }
      : { kind: "listed", entries: files.entries, more: files.more, loadingMore: files.loadingMore, moreError: files.moreError };
  return <FilesView project={project} share={name} path={path} state={state} onOpen={onOpen} onLoadMore={files.loadMore} onRetry={files.refetch} />;
}

/** One directory of a share, drawn from what was listed. */
export function FilesView({
  project,
  share,
  path,
  state,
  onOpen,
  onLoadMore,
  onRetry,
}: {
  project: string;
  share: string;
  path: SharePath;
  state: FilesState;
  /** Opens the directory at the path given. */
  onOpen: (path: SharePath) => void;
  onLoadMore: () => void;
  onRetry: () => void;
}) {
  const crumbs = [
    <Link key="list" to="/projects/$project/file-shares" params={{ project }} className="hover:text-foreground">
      File shares
    </Link>,
    <Link key="share" to="/projects/$project/file-shares/$name" params={{ project, name: share }} className="hover:text-foreground">
      {share}
    </Link>,
    "Files",
  ];
  return (
    <>
      <Topbar crumbs={crumbs} />
      <div className="border-b px-4 pb-5 pt-6 md:px-8">
        <div className="flex flex-col gap-3 md:flex-row md:items-start md:justify-between">
          <div className="min-w-0">
            <div className="flex items-center gap-2.5">
              <h1 className="truncate text-[22px] font-semibold tracking-tight">Files</h1>
              <Badge>Read-only</Badge>
            </div>
            <p className="mt-2 text-[13px] text-muted">
              What <span className="font-mono text-foreground">{share}</span> holds, as its server lists it: names, sizes and modification times. What
              files hold is not shown, and links are not followed.
            </p>
          </div>
          <Button onClick={onRetry} aria-label="Refresh">
            <RefreshCw /> Refresh
          </Button>
        </div>
      </div>
      <div className="px-4 py-6 md:px-8">
        <Card className="overflow-hidden">
          <PathBar share={share} path={path} onOpen={onOpen} />
          <Listing path={path} state={state} onOpen={onOpen} onLoadMore={onLoadMore} onRetry={onRetry} />
        </Card>
      </div>
    </>
  );
}

/** The directory's path from the share's root, each step a link to it. */
function PathBar({ share, path, onOpen }: { share: string; path: SharePath; onOpen: (path: SharePath) => void }) {
  return (
    <nav aria-label="Path" className="flex min-h-11 flex-wrap items-center gap-x-1 gap-y-1 border-b px-4 py-2 font-mono text-[12.5px]">
      <button type="button" onClick={() => onOpen([])} className={cn("inline-flex items-center gap-1.5 rounded px-1 hover:bg-surface-2", path.length === 0 && "text-foreground")}>
        <HardDrive className="size-3.5 text-muted" strokeWidth={1.8} />
        {share}
      </button>
      {path.map((name, i) => {
        const last = i === path.length - 1;
        return (
          <span key={i} className="inline-flex items-center gap-1">
            <ChevronRight className="size-3.5 text-subtle" />
            {last ? (
              <span className="px-1 text-foreground">{displayName(name).text}</span>
            ) : (
              <button type="button" onClick={() => onOpen(path.slice(0, i + 1))} className="rounded px-1 text-muted hover:bg-surface-2 hover:text-foreground">
                {displayName(name).text}
              </button>
            )}
          </span>
        );
      })}
    </nav>
  );
}

function Listing({
  path,
  state,
  onOpen,
  onLoadMore,
  onRetry,
}: {
  path: SharePath;
  state: FilesState;
  onOpen: (path: SharePath) => void;
  onLoadMore: () => void;
  onRetry: () => void;
}) {
  if (state.kind === "loading") return <SkeletonRows rows={6} className="p-4" />;
  if (state.kind === "failed") return <Failure failure={state.failure} path={path} onOpen={onOpen} onRetry={onRetry} />;
  if (state.entries.length === 0) {
    return <div className="px-4 py-10 text-center text-[13px] text-muted">{path.length === 0 ? "The share is empty." : "This directory is empty."}</div>;
  }
  return (
    <>
      <div className="overflow-x-auto">
        <table className="w-full text-[13px]">
          <thead>
            <tr className={theadRow}>
              <Th className="pl-4">Name</Th>
              <Th className="hidden md:table-cell">Type</Th>
              <Th className="pr-4 text-right md:pr-3">Size</Th>
              <Th className="hidden pr-4 md:table-cell">Modified (UTC)</Th>
            </tr>
          </thead>
          <tbody>
            {state.entries.map((e, i) => (
              <Row key={i} e={e} onOpen={() => onOpen([...path, e.name])} />
            ))}
          </tbody>
        </table>
      </div>
      <div className="flex h-11 items-center justify-between border-t px-4 text-[12px] text-muted">
        <span>
          <span className="font-mono tabular">{state.entries.length.toLocaleString("en-US")}</span> shown{state.more ? ", more after them" : ""} · sorted by name
        </span>
        {state.more && (
          <Button size="sm" onClick={onLoadMore} disabled={state.loadingMore}>
            {state.loadingMore ? "Loading…" : "Load more"}
          </Button>
        )}
      </div>
      {state.moreError !== undefined && <ErrorNotice error={state.moreError} title="The next entries did not load" onRetry={onLoadMore} className="border-t" />}
    </>
  );
}

function Row({ e, onOpen }: { e: FileShareEntry; onOpen: () => void }) {
  const Icon = icons[e.type];
  const { text, utf8 } = displayName(e.name);
  const dir = e.type === FileShareEntryType.DIRECTORY;
  const name = (
    <span className="inline-flex min-w-0 items-center gap-2">
      <Icon className={cn("size-4 shrink-0", dir ? "text-accent" : "text-muted")} strokeWidth={1.7} />
      <span className="truncate font-mono text-[12.5px]">{text}</span>
      {!utf8 && (
        <Badge className="shrink-0" title="The name is not valid UTF-8: bytes that are not are shown as \xNN.">
          not UTF-8
        </Badge>
      )}
    </span>
  );
  return (
    <tr className={tbodyRow}>
      <td className="max-w-0 py-2 pl-4 pr-3 md:w-[55%]">
        {dir ? (
          <button type="button" onClick={onOpen} className="flex max-w-full items-center text-left hover:underline">
            {name}
          </button>
        ) : (
          name
        )}
        {/* The columns a phone has no room for, under the name. */}
        <span className="mt-0.5 block pl-6 text-[11.5px] text-subtle md:hidden">
          {typeWord(e.type)} · <span className="whitespace-nowrap font-mono">{modified(e)}</span>
        </span>
      </td>
      <td className="hidden whitespace-nowrap px-3 py-2 text-muted md:table-cell">
        {typeWord(e.type)}
        {e.type === FileShareEntryType.SYMLINK && <span className="ml-1.5 text-[11.5px] text-subtle">not followed</span>}
      </td>
      <td className="whitespace-nowrap py-2 pl-3 pr-4 text-right font-mono text-[12.5px] tabular md:pr-3" title={e.type === FileShareEntryType.REGULAR ? `${e.sizeBytes} bytes` : undefined}>
        {e.type === FileShareEntryType.REGULAR ? formatSize(e.sizeBytes) : <span className="text-subtle">–</span>}
      </td>
      <td className="hidden whitespace-nowrap py-2 pl-3 pr-4 font-mono text-[12.5px] text-muted tabular md:table-cell">{modified(e)}</td>
    </tr>
  );
}

/** Why the directory is not listed, in the card where its entries would be. */
function Failure({ failure, path, onOpen, onRetry }: { failure: FilesFailure; path: SharePath; onOpen: (path: SharePath) => void; onRetry: () => void }) {
  const toRoot = path.length > 0 && (
    <Button size="sm" className="mt-3" onClick={() => onOpen([])}>
      Back to the share's root
    </Button>
  );
  const retry = (
    <Button size="sm" className="mt-3" onClick={onRetry}>
      <RefreshCw /> Try again
    </Button>
  );
  const box = (n: React.ReactNode, action?: React.ReactNode) => (
    <div className="p-4">
      {n}
      {action}
    </div>
  );
  switch (failure.kind) {
    case "serverDown":
      return box(
        <Notice tone="warning" title="The share's server is not running">
          It is starting, or starting again, as after the share grew. Its files are listed once it runs. <span className="text-subtle">({failure.message})</span>
        </Notice>,
        retry,
      );
    case "oldServer":
      return box(
        <Notice tone="info" title="This share's server cannot list files yet">
          It started before servers could list a share's files, and lists them once it starts again: when the share grows, when its networks change, or when the platform restarts it. Machines using the share are not affected.
        </Notice>,
      );
    case "notFound":
      return box(<Notice tone="warning" title="No such directory">It may have been removed or renamed since it was listed.</Notice>, toRoot);
    case "refused":
      return box(<Notice tone="warning" title="This directory cannot be listed">{failure.message}</Notice>, toRoot);
    case "tooLarge":
      return box(
        <Notice tone="info" title="This directory is too large to list here">
          It has more than 100,000 entries. A machine that mounts the share sees all of them.
        </Notice>,
        toRoot,
      );
    case "deleting":
      return box(<Notice tone="info" title="This share is being deleted">Its files are no longer listed.</Notice>);
    case "other":
      return <ErrorNotice error={failure.error} title="The directory did not load" onRetry={onRetry} />;
  }
}
