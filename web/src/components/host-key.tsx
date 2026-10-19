// A VM's SSH host key as the apiserver recorded it on the first login it
// made, which every later one must match (docs/decisions/vm-ssh.md), and
// forgetting it, as when the VM's host keys were made again.

import { useMutation } from "@connectrpc/connect-query";
import type { VirtualMachine } from "@hangar/api/hangar/compute/v1alpha1/virtual_machine_pb";
import { MachineCapabilityService } from "@hangar/api/hangar/compute/v1alpha1/machine_capability_pb";
import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { createPortal } from "react-dom";
import { Dialog } from "@/components/dialog";
import { Notice } from "@/components/notice";
import { Button } from "@/components/ui";
import { errorMessage } from "@/lib/api";
import { date } from "@/lib/machines";
import { useCan } from "@/lib/permissions";

/** The permission forgetting a VM's host keys needs: changing the VM, not only logging in to it. */
export const forgetPermission = "compute.virtualmachines.apply";

/**
 * fingerprint is a key in authorized_keys form as ssh-keygen -l shows it:
 * its type and SHA256:base64 of its blob; undefined when it cannot be
 * worked out, as for a key that is not base64, or outside a secure context,
 * where browsers give no crypto.subtle.
 */
export async function fingerprint(authorizedKey: string): Promise<string | undefined> {
  const [type, b64] = authorizedKey.trim().split(/\s+/);
  if (!type || !b64) return undefined;
  try {
    const blob = Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
    const sum = new Uint8Array(await crypto.subtle.digest("SHA-256", blob));
    return `${type} SHA256:${btoa(String.fromCharCode(...sum)).replace(/=+$/, "")}`;
  } catch {
    return undefined;
  }
}

/** The key's fingerprint, or the key itself where no fingerprint can be worked out. */
function Fingerprint({ k }: { k: string }) {
  const [fp, setFp] = useState<{ of: string; text: string | undefined }>();
  useEffect(() => {
    let live = true;
    void fingerprint(k).then((text) => live && setFp({ of: k, text }));
    return () => {
      live = false;
    };
  }, [k]);
  const shown = fp?.of === k ? (fp.text ?? k) : "…";
  return <span className="break-all font-mono text-[12px]">{shown}</span>;
}

/** HostKeys shows the host keys recorded for a VM, and Forget where the caller may change the VM. */
export function HostKeys({ vm, project }: { vm: VirtualMachine; project: string }) {
  const name = vm.metadata?.name ?? "";
  const keys = vm.status?.sshHostKeys ?? [];
  const { can } = useCan({ level: "project", name: project }, [forgetPermission]);
  const [asking, setAsking] = useState(false);
  const client = useQueryClient();
  const forget = useMutation(MachineCapabilityService.method.forgetSSHHostKeys);
  const close = () => {
    forget.reset();
    setAsking(false);
  };
  if (keys.length === 0) return <span className="text-subtle">Not recorded yet; the first terminal login records it</span>;
  return (
    <span className="block">
      {keys.map((k) => (
        <span key={k.publicKey} className="block">
          <Fingerprint k={k.publicKey} />
          <span className="block text-[11.5px] text-subtle">recorded {date(k.recordTime)?.toISOString().replace("T", " ").slice(0, 16)} UTC</span>
        </span>
      ))}
      {can(forgetPermission) && (
        <Button size="sm" className="mt-1.5" onClick={() => setAsking(true)}>
          Forget host key
        </Button>
      )}
      {/* Outside the summary's field, whose break-all would break the dialog's prose mid-word. */}
      {createPortal(
      <Dialog open={asking} onClose={close} title={`Forget ${name}'s host key`}>
        <p>
          The next terminal login trusts whatever key the VM then shows, and records it. Forget it only when the VM's host keys were made
          again in it; a key that changed for no reason you know of may be something else answering.
        </p>
        {forget.error != null && (
          <Notice tone="error" title="Forgetting the host key failed" className="mt-3">
            {errorMessage(forget.error)}
          </Notice>
        )}
        <div className="mt-5 flex justify-end gap-2">
          <Button onClick={close}>Cancel</Button>
          <Button
            variant="danger"
            disabled={forget.isPending}
            onClick={() =>
              forget.mutate(
                { project, kind: "VirtualMachine", name },
                {
                  onSuccess: () => {
                    // The record is in the VM's status: only the machine lists change.
                    void client.invalidateQueries({ predicate: (q) => /ListVirtualMachines|ListMachines/.test(JSON.stringify(q.queryKey)) });
                    close();
                  },
                },
              )
            }
          >
            {forget.isPending ? "Forgetting…" : "Forget host key"}
          </Button>
        </div>
      </Dialog>,
        document.body,
      )}
    </span>
  );
}
