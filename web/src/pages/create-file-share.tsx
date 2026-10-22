// A new file share, its networks chosen as cards that count their machines.
// Nothing is sent until the project's shares are known and the name is not
// among them, and the one apply is create-only (lib/create.ts).

import { Link, useNavigate } from "@tanstack/react-router";
import { useState, type ReactNode } from "react";
import { machinesByNetwork, OrNames, type MachineRef } from "@/components/file-shares";
import { Input } from "@/components/form";
import { Choice, CreateGate, FormManifest, FormPage, FormSection, FormSummary } from "@/components/form-page";
import { CappedNames, MachinesPopover, namesShown } from "@/components/machine-names";
import { useProjectName } from "@/components/shell";
import { ErrorNotice, Skeleton } from "@/components/states";
import { cn } from "@/lib/cn";
import { checkName } from "@/lib/create";
import { byDefaultFirst, newFileShare, sharePerms, shareName, shareYaml, squashWords, useCreateFileShare, useFileShares, type Squash } from "@/lib/file-shares";
import { count, nameOf, useAttachments, useNetworks } from "@/lib/network";
import { useCan } from "@/lib/permissions";

/** The most networks a share is on (docs/decisions/file-shares.md). */
const maxNetworks = 8;

/** CreateFileShare is the New file share page; networks opens it with those chosen, [default] otherwise. */
export function CreateFileShare({ networks }: { networks?: string[] }) {
  const project = useProjectName();
  const perm = useCan({ level: "project", name: project }, [sharePerms.apply]);
  const crumbs = [
    <Link key="list" to="/projects/$project/file-shares" params={{ project }} className="hover:text-foreground">
      File shares
    </Link>,
    "New file share",
  ];
  return (
    <CreateGate crumbs={crumbs} title="New file share" perm={perm} may={perm.can(sharePerms.apply)} refusal="creating file shares">
      <ShareForm crumbs={crumbs} project={project} initialNetworks={networks?.length ? networks : ["default"]} />
    </CreateGate>
  );
}

/**
 * The networks as cards, any number chosen. A card is as tall whatever the
 * number of machines on its network: it counts them, names the first two,
 * and lists them all behind "+N more".
 */
function NetworkPicker({ project, value, onChange }: { project: string; value: string[]; onChange: (n: string[]) => void }) {
  const nets = useNetworks(project);
  const att = useAttachments(project);
  const by = machinesByNetwork(att.items);
  if (nets.loading) {
    return (
      <div className="grid max-w-2xl grid-cols-1 gap-3 sm:grid-cols-2">
        {[0, 1].map((i) => (
          <Skeleton key={i} className="h-[104px] rounded-lg" />
        ))}
      </div>
    );
  }
  if (nets.error) return <ErrorNotice error={nets.error} title="The networks did not load" onRetry={nets.refetch} className="max-w-2xl p-0" />;
  const names = nets.items.map(nameOf).sort(byDefaultFirst);
  const chosen = names.filter((n) => value.includes(n));
  const can = new Map<string, MachineRef>();
  for (const n of chosen) for (const m of by.get(n) ?? []) can.set(`${m.kind}/${m.name}`, m);
  const canMachines = [...can.values()];
  return (
    <div className="max-w-2xl">
      <div role="group" aria-label="Networks" className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        {names.map((name) => {
          const n = nets.items.find((x) => nameOf(x) === name);
          const ms = by.get(name) ?? [];
          const on = value.includes(name);
          const rest = ms.length - namesShown;
          return (
            <div key={name} className="relative">
              <Choice
                role="checkbox"
                selected={on}
                onClick={() => onChange(on ? value.filter((x) => x !== name) : [...value, name])}
                label={name}
                className="h-[104px] w-full p-3.5"
              >
                <div className="flex items-baseline gap-2 pr-6">
                  <span className="truncate font-mono text-[13.5px] font-medium">{name}</span>
                  <span className="font-mono text-[11.5px] text-muted">{n?.spec?.ipv4Cidr}</span>
                </div>
                <div className="mt-2 h-5 text-[12.5px] leading-5">
                  {att.loading ? (
                    <Skeleton className="h-3 w-24" />
                  ) : att.error ? (
                    <span className="text-subtle">Machines not loaded</span>
                  ) : ms.length === 0 ? (
                    <span className="text-subtle">No machine is on it</span>
                  ) : (
                    count(ms.length, "machine")
                  )}
                </div>
                <div className={cn("mt-auto h-5 truncate font-mono text-[11.5px] leading-5 text-muted", rest > 0 && "pr-24")}>
                  {ms
                    .slice(0, namesShown)
                    .map((m) => m.name)
                    .join(", ")}
                </div>
              </Choice>
              {rest > 0 && (
                <MachinesPopover
                  machines={ms}
                  label={`+${rest} more`}
                  title={
                    <>
                      {count(ms.length, "machine")} on <span className="font-mono">{name}</span>
                    </>
                  }
                  align="right"
                  className="absolute bottom-3.5 right-3.5 h-5 leading-5"
                />
              )}
            </div>
          );
        })}
      </div>
      {att.error != null && <ErrorNotice error={att.error} title="The project's machines did not load" onRetry={att.refetch} className="mt-3 p-0" />}
      <div className="mt-3 flex items-start gap-2 rounded-md border border-dashed px-3 py-2.5 text-[12.5px]">
        <span className={cn("mt-1.5 size-1.5 shrink-0 rounded-full", chosen.length ? "bg-accent" : "bg-fail")} />
        {chosen.length === 0 ? (
          <span className="text-fail">No network is chosen; a file share is on at least one.</span>
        ) : (
          <span>
            <span className="text-foreground">
              Machines with an interface on <OrNames names={chosen} /> can use this share
              {att.loading || att.error ? (
                "."
              ) : canMachines.length > 0 ? (
                <>
                  : <CappedNames machines={canMachines} title={<>Machines on <OrNames names={chosen} /></>} />
                </>
              ) : (
                "; no machine is on them now."
              )}
            </span>
          </span>
        )}
      </div>
    </div>
  );
}

function ShareForm({ crumbs, project, initialNetworks }: { crumbs: ReactNode[]; project: string; initialNetworks: string[] }) {
  const navigate = useNavigate();
  const [name, setName] = useState("");
  const [networks, setNetworks] = useState<string[]>(initialNetworks);
  const [sizeText, setSizeText] = useState("50");
  const [squash, setSquash] = useState<Squash>("None");
  const [anonUid, setAnonUid] = useState("65534");
  const [anonGid, setAnonGid] = useState("65534");
  const nets = useNetworks(project);
  const shares = useFileShares(project);
  const create = useCreateFileShare();

  const known = nets.items.map(nameOf);
  // A network named in the address that the project lacks is not chosen.
  const chosen = networks.filter((n) => nets.loading || known.includes(n)).sort(byDefaultFirst);
  const size = sizeText.trim() === "" ? NaN : Number(sizeText);
  const sizeProblem = !Number.isInteger(size) || size < 1 ? "The size is not a whole number of GiB." : undefined;
  const idProblem = (v: string) => (!/^\d+$/.test(v) || Number(v) >= 4294967295 ? "not a user or group ID" : undefined);
  const anonProblem =
    squash === "All" && (idProblem(anonUid) || idProblem(anonGid)) ? "The anonymous user and group IDs are whole numbers below 4294967295." : undefined;

  const nameCheck = checkName(name, "file share", { loading: shares.loading, error: shares.error, names: shares.items.map(shareName) });
  const problems: string[] = [];
  if (!nameCheck.ok) problems.push(nameCheck.reason);
  if (nets.loading) problems.push("The networks are loading.");
  else if (chosen.length === 0) problems.push("No network is chosen.");
  else if (chosen.length > maxNetworks) problems.push(`${chosen.length} networks are chosen; a file share is on at most ${maxNetworks}.`);
  if (sizeProblem) problems.push(sizeProblem);
  if (anonProblem) problems.push(anonProblem);

  // A whole number as the browser reads it ("1e1" is 10) goes out as its digits: the API takes no exponent.
  const sizeSent = Number.isInteger(size) ? String(size) : sizeText.trim();
  const fields = { networks: chosen, size: sizeSent, squash, anonUid, anonGid };
  const share = newFileShare(project, name, fields);
  const yaml = shareYaml(newFileShare(project, name || "NAME", fields));
  const submit = () => {
    if (problems.length > 0) return;
    create.mutate(share, {
      onSuccess: () => void navigate({ to: "/projects/$project/file-shares/$name", params: { project, name }, replace: true }),
    });
  };
  return (
    <FormPage
      crumbs={crumbs}
      title="New file share"
      onSubmit={submit}
      side={
        <>
          <FormSummary
            summary={
              <>
                <div className="flex items-baseline gap-2">
                  <span className="text-[15px] font-medium">File share</span>
                  <span className="font-mono text-[13px] text-muted tabular">{sizeText || "–"} GiB</span>
                </div>
                <div className="mt-1 space-y-0.5 text-[12px] text-muted">
                  {chosen.length === 0 && <div>On no network</div>}
                  {chosen.map((n) => (
                    <div key={n}>
                      On <span className="font-mono text-foreground">{n}</span>
                      <span className="font-mono"> · {nets.items.find((x) => nameOf(x) === n)?.spec?.ipv4Cidr}</span>
                    </div>
                  ))}
                </div>
                <div className="mt-0.5 text-[12px] text-muted">Squash {squashWords[squash].label}</div>
              </>
            }
            problems={problems}
            error={create.error}
            failed="Creating the file share failed"
            pending={create.isPending}
            pendingLabel="Creating…"
            label={shares.loading ? "Loading names…" : "Create file share"}
            cancelTo={`/projects/${project}/file-shares`}
          />
          <FormManifest yaml={yaml} cli={[`hangar apply --create-only -p ${project} -f ${name || "file-share"}.yaml`]} />
        </>
      }
    >
      <FormSection n={1} title="File share name">
        <div className="max-w-xl">
          <Input value={name} onChange={(e) => setName(e.target.value)} aria-label="File share name" spellCheck={false} placeholder="assets" />
        </div>
      </FormSection>

      <FormSection n={2} title="Networks">
        <NetworkPicker project={project} value={networks} onChange={setNetworks} />
      </FormSection>

      <FormSection n={3} title="Size">
        <div className="max-w-xl">
          <div className="flex items-center gap-2">
            <Input
              type="number"
              inputMode="numeric"
              min={1}
              step={1}
              value={sizeText}
              onChange={(e) => setSizeText(e.target.value)}
              aria-label="Size, GiB"
              aria-invalid={!!sizeProblem}
              className={cn("w-32 tabular", sizeProblem && "border-fail focus:border-fail")}
            />
            <span className="text-[13px] text-muted">GiB</span>
          </div>
          {sizeProblem && <p className="mt-1.5 text-[12px] text-fail">{sizeProblem}</p>}
          <p className="mt-1.5 text-[12px] text-subtle">Can grow later, never shrink.</p>
        </div>
      </FormSection>

      <FormSection n={4} title="Access">
        <div role="radiogroup" aria-label="Squash" className="grid max-w-2xl grid-cols-1 gap-3 sm:grid-cols-3">
          {(["None", "Root", "All"] as const).map((q) => (
            <Choice key={q} selected={squash === q} onClick={() => setSquash(q)} label={`Squash ${q}`} className="p-3.5">
              <div className="pr-5 text-[13.5px] font-medium">{squashWords[q].label}</div>
              <div className="mt-0.5 text-[12px] leading-snug text-muted">{squashWords[q].text}</div>
            </Choice>
          ))}
        </div>
        {squash === "All" && (
          <div className="mt-3 flex max-w-2xl flex-wrap items-end gap-4">
            <label className="block">
              <span className="text-[12.5px] text-muted">Anonymous user ID</span>
              <Input value={anonUid} onChange={(e) => setAnonUid(e.target.value)} aria-label="Anonymous user ID" inputMode="numeric" className="mt-1 w-32 font-mono tabular" />
            </label>
            <label className="block">
              <span className="text-[12.5px] text-muted">Anonymous group ID</span>
              <Input value={anonGid} onChange={(e) => setAnonGid(e.target.value)} aria-label="Anonymous group ID" inputMode="numeric" className="mt-1 w-32 font-mono tabular" />
            </label>
            <span className="pb-2 text-[12px] text-subtle">65534 is nobody and nogroup on most systems.</span>
          </div>
        )}
        {anonProblem && <p className="mt-1.5 text-[12px] text-fail">{anonProblem}</p>}
      </FormSection>
    </FormPage>
  );
}
