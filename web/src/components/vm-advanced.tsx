// A VM's rarely set fields, folded: the SSH keys cloud-init gives its
// default user and the user data it runs on the first boot, and the
// hardware it is given, its SMBIOS serial number and a TPM. None is needed
// to reach the VM: `hangar ssh` signs in without a key of its own.

import { ChevronDown, ChevronRight, Lock } from "lucide-react";
import type { ReactNode } from "react";
import { Input, Textarea } from "@/components/form";
import { cn } from "@/lib/cn";

export type VmAdvanced = { sshKeys: string; userData: string; serial: string; tpm: boolean };

export const noVmAdvanced: VmAdvanced = { sshKeys: "", userData: "", serial: "", tpm: false };

// The apiserver's limits on user data (internal/apiserver/virtualmachines.go)
// and the serial number (computeapi.CheckSystemSerialNumber).
const maxUserData = 16 * 1024;
const maxSerial = 64;

/** The keys as the API takes them: a line each, blank lines dropped. */
export const sshKeysOf = (a: VmAdvanced) =>
  a.sshKeys
    .split("\n")
    .map((l) => l.trim())
    .filter(Boolean);

const bytes = (s: string) => new TextEncoder().encode(s).length;

/** Why the apiserver would refuse the serial number. */
function serialProblem(s: string): string | undefined {
  const n = bytes(s);
  if (n > maxSerial) return `The serial number is ${n} bytes, at most ${maxSerial}.`;
  if (/[^\x20-\x7e]/.test(s)) return "The serial number is not printable ASCII.";
  return undefined;
}

/** What keeps the fields from being sent. The apiserver checks each key itself. */
export function vmAdvancedProblems(a: VmAdvanced): string[] {
  const n = bytes(a.userData);
  const serial = serialProblem(a.serial);
  return [...(n > maxUserData ? [`User data is ${n} bytes, at most ${maxUserData}.`] : []), ...(serial ? [serial] : [])];
}

function Group({ title, note, children }: { title: string; note?: string; children: ReactNode }) {
  return (
    <div className="space-y-4">
      <div className="flex items-baseline gap-2">
        <span className="text-[11px] font-medium uppercase tracking-wide text-subtle">{title}</span>
        {note && <span className="text-[11.5px] text-subtle">{note}</span>}
      </div>
      {children}
    </div>
  );
}

/**
 * The folded fields. `tpmLocked` is an existing VM's: its TPM was chosen
 * when it was created and cannot change.
 */
export function VmAdvancedFields({
  value: a,
  onChange,
  open,
  onToggle,
  tpmLocked = false,
}: {
  value: VmAdvanced;
  onChange: (a: VmAdvanced) => void;
  open: boolean;
  onToggle: () => void;
  tpmLocked?: boolean;
}) {
  const keys = sshKeysOf(a).length;
  const ud = bytes(a.userData);
  // A serial number of its own hides the platform's seed from cloud-init.
  const noSeed = a.serial !== "";
  const serial = serialProblem(a.serial);
  const tags = [keys && `${keys} SSH key${keys === 1 ? "" : "s"}`, ud && "user data", noSeed && "serial", a.tpm && "TPM"].filter(Boolean) as string[];
  return (
    <div className="max-w-xl rounded-lg border bg-surface">
      <button type="button" aria-expanded={open} onClick={onToggle} className="flex w-full items-center gap-3 px-4 py-3 text-left hover:bg-surface-2/60">
        {open ? <ChevronDown className="size-4 shrink-0 text-muted" /> : <ChevronRight className="size-4 shrink-0 text-muted" />}
        <span className="text-[13px] font-medium">cloud-init, hardware</span>
        <span className="ml-auto flex min-w-0 flex-wrap justify-end gap-1.5">
          {tags.map((s) => (
            <span key={s} className="rounded border bg-background px-1.5 py-0.5 font-mono text-[11px] text-muted">
              {s}
            </span>
          ))}
        </span>
      </button>
      {open && (
        <div className="space-y-6 border-t px-4 py-4">
          <Group title="cloud-init" note={noSeed ? "Off while a serial number is set" : undefined}>
            <div className={cn("space-y-4", noSeed && "opacity-50")}>
              <label className="block">
                <span className="flex items-baseline gap-2">
                  <span className="text-[12.5px] font-medium">SSH keys</span>
                  <span className="text-[11.5px] text-subtle">One per line</span>
                </span>
                <Textarea
                  value={a.sshKeys}
                  onChange={(e) => onChange({ ...a, sshKeys: e.target.value })}
                  disabled={noSeed}
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
                  <span className="text-[11.5px] text-subtle">First boot only</span>
                  <span className={ud > maxUserData ? "ml-auto font-mono text-[11px] text-fail" : "ml-auto font-mono text-[11px] text-subtle"}>
                    {(ud / 1024).toFixed(1)} / 16 KiB
                  </span>
                </span>
                <Textarea
                  value={a.userData}
                  onChange={(e) => onChange({ ...a, userData: e.target.value })}
                  disabled={noSeed}
                  aria-label="User data"
                  rows={6}
                  placeholder="#cloud-config"
                  spellCheck={false}
                  className="mt-1.5 text-[12px]"
                />
              </label>
            </div>
          </Group>
          <Group title="Hardware">
            <label className="block">
              <span className="flex items-baseline gap-2">
                <span className="text-[12.5px] font-medium">Serial number</span>
                <span className="text-[11.5px] text-subtle">SMBIOS, empty for the platform's</span>
              </span>
              <Input
                value={a.serial}
                onChange={(e) => onChange({ ...a, serial: e.target.value })}
                aria-label="Serial number"
                aria-invalid={!!serial}
                spellCheck={false}
                className={cn("mt-1.5 text-[12px]", serial && "border-fail focus:border-fail")}
              />
              {serial && <span className="mt-1 block text-[11.5px] text-fail">{serial}</span>}
            </label>
            <label className={cn("flex items-center gap-2 text-[12.5px]", tpmLocked && "text-muted")}>
              <input type="checkbox" checked={a.tpm} disabled={tpmLocked} onChange={(e) => onChange({ ...a, tpm: e.target.checked })} className="accent-[var(--accent)]" />
              <span className="font-medium">TPM 2.0</span>
              {tpmLocked && (
                <span className="ml-1 flex items-center gap-1 text-[11.5px] text-subtle">
                  <Lock className="size-3" /> Set when the VM was created
                </span>
              )}
            </label>
          </Group>
        </div>
      )}
    </div>
  );
}
