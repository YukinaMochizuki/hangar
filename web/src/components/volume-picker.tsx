// A new container's volumes, each a disk or a file share. A share on none of
// the container's networks is shown off with the two ways to reach it, in the
// order docs/decisions/file-shares.md ("Console") gives.

import { AlertTriangle, Check, ChevronDown, Folders, HardDrive, Info, Plus, Undo2, X } from "lucide-react";
import type { ReactNode } from "react";
import { usedAsText } from "@/components/storage";
import { Input } from "@/components/form";
import { CappedNames } from "@/components/machine-names";
import { useDeclarations } from "@/components/file-shares";
import { ErrorNotice, Skeleton } from "@/components/states";
import { Button } from "@/components/ui";
import { cn } from "@/lib/cn";
import { addressOn, mountNetwork, phaseWords, shareGiB, shareName, shareNetworks, shareUses, useFileShares } from "@/lib/file-shares";
import { formatNumber } from "@/lib/quantity";
import { gib, rootDiskOwner, useDisks } from "@/lib/storage";
import type { FileShare } from "@hangar/api/hangar/storage/v1alpha1/file_share_pb";

export type VolumeRow = { source: string; kind: "Disk" | "FileShare"; target: string; readOnly: boolean };

/** Networks to add to existing shares when the container is created, by share. */
export type ShareAdds = Record<string, string[]>;

/** The most networks a share is on. */
const maxShareNetworks = 8;

/** The networks a share will be on once the container is created. */
export const networksAfter = (s: FileShare, adds: ShareAdds) => [...shareNetworks(s), ...(adds[shareName(s)] ?? []).filter((n) => !shareNetworks(s).includes(n))];

/** What stops the volumes being sent, each a sentence. */
export function volumeProblems(rows: VolumeRow[]): string[] {
  const out: string[] = [];
  rows.forEach((r, i) => {
    const which = `Volume ${i + 1}`;
    if (!r.source) out.push(`${which} has no source.`);
    if (!r.target.startsWith("/") || r.target === "/") out.push(`${which}'s target is not an absolute path below /.`);
  });
  const targets = rows.map((r) => r.target).filter(Boolean);
  if (new Set(targets).size < targets.length) out.push("Two volumes have the same target.");
  const shares = rows.filter((r) => r.kind === "FileShare" && r.source).map((r) => r.source);
  if (new Set(shares).size < shares.length) out.push("A file share is mounted more than once.");
  return out;
}

function Group({ label, icon: Icon }: { label: string; icon: typeof HardDrive }) {
  return (
    <div className="flex items-center gap-1.5 border-b bg-surface-2/60 px-3 py-1.5 text-[11px] font-medium uppercase tracking-wide text-subtle first:rounded-t-md">
      <Icon className="size-3" strokeWidth={1.75} />
      {label}
    </div>
  );
}

function Option({
  selected,
  disabled,
  onClick,
  name,
  facts,
  children,
}: {
  selected: boolean;
  disabled?: boolean;
  onClick: () => void;
  name: string;
  facts: ReactNode;
  children?: ReactNode;
}) {
  return (
    <div className={cn("border-b last:rounded-b-md last:border-0", selected && "bg-accent-soft")}>
      <button
        type="button"
        role="option"
        aria-selected={selected}
        aria-disabled={disabled}
        disabled={disabled}
        onClick={onClick}
        className={cn("flex w-full items-center gap-3 px-3 py-2 text-left text-[13px]", disabled ? "cursor-default" : "hover:bg-surface-2/60")}
      >
        <span className={cn("w-24 shrink-0 truncate font-mono text-[12.5px] font-medium", disabled && "text-muted")}>{name}</span>
        <span className={cn("min-w-0 flex-1 truncate text-[12px]", disabled ? "text-subtle" : "text-muted")}>{facts}</span>
        {selected && <Check className="size-3.5 shrink-0 text-accent" strokeWidth={2.5} />}
      </button>
      {children}
    </div>
  );
}

/** Names in the monospace face, joined as a sentence joins them. */
function Names({ names, or }: { names: string[]; or?: boolean }) {
  return (
    <>
      {names.map((n, k) => (
        <span key={n}>
          {k > 0 && (k === names.length - 1 ? (or ? " or " : " and ") : ", ")}
          <span className="font-mono">{n}</span>
        </span>
      ))}
    </>
  );
}

/**
 * One way to use a share the container cannot reach yet: a link-style
 * button named for the action, in the left column of the ways' grid, and
 * what it changes, muted, in the right one. Off, it says why instead.
 */
function Way({ title, onClick, off, children }: { title: string; onClick: () => void; off?: string; children: ReactNode }) {
  return (
    <>
      <button
        type="button"
        onClick={onClick}
        disabled={!!off}
        className={cn(
          "-ml-1.5 inline-flex h-6 items-center gap-1.5 justify-self-start whitespace-nowrap rounded px-1.5 text-[12.5px] font-medium transition-colors",
          off ? "cursor-default text-subtle" : "text-accent hover:bg-accent-soft",
        )}
      >
        <Plus className="size-3.5" strokeWidth={2.25} />
        {title}
      </button>
      <span className="pt-[3px] text-[12px] leading-[18px] text-muted">{off ?? children}</span>
    </>
  );
}

export function VolumePicker({
  project,
  rows,
  onChange,
  open,
  onOpen,
  networks,
  onAddInterface,
  shareAdds,
  onShareAdds,
  applied,
  mayApplyShares,
}: {
  project: string;
  rows: VolumeRow[];
  onChange: (rows: VolumeRow[]) => void;
  /** The row whose source list is open. */
  open: number | null;
  onOpen: (i: number | null) => void;
  /** The networks the container has an interface on, its first interface's first. */
  networks: string[];
  onAddInterface: (network: string, share: string) => void;
  shareAdds: ShareAdds;
  /** The shares whose adds were applied already, by a create that then failed. */
  applied: string[];
  onShareAdds: (a: ShareAdds) => void;
  /** Whether the caller may apply shares: true, false once the answer says not, undefined while unknown or failed. */
  mayApplyShares: boolean | undefined;
}) {
  const disks = useDisks(project);
  const shares = useFileShares(project);
  const decls = useDeclarations(project);
  const set = (i: number, r: Partial<VolumeRow>) => onChange(rows.map((x, j) => (j === i ? { ...x, ...r } : x)));
  const own = networks[0] ?? "default";
  return (
    <div className="max-w-2xl space-y-3">
      {rows.length === 0 && <p className="text-[12.5px] text-muted">No volume: the container has its root disk alone, rebuilt when its image changes.</p>}
      {rows.map((r, i) => {
        const share = r.kind === "FileShare" ? shares.items.find((s) => shareName(s) === r.source) : undefined;
        // The machine's networks in order, as the apiserver picks the one it mounts from.
        const via = share ? mountNetwork(networks, networksAfter(share, shareAdds)) || undefined : undefined;
        const done = share ? applied.includes(shareName(share)) : false;
        const pendingAdd = share && !done && shareAdds[shareName(share)]?.length ? shareAdds[shareName(share)] : undefined;
        const viaAddress = share && via ? addressOn(share, via) : undefined;
        return (
          <div key={i} className="rounded-lg border bg-surface">
            <div className="flex flex-wrap items-end gap-3 px-3 py-3">
              <div className="block min-w-0 flex-1">
                <span className="text-[12px] text-muted">Source</span>
                <button
                  type="button"
                  aria-expanded={open === i}
                  aria-label={`Source of volume ${i + 1}`}
                  onClick={() => onOpen(open === i ? null : i)}
                  className={cn(
                    "mt-1 flex h-8 w-full items-center gap-2 rounded-md border bg-surface px-2.5 text-left text-[13px]",
                    open === i ? "border-accent ring-1 ring-accent" : "hover:border-border-strong",
                  )}
                >
                  {r.source ? (
                    <>
                      {r.kind === "Disk" ? <HardDrive className="size-3.5 text-muted" strokeWidth={1.75} /> : <Folders className="size-3.5 text-muted" strokeWidth={1.75} />}
                      <span className="font-mono text-[12.5px]">{r.source}</span>
                      <span className="text-[12px] text-subtle">{r.kind === "Disk" ? "disk" : via ? `file share, from ${via}` : "file share"}</span>
                    </>
                  ) : (
                    <span className="text-subtle">A disk or a file share</span>
                  )}
                  <ChevronDown className="ml-auto size-3.5 text-subtle" />
                </button>
              </div>
              <label className="block w-40">
                <span className="text-[12px] text-muted">Target</span>
                <Input value={r.target} onChange={(e) => set(i, { target: e.target.value })} aria-label="Target" placeholder="/data" className="mt-1 font-mono text-[12.5px]" spellCheck={false} />
              </label>
              <label className="flex h-8 items-center gap-1.5 text-[12.5px] text-muted">
                <input type="checkbox" checked={r.readOnly} onChange={(e) => set(i, { readOnly: e.target.checked })} className="accent-[var(--accent)]" />
                Read-only
              </label>
              <Button type="button" variant="ghost" size="icon" aria-label="Remove volume" onClick={() => onChange(rows.filter((_, j) => j !== i))}>
                <X />
              </Button>
            </div>
            {open === i && (
              <div role="listbox" aria-label="Source" className="mx-3 mb-3 rounded-md border">
                <Group label="Disks" icon={HardDrive} />
                {disks.loading ? (
                  <div className="border-b px-3 py-2.5">
                    <Skeleton className="h-3 w-40" />
                  </div>
                ) : disks.error ? (
                  <ErrorNotice error={disks.error} title="The disks did not load" onRetry={disks.refetch} className="border-b px-3 py-2" />
                ) : disks.items.length === 0 ? (
                  <div className="border-b px-3 py-2 text-[12px] text-subtle">This project has no disk.</div>
                ) : (
                  disks.items.map((d) => {
                    const name = d.metadata?.name ?? "";
                    const owner = rootDiskOwner(d);
                    const used = usedAsText(d);
                    return (
                      <Option
                        key={name}
                        name={name}
                        selected={r.kind === "Disk" && r.source === name}
                        disabled={!!owner || !!used}
                        onClick={() => set(i, { kind: "Disk", source: name })}
                        facts={
                          <>
                            <span className="font-mono">{gib(d.status?.size || d.spec?.size)}</span> ·{" "}
                            {used ? (
                              used
                            ) : owner ? (
                              <>
                                root disk of <span className="font-mono">{owner}</span>
                              </>
                            ) : (
                              "not attached · one machine at a time"
                            )}
                          </>
                        }
                      />
                    );
                  })
                )}
                <Group label="File shares" icon={Folders} />
                {shares.loading ? (
                  <div className="px-3 py-2.5">
                    <Skeleton className="h-3 w-40" />
                  </div>
                ) : shares.error ? (
                  <ErrorNotice error={shares.error} title="The file shares did not load" onRetry={shares.refetch} className="px-3 py-2" />
                ) : shares.items.length === 0 ? (
                  <div className="px-3 py-2 text-[12px] text-subtle">This project has no file share.</div>
                ) : (
                  shares.items.map((s) => {
                    const sname = shareName(s);
                    const on = shareNetworks(s);
                    const nets = networksAfter(s, shareAdds);
                    const reachable = nets.some((n) => networks.includes(n));
                    const added = shareAdds[sname];
                    const uses = shareUses(s, decls.items);
                    const full = on.length >= maxShareNetworks;
                    const addOff =
                      mayApplyShares === false
                        ? "This account's roles do not include changing file shares in this project."
                        : full
                          ? `${sname} is on ${maxShareNetworks} networks, the most a file share is on.`
                          : s.metadata?.deleteTime
                            ? `${sname} is being deleted.`
                            : undefined;
                    return (
                      <Option
                        key={sname}
                        name={sname}
                        selected={r.kind === "FileShare" && r.source === sname}
                        disabled={!reachable || !!s.metadata?.deleteTime}
                        onClick={() => set(i, { kind: "FileShare", source: sname })}
                        facts={
                          <>
                            on{" "}
                            {nets.map((n, k) => (
                              <span key={n}>
                                {k > 0 && ", "}
                                <span className={cn("font-mono", networks.includes(n) && "text-foreground", added?.includes(n) && "text-accent")}>{n}</span>
                              </span>
                            ))}{" "}
                            ·{" "}
                            <span className="font-mono">
                              {s.status?.addresses.length ? s.status.addresses.map((a) => a.address).join(", ") : phaseWords(s).label.toLowerCase()}
                            </span>{" "}
                            · <span className="font-mono">{formatNumber(shareGiB(s))} GiB</span>
                            {!decls.loading && !decls.error && uses.length > 0 && <> · used by {uses.length}</>}
                          </>
                        }
                      >
                        {!reachable && (
                          <div className="pb-3 pl-[7.5rem] pr-3">
                            <div className="flex items-center gap-1.5 text-[12px] leading-[18px] text-pending">
                              <AlertTriangle className="size-3.5 shrink-0" strokeWidth={2} />
                              <span>
                                On {on.length === 1 ? "network" : "networks"} <Names names={on} or />; this container has no interface on{" "}
                                {on.length === 1 ? <span className="font-mono">{on[0]}</span> : "any of them"}.
                              </span>
                            </div>
                            <div role="group" aria-label="Ways to use it" className="mt-1.5 grid grid-cols-[auto_minmax(0,1fr)] items-start gap-x-5 gap-y-1">
                              {/* The share first: a share only answers on its NFS port, so adding a network to it opens no path
                                  between networks, while a second interface makes the container a bridge between them. */}
                              {mayApplyShares !== undefined && (
                                <Way
                                  title={`Add ${own} to this share`}
                                  off={addOff}
                                  onClick={() => {
                                    onShareAdds({ ...shareAdds, [sname]: [own] });
                                    set(i, { kind: "FileShare", source: sname });
                                  }}
                                >
                                  Changes <span className="font-mono">{sname}</span>; its mounts pause a few seconds.
                                  {uses.length > 0 && (
                                    <>
                                      {" "}
                                      Also used by <CappedNames machines={uses} title={<>Using <span className="font-mono">{sname}</span></>} align="right" />
                                    </>
                                  )}
                                </Way>
                              )}
                              <Way
                                title={`Add interface on ${on[0]}`}
                                onClick={() => {
                                  onAddInterface(on[0] ?? "default", sname);
                                  set(i, { kind: "FileShare", source: sname });
                                }}
                              >
                                Changes this container only: a second interface, on <span className="font-mono">{on[0]}</span>.
                              </Way>
                            </div>
                          </div>
                        )}
                        {added && applied.includes(sname) ? (
                          <div className="mx-3 mb-3 grid grid-cols-[16px_minmax(0,1fr)] items-center gap-x-2 rounded-md border border-pending/50 bg-surface p-3 text-[12.5px]">
                            <Info className="size-4 text-pending" strokeWidth={1.9} />
                            <span className="text-foreground">
                              <span className="font-mono">{sname}</span> has network <span className="font-mono">{added.join(", ")}</span> now, added for this
                              container, which is not created yet.
                            </span>
                          </div>
                        ) : added ? (
                          <div className="mx-3 mb-3 grid grid-cols-[16px_minmax(0,1fr)_auto] items-center gap-x-2 rounded-md border border-accent/40 bg-surface p-3 text-[12.5px]">
                            <Info className="size-4 text-accent" strokeWidth={1.9} />
                            <span className="text-foreground">
                              <span className="font-mono">{sname}</span> will be updated with network <span className="font-mono">{added.join(", ")}</span> when
                              this container is created; its mounts pause a few seconds.
                            </span>
                            <Button
                              type="button"
                              size="sm"
                              onClick={() => {
                                const next = { ...shareAdds };
                                delete next[sname];
                                onShareAdds(next);
                              }}
                            >
                              <Undo2 /> Undo
                            </Button>
                          </div>
                        ) : null}
                      </Option>
                    );
                  })
                )}
              </div>
            )}
            {share && open !== i && !via && (
              <div className="border-t px-3 py-2 text-[12px] text-pending">
                <span className="font-mono">{shareName(share)}</span> is on none of this container's networks.
              </div>
            )}
            {share && open !== i && via && (
              <div className="border-t px-3 py-2 text-[12px] text-subtle">
                Mounted at <span className="font-mono">{r.target || "TARGET"}</span> from{" "}
                {viaAddress ? (
                  <>
                    <span className="font-mono">{viaAddress}</span> (<span className="font-mono">{via}</span>)
                  </>
                ) : (
                  <>
                    its {pendingAdd?.includes(via) ? "new " : ""}address on <span className="font-mono">{via}</span>
                  </>
                )}
                , nosuid,nodev; the image's files there are not copied onto the share.
                {!viaAddress && !pendingAdd && (
                  <>
                    {" "}
                    <span className="font-mono">{shareName(share)}</span> has no address on <span className="font-mono">{via}</span> yet.
                  </>
                )}
                {pendingAdd && (
                  <>
                    {" "}
                    <span className="font-mono">{shareName(share)}</span> is updated with network <span className="font-mono">{pendingAdd.join(", ")}</span> on create.
                  </>
                )}
              </div>
            )}
          </div>
        );
      })}
      <Button type="button" size="sm" onClick={() => onChange([...rows, { source: "", kind: "Disk", target: "", readOnly: false }])}>
        <Plus /> Add volume
      </Button>
    </div>
  );
}
