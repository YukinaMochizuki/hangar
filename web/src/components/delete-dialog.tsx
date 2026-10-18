// Delete, confirmed by typing the object's name, for every kind the console
// deletes.

import { Trash2 } from "lucide-react";
import { useState, type ReactNode } from "react";
import { Dialog } from "@/components/dialog";
import { CommandLine, Input } from "@/components/form";
import { Notice } from "@/components/notice";
import { Button } from "@/components/ui";
import { errorMessage } from "@/lib/api";

/** The delete call, as a mutation of no variables gives it. */
export type DeleteMutation = {
  mutate: (variables: undefined, options: { onSuccess: () => void }) => void;
  isPending: boolean;
  error: unknown;
  reset: () => void;
};

/**
 * ConfirmDeleteDialog says what deleting takes with it, asks for the name
 * typed, and sends the delete; its error is shown here. When the console
 * already knows the API will refuse, the blocked fact stands above the
 * button and the button is off.
 */
export function ConfirmDeleteDialog({
  open,
  onClose,
  title,
  name,
  consequence,
  cli,
  blocked,
  del,
  after,
}: {
  open: boolean;
  onClose: () => void;
  title: string;
  name: string;
  consequence: ReactNode;
  /** The command that does the same. */
  cli?: string;
  blocked?: ReactNode;
  del: DeleteMutation;
  /** What follows a delete the API took, such as going to the list. */
  after?: () => void;
}) {
  const [typed, setTyped] = useState("");
  const close = () => {
    setTyped("");
    del.reset();
    onClose();
  };
  return (
    <Dialog open={open} onClose={close} title={title}>
      <p>{consequence}</p>
      <label className="mt-4 block">
        <span className="text-[12.5px] text-foreground">
          Name, to confirm: <span className="font-mono">{name}</span>
        </span>
        <Input
          tone="danger"
          value={typed}
          onChange={(e) => setTyped(e.target.value)}
          autoFocus
          spellCheck={false}
          aria-label="Name, to confirm"
          className="mt-1.5"
        />
      </label>
      {cli && <CommandLine command={cli} />}
      {blocked && <Notice tone="warning" title={blocked} className="mt-3" />}
      {del.error != null && (
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
