// Pieces the network pages share: what the project's machines say about
// interfaces, a form's equivalent and footer, and delete by typed name.

import { Trash2 } from "lucide-react";
import { useState, type ReactNode } from "react";
import { Dialog } from "@/components/dialog";
import { Notice } from "@/components/notice";
import { useProjectName } from "@/components/shell";
import { ErrorNotice, Skeleton } from "@/components/states";
import { FormEquivalent } from "@/components/form";
import { Button } from "@/components/ui";
import { errorMessage } from "@/lib/api";
import { cn } from "@/lib/cn";
import { count } from "@/lib/network";
import { nouns, useDelete, type NetworkKind } from "@/lib/network-actions";

/** What the project's machines say about interfaces: loading, failed, or loaded. */
export type MachinesState = { loading: boolean; error: unknown; refetch: () => void };

/** A count read from the project's machines: a skeleton while they load, unknown if they failed. */
export function MemberCount({ state, n, word }: { state: MachinesState; n: number; word?: string }) {
  if (state.loading) return <Skeleton className="inline-block h-3 w-8 align-middle" />;
  if (state.error) return <span className="text-subtle">unknown</span>;
  return <span className="tabular">{word ? count(n, word) : n}</span>;
}

/** The project's machines failing to load, above what they would have filled in. */
export function MachinesNotice({ state, className }: { state: MachinesState; className?: string }) {
  if (!state.error) return null;
  return (
    <ErrorNotice
      error={state.error}
      title="The project's machines did not load, so which interfaces are on each network is not known"
      onRetry={state.refetch}
      className={cn("px-0 pb-3 pt-0", className)}
    />
  );
}

/**
 * Why a delete the API refuses while machines use the object cannot be
 * judged yet, or the fact that it will be refused; undefined when it can go.
 */
export function usageBlock(state: MachinesState, inUse: string | undefined): string | undefined {
  if (state.loading) return "The project's machines are still loading.";
  if (state.error) return "The project's machines did not load, so whether any uses it is not known.";
  return inUse;
}

/** The CLI's name for each kind. */
export const cliKinds: Record<NetworkKind, string> = { network: "network", securityGroup: "sg", address: "addr", publicEndpoint: "ep" };

/**
 * Delete, confirmed by typing the name, as DeleteX; its error is shown here.
 * When the console already knows the API will refuse, the fact stands above
 * the button and the button is off.
 */
export function DeleteDialog({
  kind,
  name,
  consequence,
  open,
  onClose,
  after,
  blocked,
}: {
  kind: NetworkKind;
  name: string;
  consequence: ReactNode;
  open: boolean;
  onClose: () => void;
  /** What follows a delete the API took, such as going to the list. */
  after?: () => void;
  blocked?: ReactNode;
}) {
  const project = useProjectName();
  const del = useDelete(kind, project, name);
  const [typed, setTyped] = useState("");
  const close = () => {
    setTyped("");
    del.reset();
    onClose();
  };
  return (
    <Dialog open={open} onClose={close} title={`Delete ${nouns[kind]} ${name}`}>
      <p>{consequence}</p>
      <label className="mt-4 block">
        <span className="text-[12.5px] text-foreground">
          Name, to confirm: <span className="font-mono">{name}</span>
        </span>
        <input
          value={typed}
          onChange={(e) => setTyped(e.target.value)}
          autoFocus
          spellCheck={false}
          aria-label="Name, to confirm"
          className="mt-1.5 h-9 w-full rounded-md border border-border-strong bg-background px-2.5 font-mono text-[13px] text-foreground outline-none focus:border-fail"
        />
      </label>
      <FormEquivalent cli={[`hangar delete ${cliKinds[kind]} ${name} -p ${project}`]} />
      {blocked && <Notice tone="warning" title={blocked} className="mt-3" />}
      {del.error && (
        <Notice tone="error" title="Deleting failed" className="mt-3">
          {errorMessage(del.error)}
        </Notice>
      )}
      <div className="mt-5 flex justify-end gap-2">
        <Button onClick={close}>Cancel</Button>
        <Button
          variant="danger"
          disabled={typed !== name || del.isPending || !!blocked}
          onClick={() =>
            del.mutate(undefined, {
              onSuccess: () => {
                close();
                after?.();
              },
            })
          }
        >
          <Trash2 /> {del.isPending ? "Deleting…" : "Delete"}
        </Button>
      </div>
    </Dialog>
  );
}

/**
 * A change the API refused because the object changed after the dialog
 * opened on it: the fact, and a button that starts the dialog again from the
 * object as it is now.
 */
export function StaleNotice({ title, reloading, error, onReload }: { title: string; reloading: boolean; error: unknown; onReload: () => void }) {
  return (
    <Notice tone="warning" title={title} className="mt-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <span>Nothing was applied.</span>
        <Button size="sm" disabled={reloading} onClick={onReload}>
          {reloading ? "Reloading…" : "Reload"}
        </Button>
      </div>
      {error != null && <div className="mt-1 font-mono text-[11.5px] text-fail">{errorMessage(error)}</div>}
    </Notice>
  );
}

/**
 * A form dialog's end: why it cannot be sent yet, the API's error, and the
 * buttons. The submit button is named for what it does.
 */
export function FormFooter({
  problems,
  blocked,
  error,
  failed,
  pending,
  pendingLabel,
  label,
  onSubmit,
  onCancel,
}: {
  problems: string[];
  /** Off for a reason stated elsewhere in the dialog. */
  blocked?: boolean;
  error: unknown;
  failed: string;
  pending: boolean;
  pendingLabel: string;
  label: ReactNode;
  onSubmit: () => void;
  onCancel: () => void;
}) {
  return (
    <>
      {error != null && (
        <Notice tone="error" title={failed} className="mt-3">
          {errorMessage(error)}
        </Notice>
      )}
      {problems.length > 0 && <div className="mt-3 text-[12px] text-subtle">{problems.join(" ")}</div>}
      <div className="mt-5 flex justify-end gap-2">
        <Button onClick={onCancel}>Cancel</Button>
        <Button variant="primary" disabled={problems.length > 0 || !!blocked || pending} onClick={onSubmit}>
          {pending ? pendingLabel : label}
        </Button>
      </div>
    </>
  );
}
