// A new container's process, as a Compose service writes it: its
// entrypoint and command, its restart policy and its environment. Folded
// on the create page; every field may stay as it is, the image's own
// defaults then counting. Ports are not here yet: a port that is not
// public opens nothing, which a form would have to say plainly first.
//
// Compose's words, not the API's: Compose's entrypoint is the API's
// spec.command and Compose's command its spec.args, as Kubernetes names
// them. Each field says which it is, since the manifest beside the form
// shows the API's.

import { create } from "@bufbuild/protobuf";
import { EnvVarSchema, RestartPolicy, type EnvVar } from "@hangar/api/hangar/compute/v1alpha1/container_pb";
import { ChevronDown, ChevronRight, Info, Plus, Trash2 } from "lucide-react";
import { Input } from "@/components/form";
import { cn } from "@/lib/cn";
import { splitWords } from "@/lib/shell-words";

export type Restart = "always" | "on-failure" | "no";
export type EnvRow = { id: number; name: string; value: string };
export type Advanced = { entrypoint: string; command: string; restart: Restart; env: EnvRow[] };

export const noAdvanced: Advanced = { entrypoint: "", command: "", restart: "always", env: [] };

const restarts: { id: Restart; label: string; detail: string }[] = [
  { id: "always", label: "always", detail: "Boot again whenever the process ends. The default." },
  { id: "on-failure", label: "on-failure", detail: "Boot again only when it exits non-zero." },
  { id: "no", label: "no", detail: "Leave it off once the process ends." },
];

const envName = /^[A-Za-z_][A-Za-z0-9_]*$/;

/** What the fields amount to in a ContainerSpec, and what keeps them from being sent. */
export function advancedSpec(a: Advanced): {
  command: string[];
  args: string[];
  restartPolicy: RestartPolicy;
  env: EnvVar[];
  problems: string[];
} {
  const problems: string[] = [];
  const entry = splitWords(a.entrypoint);
  const cmd = splitWords(a.command);
  if (entry.error) problems.push(`Entrypoint: ${entry.error}`);
  if (cmd.error) problems.push(`Command: ${cmd.error}`);
  const env = a.env.filter((e) => e.name !== "" || e.value !== "");
  const envNames = new Set<string>();
  for (const e of env) {
    if (!envName.test(e.name)) problems.push(`The variable name "${e.name}" is not a name: letters, digits and _, not starting with a digit.`);
    else if (envNames.has(e.name)) problems.push(`The variable ${e.name} is set twice.`);
    envNames.add(e.name);
  }
  return {
    command: entry.words ?? [],
    args: cmd.words ?? [],
    // Always is the default, so it is left out of the manifest.
    restartPolicy: a.restart === "on-failure" ? RestartPolicy.ON_FAILURE : a.restart === "no" ? RestartPolicy.NEVER : RestartPolicy.UNSPECIFIED,
    env: env.map((e) => create(EnvVarSchema, { name: e.name, value: e.value })),
    problems,
  };
}

/** The folded section's one line: what differs from the image's defaults. */
function summary(a: Advanced): string[] {
  const out: string[] = [];
  if (a.entrypoint.trim()) out.push("entrypoint");
  if (a.command.trim()) out.push("command");
  out.push(`restart: ${a.restart}`);
  const env = a.env.filter((e) => e.name !== "").length;
  if (env) out.push(`${env} env`);
  return out;
}

let nextId = 1;

export function ContainerAdvanced({
  value: a,
  onChange,
  open,
  onToggle,
}: {
  value: Advanced;
  onChange: (a: Advanced) => void;
  open: boolean;
  onToggle: () => void;
}) {
  const set = (patch: Partial<Advanced>) => onChange({ ...a, ...patch });
  return (
    <div className="rounded-lg border bg-surface">
      <button
        type="button"
        aria-expanded={open}
        onClick={onToggle}
        className="flex w-full items-center gap-3 px-4 py-3 text-left hover:bg-surface-2/60"
      >
        {open ? <ChevronDown className="size-4 shrink-0 text-muted" /> : <ChevronRight className="size-4 shrink-0 text-muted" />}
        <span className="text-[13px] font-medium">Entrypoint, command, restart, environment</span>
        <span className="ml-auto flex min-w-0 flex-wrap justify-end gap-1.5">
          {summary(a).map((s) => (
            <span key={s} className="rounded border bg-background px-1.5 py-0.5 font-mono text-[11px] text-muted">
              {s}
            </span>
          ))}
        </span>
      </button>
      {open && (
        <div className="divide-y border-t">
          <div className="space-y-4 px-4 py-4">
            <WordsField
              label="Entrypoint"
              api="spec.command"
              replaces="ENTRYPOINT"
              value={a.entrypoint}
              onChange={(entrypoint) => set({ entrypoint })}
              placeholder="the image's ENTRYPOINT"
            />
            <WordsField
              label="Command"
              api="spec.args"
              replaces="CMD"
              value={a.command}
              onChange={(command) => set({ command })}
              placeholder="the image's CMD"
            />
          </div>

          <div className="px-4 py-4">
            <div className="flex items-baseline gap-2">
              <span className="text-[12.5px] font-medium">Restart</span>
              <span className="font-mono text-[11px] text-subtle">spec.restartPolicy</span>
            </div>
            <div role="radiogroup" aria-label="Restart" className="mt-2 grid grid-cols-1 gap-2 sm:grid-cols-3">
              {restarts.map((r) => (
                <button
                  key={r.id}
                  type="button"
                  role="radio"
                  aria-checked={a.restart === r.id}
                  onClick={() => set({ restart: r.id })}
                  className={cn(
                    "flex flex-col items-start justify-start rounded-md border px-3 py-2 text-left transition-colors",
                    a.restart === r.id ? "border-accent bg-accent-soft ring-1 ring-accent" : "hover:border-border-strong",
                  )}
                >
                  <div className="font-mono text-[12.5px] font-medium">{r.label}</div>
                  <div className="mt-0.5 text-[11.5px] text-muted">{r.detail}</div>
                </button>
              ))}
            </div>
            {a.restart === "always" && !a.entrypoint.trim() && !a.command.trim() && (
              <p className="mt-2 flex items-start gap-1.5 text-[11.5px] text-muted">
                <Info className="mt-0.5 size-3.5 shrink-0" strokeWidth={1.75} />
                <span>
                  An image whose own command ends at once, as a shell such as ubuntu's bash does with no terminal, boots again
                  and again under always; a command that keeps running, such as{" "}
                  <code className="whitespace-nowrap font-mono text-foreground">sleep infinity</code>, stays up.
                </span>
              </p>
            )}
          </div>

          <div className="px-4 py-4">
            <div className="flex items-baseline gap-2">
              <span className="text-[12.5px] font-medium">Environment</span>
              <span className="font-mono text-[11px] text-subtle">spec.env</span>
            </div>
            {a.env.length > 0 && (
              <div className="mt-2 space-y-2">
                {a.env.map((e, i) => (
                  <div key={e.id} className="flex items-center gap-2">
                    <Input
                      value={e.name}
                      onChange={(ev) => set({ env: a.env.map((x, j) => (j === i ? { ...x, name: ev.target.value } : x)) })}
                      placeholder="NAME"
                      aria-label={`Variable ${i + 1} name`}
                      spellCheck={false}
                      className="w-56 shrink-0"
                    />
                    <span className="font-mono text-[13px] text-subtle">=</span>
                    <Input
                      value={e.value}
                      onChange={(ev) => set({ env: a.env.map((x, j) => (j === i ? { ...x, value: ev.target.value } : x)) })}
                      placeholder="value"
                      aria-label={`Variable ${i + 1} value`}
                      spellCheck={false}
                    />
                    <RemoveButton label={`Remove variable ${e.name || i + 1}`} onClick={() => set({ env: a.env.filter((_, j) => j !== i) })} />
                  </div>
                ))}
              </div>
            )}
            <AddButton onClick={() => set({ env: [...a.env, { id: nextId++, name: "", value: "" }] })}>Add variable</AddButton>
            <p className="mt-1.5 text-[11.5px] text-subtle">Values are kept in the spec as written: anyone who can read the container reads them.</p>
          </div>
        </div>
      )}
    </div>
  );
}

/** A command line as one line, and the words it splits into under it. */
function WordsField({
  label,
  api,
  replaces,
  value,
  onChange,
  placeholder,
}: {
  label: string;
  api: string;
  replaces: string;
  value: string;
  onChange: (v: string) => void;
  placeholder: string;
}) {
  const split = splitWords(value);
  const words = split.words ?? [];
  return (
    <div>
      <label className="block">
        <span className="flex items-baseline gap-2">
          <span className="text-[12.5px] font-medium">{label}</span>
          <span className="font-mono text-[11px] text-subtle">{api}</span>
        </span>
        <Input
          value={value}
          onChange={(e) => onChange(e.target.value)}
          placeholder={placeholder}
          spellCheck={false}
          aria-invalid={!!split.error}
          className={cn("mt-1.5", split.error && "border-fail focus:border-fail")}
        />
      </label>
      <div className="mt-1.5 flex min-h-5 flex-wrap items-center gap-1 text-[11.5px]">
        {split.error ? (
          <span className="text-fail">{split.error}</span>
        ) : words.length === 0 ? (
          <span className="text-subtle">Empty keeps the image's {replaces}. Split as a shell splits it; quote a word with spaces.</span>
        ) : (
          <>
            <span className="mr-0.5 text-subtle">Replaces the image's {replaces} with</span>
            {words.map((w, i) => (
              <span key={i} className="rounded border bg-surface-2 px-1.5 font-mono text-[11px] text-foreground">
                {w === "" ? <span className="text-subtle">""</span> : w}
              </span>
            ))}
          </>
        )}
      </div>
    </div>
  );
}

function AddButton({ onClick, children }: { onClick: () => void; children: string }) {
  return (
    <button
      type="button"
      onClick={onClick}
      className="mt-2 flex h-7 items-center gap-1.5 rounded-md px-2 text-[12px] text-muted hover:bg-surface-2 hover:text-foreground"
    >
      <Plus className="size-3.5" />
      {children}
    </button>
  );
}

function RemoveButton({ label, onClick }: { label: string; onClick: () => void }) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-label={label}
      className="flex size-8 shrink-0 items-center justify-center rounded-md text-subtle hover:bg-surface-2 hover:text-fail"
    >
      <Trash2 className="size-3.5" />
    </button>
  );
}
