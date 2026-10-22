// What the file share pages share: a share's state and networks, its dialogs
// (grow, networks, delete), each showing the API's error where it was asked,
// and the File shares cards of a network's and a VM's page.

import { Link } from "@tanstack/react-router";
import type { FileShare } from "@hangar/api/hangar/storage/v1alpha1/file_share_pb";
import { Box, ChevronDown, HardDrive, Network as NetworkIcon, Plus, Server } from "lucide-react";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { ConfirmDeleteDialog } from "@/components/delete-dialog";
import { Dialog } from "@/components/dialog";
import { CommandLine, FormEquivalent, Input } from "@/components/form";
import { CappedNames } from "@/components/machine-names";
import { Notice } from "@/components/notice";
import { StaleNotice } from "@/components/resource";
import { useProjectName } from "@/components/shell";
import { ErrorNotice, Skeleton, SkeletonRows } from "@/components/states";
import { Th } from "@/components/table";
import { Badge, Button, buttonVariants, Card, CardHeader, CopyButton, Led } from "@/components/ui";
import { errorMessage } from "@/lib/api";
import { cn } from "@/lib/cn";
import {
  addressOn,
  byDefaultFirst,
  declarationsOf,
  phaseWords,
  shareGiB,
  shareName,
  shareNetworks,
  sharePerms,
  shareUses,
  shareYaml,
  useDeleteFileShare,
  useFileShares,
  useGrowFileShare,
  useReloadShare,
  useSetNetworks,
  usesVia,
  vmMountCommands,
  withNetworks,
  type Declaration,
  type ShareUse,
} from "@/lib/file-shares";
import { machinePath, useMachines, type MachineKind, type MachineView } from "@/lib/machines";
import { count, nameOf, useNetworks, type Attachment } from "@/lib/network";
import { isStale } from "@/lib/network-actions";
import { useCan } from "@/lib/permissions";
import { formatNumber, quantityGiB } from "@/lib/quantity";

export type MachineRef = { kind: MachineKind; name: string; ipv4: string };

/** The project's declarations of shares, from its Containers' and VMs' specs. */
export function useDeclarations(project: string): { items: Declaration[]; machines: MachineView[]; loading: boolean; error: unknown; refetch: () => void } {
  const m = useMachines(project);
  return { items: m.items.flatMap(declarationsOf), machines: m.items, loading: m.loading, error: m.error, refetch: m.refetch };
}

/** The share controls' permissions, asked of TestPermissions at the project as one question. */
export function useShareCan(project: string) {
  return useCan({ level: "project", name: project }, [sharePerms.apply, sharePerms.delete]);
}

/** Which share controls the caller may use in the project; none while the answer is unknown or failed. */
export function useSharePerms(project: string) {
  const perm = useShareCan(project);
  return { canApply: perm.can(sharePerms.apply), canDelete: perm.can(sharePerms.delete), known: perm.known };
}

/** The machines with an interface on each network, by network, each once. */
export function machinesByNetwork(items: Attachment[]): Map<string, MachineRef[]> {
  const by = new Map<string, MachineRef[]>();
  for (const a of items) {
    const list = by.get(a.network) ?? [];
    if (!list.some((m) => m.kind === a.kind && m.name === a.name)) list.push({ kind: a.kind, name: a.name, ipv4: a.ipv4.split("/")[0] ?? "" });
    by.set(a.network, list);
  }
  return by;
}

/** Each machine of the project once, with the networks it has an interface on. */
export function machinesWithNetworks(items: Attachment[]) {
  const all = new Map<string, { kind: MachineKind; name: string; networks: { network: string; ipv4: string }[] }>();
  for (const a of items) {
    const k = `${a.kind}/${a.name}`;
    const e = all.get(k) ?? { kind: a.kind, name: a.name, networks: [] };
    if (!e.networks.some((n) => n.network === a.network)) e.networks.push({ network: a.network, ipv4: a.ipv4.split("/")[0] ?? "" });
    all.set(k, e);
  }
  return [...all.values()];
}

/** How many rows a card of machines shows before "Show all N". */
export const rowsShown = 5;

/** The row under a capped list that shows the rest, or folds it again. */
export function ShowAll({ all, n, onToggle }: { all: boolean; n: number; onToggle: () => void }) {
  return (
    <div className="border-t px-4 py-2">
      <button type="button" onClick={onToggle} className="inline-flex items-center gap-1 text-[12.5px] font-medium text-accent hover:underline">
        {all ? `Show the first ${rowsShown}` : `Show all ${n}`}
        <ChevronDown className={cn("size-3.5", all && "rotate-180")} />
      </button>
    </div>
  );
}

export const kindWord = (k: MachineKind) => (k === "Container" ? "container" : "VM");

/** A machine's name, linked, with its kind's icon. */
export function MachineChip({ m }: { m: { kind: MachineKind; name: string } }) {
  const project = useProjectName();
  return (
    <Link
      to={machinePath(m.kind)}
      params={{ project, name: m.name }}
      className="inline-flex h-5 max-w-full items-center gap-1 rounded border bg-surface px-1.5 font-mono text-[11.5px] hover:border-border-strong hover:text-accent"
    >
      {m.kind === "Container" ? <Box className="size-3 shrink-0 text-subtle" strokeWidth={1.75} /> : <Server className="size-3 shrink-0 text-subtle" strokeWidth={1.75} />}
      <span className="truncate">{m.name}</span>
    </Link>
  );
}

/** A share's phase, with its light; Deleting once it is being deleted. */
export function ShareState({ s }: { s: FileShare }) {
  const p = s.metadata?.deleteTime ? { label: "Deleting", tone: "pending" as const, pulse: true } : phaseWords(s);
  return (
    <span className="inline-flex items-center gap-2">
      <Led tone={p.tone} pulse={p.pulse} />
      {p.label}
    </span>
  );
}

/** A network's name, linked to its page. */
export function NetworkLink({ name, className }: { name: string; className?: string }) {
  const project = useProjectName();
  return (
    <Link to="/projects/$project/networks/$name" params={{ project, name }} className={cn("font-mono text-[12.5px] text-accent hover:underline", className)}>
      {name}
    </Link>
  );
}

/** What stands where a share's address on a network goes before its server has one. */
export const notServed = "Not served yet";

/** A share's networks as chips, each linked, with the share's address on it. */
export function NetworkChips({ s }: { s: FileShare }) {
  const project = useProjectName();
  return (
    <span className="flex flex-wrap gap-1.5">
      {shareNetworks(s).map((n) => (
        <Link
          key={n}
          to="/projects/$project/networks/$name"
          params={{ project, name: n }}
          className="inline-flex h-6 items-center gap-1.5 rounded border bg-surface px-1.5 font-mono text-[11.5px] hover:border-border-strong"
        >
          <span className="text-accent">{n}</span>
          <span className={addressOn(s, n) ? "text-muted" : "font-sans text-subtle"}>{addressOn(s, n) ?? notServed}</span>
        </Link>
      ))}
    </span>
  );
}

/** A name in running text, such as a share's or a network's, in the monospace face. */
export const Mono = ({ children }: { children: ReactNode }) => <span className="font-mono">{children}</span>;

/** Names in the monospace face, joined as a sentence joins them: "default or data". */
export function OrNames({ names, and }: { names: string[]; and?: boolean }) {
  return (
    <>
      {names.map((n, i) => (
        <span key={n}>
          {i > 0 && (i === names.length - 1 ? (and ? " and " : " or ") : ", ")}
          <span className="font-mono">{n}</span>
        </span>
      ))}
    </>
  );
}

/** A labelled block of copyable text, a command or YAML. */
export function CopyBlock({ label, text, prompt }: { label: ReactNode; text: string; prompt?: boolean }) {
  return (
    <div>
      {label && <div className="mb-1.5 text-[12px] text-muted">{label}</div>}
      <div className="flex items-start justify-between gap-2 rounded-md border bg-surface-2/60">
        <pre className="min-w-0 flex-1 overflow-x-auto px-3 py-2 font-mono text-[12px] leading-5">
          {text.split("\n").map((line, i) => (
            <span key={i} className="block">
              {prompt && <span className="select-none text-accent">$ </span>}
              {line}
            </span>
          ))}
        </pre>
        <CopyButton text={text} className="mr-1.5 mt-1.5 shrink-0" />
      </div>
    </div>
  );
}

const apiWords = (m: string) => m.replace(/^failed_precondition: /, "");

/** A use's state in a row, when it is not mounted: a word, the API's reason in its title and in UseReasons under the table. */
export function UseBadge({ u }: { u: Pick<ShareUse, "heldBack" | "notOnNode"> }) {
  const reason = u.heldBack ?? u.notOnNode;
  if (!reason) return null;
  return (
    <span className="block text-[11.5px] text-pending" title={apiWords(reason)}>
      {u.heldBack ? "Held back" : "Not mounted"}
    </span>
  );
}

/** Why uses are held back or not mounted, each reason the API gives once, with how many it holds. */
export function UseReasons({ uses }: { uses: ShareUse[] }) {
  const by = new Map<string, { held: boolean; n: number }>();
  for (const u of uses) {
    const reason = u.heldBack ?? u.notOnNode;
    if (!reason) continue;
    const key = `${u.heldBack ? "h" : "n"}:${apiWords(reason)}`;
    by.set(key, { held: !!u.heldBack, n: (by.get(key)?.n ?? 0) + 1 });
  }
  if (by.size === 0) return null;
  return (
    <div className="space-y-0.5 border-t px-4 py-2.5 text-[12px] text-pending">
      {[...by.entries()].map(([key, { held, n }]) => (
        <div key={key}>
          {held ? `${count(n, "container")} held back` : `Not mounted on ${count(n, "VM")}`}: {key.slice(2)}
        </div>
      ))}
    </div>
  );
}

/** A use's state, when it is not mounted for a reason the API gives. */
export function UseState({ u }: { u: Pick<ShareUse, "heldBack" | "notOnNode"> }) {
  if (u.heldBack) {
    return (
      <span className="block text-[11.5px] text-pending" title={u.heldBack}>
        Held back: {apiWords(u.heldBack)}
      </span>
    );
  }
  if (u.notOnNode) {
    return (
      <span className="block text-[11.5px] text-pending" title={u.notOnNode}>
        Not mounted: {u.notOnNode}
      </span>
    );
  }
  return null;
}

/**
 * GrowShareDialog sets a share's size, never below what its node holds.
 * Applied at the version read; a stale read applies nothing.
 */
export function GrowShareDialog({
  s,
  uses,
  usesKnown,
  open,
  onClose,
}: {
  s: FileShare;
  uses: ShareUse[];
  /** Whether the project's machines have loaded, so uses holds every one. */
  usesKnown: boolean;
  open: boolean;
  onClose: () => void;
}) {
  const name = shareName(s);
  const project = s.metadata?.project ?? "";
  const spec = quantityGiB(s.spec?.size);
  const held = quantityGiB(s.status?.size);
  const floor = Number.isFinite(held) ? Math.ceil(held) : 1;
  const [size, setSize] = useState(String(spec * 2));
  const grow = useGrowFileShare(project, name);
  // The size field takes the focus once open: the dialog gives it to its
  // first control, which a long list of users makes its "+N more".
  const sizeRef = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (!open) return;
    const t = requestAnimationFrame(() => sizeRef.current?.focus());
    return () => cancelAnimationFrame(t);
  }, [open]);
  const n = size.trim() === "" ? NaN : Number(size);
  const problem: ReactNode = !Number.isInteger(n) ? (
    "The size is not a whole number of GiB."
  ) : n === spec ? (
    <>
      <Mono>{name}</Mono> is already set to <Mono>{formatNumber(spec)} GiB</Mono>.
    </>
  ) : n < floor ? (
    <>
      <Mono>{name}</Mono> has <Mono>{formatNumber(floor)} GiB</Mono>; a file share never shrinks.
    </>
  ) : undefined;
  const close = () => {
    grow.reset();
    onClose();
  };
  return (
    <Dialog open={open} onClose={close} title={`Grow ${name}`}>
      <p>
        <Mono>{name}</Mono> has <Mono>{formatNumber(shareGiB(s))} GiB</Mono>. A file share never shrinks.
        {usesKnown && uses.length > 0 && (
          <>
            {" "}
            {count(uses.length, "machine")} use it: <CappedNames machines={uses} title={<>Using <span className="font-mono">{name}</span></>} />
          </>
        )}
      </p>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (!problem) grow.mutate({ sizeGi: n, shownGi: spec }, { onSuccess: close });
        }}
      >
        <label className="mt-4 block">
          <span className="text-[12.5px] text-foreground">New size</span>
          <span className="mt-1.5 flex items-center gap-2">
            <Input value={size} onChange={(e) => setSize(e.target.value)} aria-label="New size, GiB" inputMode="numeric" className="w-32 tabular" ref={sizeRef} />
            <span className="text-[13px] text-muted">GiB</span>
          </span>
        </label>
        {problem && <p className="mt-1.5 text-[12px] text-fail">{problem}</p>}
        <p className="mt-2 text-[12px] text-subtle">
          Its mounts pause for a few seconds while it grows. The growth counts toward the project's disk limit.
        </p>
        <CommandLine command={`hangar apply -p ${project} -f ${name}.yaml`} />
        {grow.error != null && (
          <Notice tone="error" title="Growing the file share failed" className="mt-3">
            {errorMessage(grow.error)}
          </Notice>
        )}
        <div className="mt-5 flex justify-end gap-2">
          <Button type="button" onClick={close}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabled={!!problem || grow.isPending}>
            <HardDrive />{" "}
            {grow.isPending ? (
              "Growing…"
            ) : (
              <>
                Grow to <Mono>{Number.isFinite(n) ? formatNumber(n) : size} GiB</Mono>
              </>
            )}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

/**
 * EditNetworksDialog sets a share's networks at the version it opened on, so a
 * newer share is never overwritten. Removing a network a machine mounts the
 * share from is refused by the API, and said so on its row and above the button.
 */
export function EditNetworksDialog({
  s: opened,
  decls,
  declsState,
  attachments,
  onClose,
  initial,
}: {
  s: FileShare;
  decls: Declaration[];
  /** Whether the project's machines, which decls come from, are loading or failed to. */
  declsState: { loading: boolean; error: unknown };
  attachments: Attachment[];
  onClose: () => void;
  initial: string[];
}) {
  const project = opened.metadata?.project ?? "";
  // The share as the dialog opened on it; the list refreshing does not move it.
  const [s, setS] = useState(opened);
  const name = shareName(s);
  const nets = useNetworks(project);
  const by = machinesByNetwork(attachments);
  const [value, setValue] = useState(initial);
  const apply = useSetNetworks();
  const reload = useReloadShare(project, name);
  const stale = isStale(apply.error);
  const uses = shareUses(s, decls);
  const on = shareNetworks(s);
  const names = [...new Set([...nets.items.map(nameOf), ...on])].sort(byDefaultFirst);
  const chosen = names.filter((n) => value.includes(n));
  const added = chosen.filter((n) => !on.includes(n));
  const removed = on.filter((n) => !value.includes(n));
  const refused = removed.filter((n) => usesVia(uses, n).length > 0);
  const changed = added.length + removed.length > 0;
  // Who mounts from a network removed is known only once the machines have loaded.
  const unknownUses = removed.length > 0 && (declsState.loading || declsState.error != null);
  const blocked = refused.length > 0 || chosen.length === 0 || chosen.length > 8 || unknownUses;
  const next = withNetworks(s, chosen);
  const label = !changed
    ? "Save networks"
    : added.length === 1 && removed.length === 0
      ? `Add ${added[0]}`
      : removed.length === 1 && added.length === 0
        ? `Remove ${removed[0]}`
        : "Save networks";
  const close = () => {
    apply.reset();
    onClose();
  };
  const startAgain = () =>
    reload.mutate(undefined, {
      onSuccess: (fresh) => {
        setS(fresh);
        apply.reset();
      },
    });
  return (
    <Dialog open onClose={close} title={`Networks of ${name}`} wide>
      <p>
        Machines with an interface on one of these networks can use <Mono>{name}</Mono>, each from its address on its own network.
      </p>
      {nets.error != null && <ErrorNotice error={nets.error} title="The networks did not load" onRetry={nets.refetch} className="p-0 pt-3" />}
      <ul className="mt-4 divide-y rounded-md border">
        {names.map((n) => {
          const checked = value.includes(n);
          const via = usesVia(uses, n);
          const isRefused = refused.includes(n);
          const isAdded = added.includes(n);
          const machines = by.get(n) ?? [];
          return (
            <li key={n} className={cn("px-3 py-2.5 text-[13px]", isAdded && "bg-accent-soft", isRefused && "bg-[color-mix(in_oklch,var(--fail)_6%,transparent)]")}>
              <label className="flex cursor-pointer items-center gap-3">
                <input
                  type="checkbox"
                  checked={checked}
                  onChange={() => setValue(checked ? value.filter((x) => x !== n) : [...value, n])}
                  className="accent-[var(--accent)]"
                />
                <span className="w-20 truncate font-mono font-medium">{n}</span>
                <span className="w-28 font-mono text-[12px] text-muted">{nets.items.find((x) => nameOf(x) === n)?.spec?.ipv4Cidr}</span>
                <span className="w-28 font-mono text-[12px]">
                  {addressOn(s, n) ??
                    (isAdded ? (
                      <span className="font-sans text-accent">new address</span>
                    ) : on.includes(n) ? (
                      <span className="font-sans text-subtle">{notServed}</span>
                    ) : (
                      <span className="font-sans text-subtle">–</span>
                    ))}
                </span>
                <span className="min-w-0 flex-1 text-[12px] text-muted">
                  {via.length > 0 ? (
                    <>
                      Used from here by{" "}
                      <CappedNames
                        machines={via}
                        title={
                          <>
                            Using <Mono>{name}</Mono> from <Mono>{n}</Mono>
                          </>
                        }
                        align="right"
                      />
                    </>
                  ) : machines.length > 0 ? (
                    <>
                      On it: <CappedNames machines={machines} title={<>On <Mono>{n}</Mono></>} align="right" />
                    </>
                  ) : (
                    "No machine is on it"
                  )}
                </span>
              </label>
              {isRefused && (
                <div className="mt-1.5 pl-7 text-[12px] text-fail">
                  Removing <Mono>{n}</Mono> is refused while {via.length === 1 ? "the machine" : `the ${via.length} machines`} using <Mono>{name}</Mono>{" "}
                  from <Mono>{n}</Mono> declare{via.length === 1 ? "s" : ""} it.
                </div>
              )}
            </li>
          );
        })}
      </ul>
      <FormEquivalent yaml={shareYaml(next)} cli={[`hangar apply -p ${project} -f ${name}.yaml`]} />
      {refused.length > 0 ? (
        <Notice
          tone="warning"
          title={
            <>
              Removing <OrNames names={refused} /> is refused
            </>
          }
          className="mt-3"
        >
          {refused.map((n) => {
            const via = usesVia(uses, n);
            return (
              <span key={n} className="block">
                {count(via.length, "machine")} {via.length === 1 ? "mounts" : "mount"} <Mono>{name}</Mono> from <Mono>{n}</Mono>:{" "}
                <CappedNames
                  machines={via}
                  title={
                    <>
                      Using <Mono>{name}</Mono> from <Mono>{n}</Mono>
                    </>
                  }
                />
              </span>
            );
          })}
        </Notice>
      ) : unknownUses ? (
        <Notice tone="warning" title="Which machines mount from the networks removed is not known" className="mt-3">
          {declsState.error != null ? "The project's machines did not load." : "The project's machines are still loading."}
        </Notice>
      ) : chosen.length === 0 ? (
        <Notice tone="warning" title="No network is chosen" className="mt-3">
          A file share is on at least one network.
        </Notice>
      ) : chosen.length > 8 ? (
        <Notice tone="warning" title={`${chosen.length} networks are chosen`} className="mt-3">
          A file share is on at most eight.
        </Notice>
      ) : changed ? (
        <p className="mt-3 text-[12.5px] text-muted">
          {added.length > 0 && (
            <>
              <Mono>{name}</Mono> gets an address on <OrNames names={added} and />.{" "}
            </>
          )}
          {removed.length > 0 && (
            <>
              Its address on <OrNames names={removed} and /> goes.{" "}
            </>
          )}
          Its mounts pause for a few seconds while it changes.
        </p>
      ) : null}
      {stale && <StaleNotice title="The file share changed after this dialog opened." reloading={reload.isPending} error={reload.error} onReload={startAgain} />}
      {apply.error != null && !stale && (
        <Notice tone="error" title="Changing the networks failed" className="mt-3">
          {errorMessage(apply.error)}
        </Notice>
      )}
      <div className="mt-5 flex justify-end gap-2">
        <Button onClick={close}>Cancel</Button>
        <Button variant="primary" disabled={blocked || !changed || stale || apply.isPending} onClick={() => apply.mutate(next, { onSuccess: close })}>
          <NetworkIcon /> {apply.isPending ? "Applying…" : label}
        </Button>
      </div>
    </Dialog>
  );
}

/** DeleteShareDialog asks for the name typed; while any machine declares the share, the API refuses, which stands above the button. */
export function DeleteShareDialog({
  s,
  uses,
  usesKnown,
  open,
  onClose,
  after,
}: {
  s: FileShare;
  uses: ShareUse[];
  /** Whether the project's machines have loaded, so uses holds every declaration. */
  usesKnown: boolean;
  open: boolean;
  onClose: () => void;
  after?: () => void;
}) {
  const name = shareName(s);
  const project = s.metadata?.project ?? "";
  const del = useDeleteFileShare(project, name);
  // The API refuses over a live declarer alone; one being deleted lets the share go once it is gone.
  const live = uses.filter((u) => u.live);
  const leaving = uses.filter((u) => u.deleting);
  return (
    <ConfirmDeleteDialog
      open={open}
      onClose={onClose}
      title={`Delete ${name}`}
      name={name}
      consequence={
        <>
          The share and every file on it go. Its <Mono>{formatNumber(shareGiB(s))} GiB</Mono> no longer count toward the project's disk limit.
          {live.length === 0 && leaving.length > 0 && (
            <>
              {" "}
              It goes once <CappedNames machines={leaving} title={<>Being deleted</>} />, which {leaving.length === 1 ? "is" : "are"} being deleted,{" "}
              {leaving.length === 1 ? "is" : "are"} gone.
            </>
          )}
        </>
      }
      cli={`hangar delete fileshare ${name} -p ${project}`}
      blocked={
        live.length > 0 ? (
          <>
            The API refuses while any live machine declares this share. {count(live.length, "machine")} {live.length === 1 ? "does" : "do"}:{" "}
            <CappedNames machines={live} title={<>Using <Mono>{name}</Mono></>} />
          </>
        ) : !usesKnown ? (
          "The project's machines have not loaded, so which machines declare this share is not known."
        ) : undefined
      }
      del={del}
      after={after}
    />
  );
}

/** The shares on one network, for its page, which says whether the caller may create one (canApply). */
export function NetworkFileSharesCard({ network, canApply }: { network: string; canApply: boolean }) {
  const project = useProjectName();
  const shares = useFileShares(project);
  const decls = useDeclarations(project);
  const on = shares.items.filter((s) => shareNetworks(s).includes(network));
  return (
    <Card>
      <CardHeader
        title={
          <>
            File shares {!shares.loading && !shares.error && <span className="ml-1 font-mono text-[11.5px] font-normal text-subtle">{on.length}</span>}
          </>
        }
        action={
          canApply && (
            <Link to="/projects/$project/new/file-share" search={{ network }} params={{ project }} className={buttonVariants({ size: "sm" })}>
              <Plus /> New file share
            </Link>
          )
        }
      />
      {shares.loading ? (
        <SkeletonRows rows={2} />
      ) : shares.error ? (
        <ErrorNotice error={shares.error} title="The file shares did not load" onRetry={shares.refetch} />
      ) : on.length === 0 ? (
        <div className="px-4 py-3 text-[13px] text-muted">No file share is on this network.</div>
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full text-[13px]">
            <thead>
              <tr className="border-b text-left text-[11.5px] text-subtle">
                <Th>Share</Th>
                <Th>State</Th>
                <Th>Address here</Th>
                <Th>Size</Th>
                <Th>Used from here by</Th>
                <Th>Also on</Th>
              </tr>
            </thead>
            <tbody>
              {on.map((s) => {
                const name = shareName(s);
                const via = usesVia(shareUses(s, decls.items), network);
                const also = shareNetworks(s).filter((n) => n !== network);
                return (
                  <tr key={name} className="border-b last:border-0">
                    <td className="px-4 py-2.5">
                      <Link to="/projects/$project/file-shares/$name" params={{ project, name }} className="font-medium text-accent hover:underline">
                        {name}
                      </Link>
                    </td>
                    <td className="whitespace-nowrap px-4 py-2.5">
                      <ShareState s={s} />
                    </td>
                    <td className="whitespace-nowrap px-4 py-2.5 font-mono text-[12px]">
                      {addressOn(s, network) ?? <span className="font-sans text-subtle">{notServed}</span>}
                    </td>
                    <td className="whitespace-nowrap px-4 py-2.5 font-mono text-[12px] tabular">{formatNumber(shareGiB(s))} GiB</td>
                    <td className="whitespace-nowrap px-4 py-2.5 text-[12px] text-muted">
                      {decls.loading ? (
                        <Skeleton className="h-3 w-20" />
                      ) : decls.error != null ? (
                        <span className="text-fail">Not loaded</span>
                      ) : via.length ? (
                        <CappedNames
                          machines={via}
                          title={
                            <>
                              Using <Mono>{name}</Mono> from <Mono>{network}</Mono>
                            </>
                          }
                          align="right"
                        />
                      ) : (
                        <span className="text-subtle">None</span>
                      )}
                    </td>
                    <td className="px-4 py-2.5">
                      {also.length ? (
                        also.map((n, i) => (
                          <span key={n}>
                            {i > 0 && ", "}
                            <NetworkLink name={n} className="text-[12px]" />
                          </span>
                        ))
                      ) : (
                        <span className="text-subtle">–</span>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      {on.length > 0 && decls.error != null && (
        <ErrorNotice error={decls.error} title="The project's machines did not load" onRetry={decls.refetch} className="border-t" />
      )}
      <div className="border-t px-4 py-2.5 text-[12px] text-subtle">
        Machines on <span className="font-mono">{network}</span> can use these shares once they declare them, from the address here.
      </div>
    </Card>
  );
}

/** The shares a VM declares, each with its access, the address it mounts from and its mount commands. */
export function VmFileSharesCard({ m }: { m: MachineView }) {
  const project = useProjectName();
  const shares = useFileShares(project);
  const decls = declarationsOf(m);
  if (decls.length === 0) {
    return (
      <Card>
        <CardHeader title="File shares" />
        <div className="px-4 py-3 text-[13px] text-muted">This VM declares no file share.</div>
      </Card>
    );
  }
  // The VM's NFSv4 client identity: its Machine's ID, as a container's is.
  const id = m.machine?.metadata?.uid || m.uid;
  return (
    <Card>
      <CardHeader
        title={
          <>
            File shares <span className="ml-1 font-mono text-[11.5px] font-normal text-subtle">{decls.length}</span>
          </>
        }
      />
      {shares.error != null && <ErrorNotice error={shares.error} title="The file shares did not load" onRetry={shares.refetch} />}
      <ul className="divide-y">
        {decls.map((d) => {
          const s = shares.items.find((x) => shareName(x) === d.share);
          const u = s ? shareUses(s, decls).find((x) => x.kind === m.kind && x.name === m.name) : undefined;
          const network = u?.network ?? "";
          const address = u?.address;
          return (
            <li key={d.share} className="px-4 py-3 text-[13px]">
              <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
                <Link to="/projects/$project/file-shares/$name" params={{ project, name: d.share }} className="font-medium text-accent hover:underline">
                  {d.share}
                </Link>
                {d.readOnly ? <Badge>read-only</Badge> : <span className="text-[12px] text-muted">read-write</span>}
                <span className="text-[12px] text-muted">
                  {shares.loading ? null : !s ? (
                    <span className="text-pending">No file share of this name exists in the project.</span>
                  ) : network ? (
                    <>
                      from <NetworkLink name={network} className="text-[12px]" /> at{" "}
                      {address ? <span className="font-mono text-foreground">{address}</span> : <span className="text-subtle">no address: {notServed.toLowerCase()}</span>}
                    </>
                  ) : (
                    <>on none of this VM's networks</>
                  )}
                </span>
              </div>
              <UseState u={d} />
              {address && (
                <div className="mt-2">
                  <CopyBlock label="" text={vmMountCommands(d.share, address, d.readOnly, id)} prompt />
                </div>
              )}
            </li>
          );
        })}
      </ul>
      <div className="border-t px-4 py-2.5 text-[12px] text-subtle">
        The VM mounts each share itself, from the share's address on the VM's own network, its client identity set to its own ID first.
      </div>
    </Card>
  );
}
