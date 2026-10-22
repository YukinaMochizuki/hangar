// A VM's rarely set fields, folded: the SSH keys cloud-init gives its
// default user, and the user data it runs on the first boot. Neither is
// needed to reach the VM: `hangar ssh` signs in without a key of its own.

import { ChevronDown, ChevronRight } from "lucide-react";
import { Textarea } from "@/components/form";

export type VmAdvanced = { sshKeys: string; userData: string };

export const noVmAdvanced: VmAdvanced = { sshKeys: "", userData: "" };

// The apiserver's limit on user data (internal/apiserver/virtualmachines.go).
const maxUserData = 16 * 1024;

/** The keys as the API takes them: a line each, blank lines dropped. */
export const sshKeysOf = (a: VmAdvanced) =>
  a.sshKeys
    .split("\n")
    .map((l) => l.trim())
    .filter(Boolean);

const bytes = (s: string) => new TextEncoder().encode(s).length;

/** What keeps the fields from being sent. The apiserver checks each key itself. */
export function vmAdvancedProblems(a: VmAdvanced): string[] {
  const n = bytes(a.userData);
  return n > maxUserData ? [`User data is ${n} bytes, at most ${maxUserData}.`] : [];
}

export function VmAdvancedFields({ value: a, onChange, open, onToggle }: { value: VmAdvanced; onChange: (a: VmAdvanced) => void; open: boolean; onToggle: () => void }) {
  const keys = sshKeysOf(a).length;
  const ud = bytes(a.userData);
  const tags = [keys && `${keys} SSH key${keys === 1 ? "" : "s"}`, ud && "user data"].filter(Boolean) as string[];
  return (
    <div className="max-w-xl rounded-lg border bg-surface">
      <button type="button" aria-expanded={open} onClick={onToggle} className="flex w-full items-center gap-3 px-4 py-3 text-left hover:bg-surface-2/60">
        {open ? <ChevronDown className="size-4 shrink-0 text-muted" /> : <ChevronRight className="size-4 shrink-0 text-muted" />}
        <span className="text-[13px] font-medium">SSH keys, user data</span>
        <span className="ml-auto flex min-w-0 flex-wrap justify-end gap-1.5">
          {tags.map((s) => (
            <span key={s} className="rounded border bg-background px-1.5 py-0.5 font-mono text-[11px] text-muted">
              {s}
            </span>
          ))}
        </span>
      </button>
      {open && (
        <div className="space-y-4 border-t px-4 py-4">
          <label className="block">
            <span className="flex items-baseline gap-2">
              <span className="text-[12.5px] font-medium">SSH keys</span>
              <span className="text-[11.5px] text-subtle">One per line</span>
            </span>
            <Textarea
              value={a.sshKeys}
              onChange={(e) => onChange({ ...a, sshKeys: e.target.value })}
              aria-label="SSH keys"
              rows={3}
              placeholder="ssh-ed25519 AAAA… me@laptop"
              spellCheck={false}
              className="mt-1.5 text-[12px]"
            />
          </label>
          <label className="block">
            <span className="flex items-baseline gap-2">
              <span className="text-[12.5px] font-medium">User data</span>
              <span className="text-[11.5px] text-subtle">cloud-init, first boot only</span>
              <span className={ud > maxUserData ? "ml-auto font-mono text-[11px] text-fail" : "ml-auto font-mono text-[11px] text-subtle"}>
                {(ud / 1024).toFixed(1)} / 16 KiB
              </span>
            </span>
            <Textarea
              value={a.userData}
              onChange={(e) => onChange({ ...a, userData: e.target.value })}
              aria-label="User data"
              rows={6}
              placeholder="#cloud-config"
              spellCheck={false}
              className="mt-1.5 text-[12px]"
            />
          </label>
        </div>
      )}
    </div>
  );
}
