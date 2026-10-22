// New port forward: a target interface, a protocol and the port the machine
// listens on. The public port is never chosen: the apiserver draws it at
// random on the shared IPv4 address when the forward is made. Screens only:
// the form sends nothing until the API kind exists (lib/port-forwards.ts).

import { Link } from "@tanstack/react-router";
import { ArrowRight, Shuffle } from "lucide-react";
import { useState, type ReactNode } from "react";
import { FormField, Input } from "@/components/form";
import { CreateGate, FormManifest, FormPage, FormSection, FormSummary } from "@/components/form-page";
import { Notice } from "@/components/notice";
import { InboundNotice, useInbound } from "@/components/port-forwards";
import { useProjectName } from "@/components/shell";
import { Button } from "@/components/ui";
import { checkName } from "@/lib/create";
import { cn } from "@/lib/cn";
import { kindNoun } from "@/lib/network";
import {
  portForwardPerms,
  portForwardYaml,
  portLabel,
  portProblem,
  publicPorts,
  suggestName,
  useForwardTargets,
  usePortForwardCan,
  usePortForwardQuota,
  usePortForwards,
  type ForwardProtocol,
  type ForwardTarget,
} from "@/lib/port-forwards";
import { InterfaceField, pickOf, pickProblem, pickTarget, type InterfacePick } from "@/pages/addresses";

const protocols: ForwardProtocol[] = ["TCP", "UDP"];

const presets: { label: string; protocol: ForwardProtocol; port: number }[] = [
  { label: "SSH", protocol: "TCP", port: 22 },
  { label: "RDP", protocol: "TCP", port: 3389 },
  { label: "WireGuard", protocol: "UDP", port: 51820 },
];

/** The forward's target as a pick stands for it; undefined for none. */
function targetOfPick(p: InterfacePick): ForwardTarget | undefined {
  const t = pickTarget(p);
  if (!t || (t.object.case !== "container" && t.object.case !== "virtualMachine")) return undefined;
  return { kind: t.object.case === "container" ? "Container" : "VirtualMachine", name: t.object.value, iface: t.interface || "eth0" };
}

/**
 * CreatePortForward is the New port forward page; target, as
 * Kind/machine/interface, starts it on that interface, as a machine's page
 * links to it.
 */
export function CreatePortForward({ target }: { target?: string }) {
  const project = useProjectName();
  const crumbs = [
    <Link key="list" to="/projects/$project/port-forwards" params={{ project }} className="hover:text-foreground">
      Port forwards
    </Link>,
    "New port forward",
  ];
  const { can } = usePortForwardCan(project);
  return (
    <CreateGate crumbs={crumbs} title="New port forward" perm={{ known: true, error: null }} may={can(portForwardPerms.apply)} refusal="creating port forwards">
      <PortForwardForm crumbs={crumbs} project={project} start={target ?? ""} />
    </CreateGate>
  );
}

function PortForwardForm({ crumbs, project, start }: { crumbs: ReactNode[]; project: string; start: string }) {
  const forwards = usePortForwards(project);
  const quota = usePortForwardQuota(project);
  const { attachments } = useForwardTargets(project);
  const [pick, setPick] = useState(pickOf(start));
  const [protocol, setProtocol] = useState<ForwardProtocol>("TCP");
  const [port, setPort] = useState("22");
  const [typedName, setTypedName] = useState<string | undefined>(undefined);
  const t = targetOfPick(pick);
  const portBad = portProblem(port);
  const portN = portBad ? 0 : Number(port);
  const name = typedName ?? suggestName(t?.name ?? "", protocol, portN);
  const inbound = useInbound(project, t ?? { kind: "Container", name: "", iface: "" }, protocol, portN);
  const shownInbound = t && !portBad ? inbound : undefined;

  const problems: string[] = [];
  const full = quota.limit !== undefined && quota.used >= quota.limit;
  if (!t) problems.push("No interface is chosen.");
  const typedProblem = pickProblem(pick);
  if (typedProblem) problems.push(typedProblem);
  if (portBad) problems.push(portBad);
  const check = checkName(name, "port forward", { loading: forwards.loading, error: forwards.error, names: forwards.items.map((f) => f.metadata.name) });
  if (!check.ok) problems.push(check.reason);
  const yaml = portForwardYaml({
    metadata: { name: name || "NAME" },
    spec: { target: t ?? { kind: "Container", name: "", iface: "eth0" }, protocol, targetPort: portN },
  });

  const quotaNotice = full && (
    <Notice tone="warning" title="The plan's limit is reached">
      This project holds {quota.used} port forwards, as many as its plan allows.
    </Notice>
  );
  const inboundNotice = shownInbound && shownInbound.known && shownInbound.verdict === "none" && t && (
    <Notice tone="warning" title="Nothing will get through yet">
      No security group of {t.name} · {t.iface} lets in {protocol} port {portN}. The forward is made, and reaches the {kindNoun(t.kind)} once a group
      lets it in.
    </Notice>
  );

  return (
    <FormPage
      crumbs={crumbs}
      title="New port forward"
      description={
        <>
          A port of the platform's shared IPv4 address, forwarded to a port of one interface of a machine in{" "}
          <span className="font-mono text-foreground">{project}</span>. The public port is drawn at random and kept until the forward is deleted.
        </>
      }
      // Screens only: nothing is sent until the API kind exists.
      onSubmit={() => {}}
      side={
        <>
          <FormSummary
            summary={
              <div className="space-y-1 text-[13px]">
                <div className="flex flex-wrap items-center gap-x-2 font-mono text-[12.5px]">
                  <span className="text-muted">shared IPv4 : drawn port</span>
                  <ArrowRight className="size-3.5 text-subtle" />
                  {t ? (
                    <span>
                      {t.name} · {t.iface} · {portN ? portLabel(portN, protocol) : "?"}
                    </span>
                  ) : (
                    <span className="font-sans text-subtle">no interface</span>
                  )}
                </div>
              </div>
            }
            notices={
              <>
                {quotaNotice}
                {inboundNotice}
              </>
            }
            problems={problems}
            blocked={full}
            error={null}
            failed="Creating the port forward failed"
            pending={false}
            pendingLabel="Creating…"
            label={forwards.loading ? "Loading names…" : "Create port forward"}
            cancelTo={`/projects/${project}/port-forwards`}
          />
          <FormManifest yaml={yaml} cli={[`hangar apply --create-only -p ${project} -f ${name || "port-forward"}.yaml`]} />
        </>
      }
    >
      <FormSection n={1} title="Target" hint="An interface of a container or virtual machine">
        <div className="max-w-xl">
          <InterfaceField
            pick={pick}
            onChange={setPick}
            none="Choose an interface"
            items={attachments.items}
            hint="Traffic arrives at this interface's private address, from the sender's own address."
          />
        </div>
      </FormSection>
      <FormSection n={2} title="Protocol and port" hint="What the machine listens on">
        <div className="max-w-xl">
          <div className="mb-3 flex flex-wrap items-center gap-2">
            <span className="text-[12px] text-subtle">Presets</span>
            {presets.map((p) => (
              <Button
                key={p.label}
                type="button"
                size="sm"
                onClick={() => {
                  setProtocol(p.protocol);
                  setPort(String(p.port));
                }}
              >
                {p.label} <span className="font-mono text-[11.5px] text-muted">{portLabel(p.port, p.protocol)}</span>
              </Button>
            ))}
          </div>
          {/* One row of two controls at the input's height, each labelled
              above; stacked on a phone. The protocol is the segmented
              control New secret uses for its two forms, its highlight one
              element that slides to the chosen option (still under reduced
              motion). */}
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-[220px_140px]">
            <div>
              <span id="pf-protocol" className="text-[12.5px] text-foreground">
                Protocol
              </span>
              <div
                role="radiogroup"
                aria-labelledby="pf-protocol"
                className="relative mt-1.5 grid h-9 grid-cols-2 items-stretch rounded-md border border-border-strong bg-surface p-0.5"
              >
                <span
                  aria-hidden
                  className={cn(
                    "absolute inset-y-0.5 left-0.5 w-[calc(50%-2px)] rounded bg-accent-soft ring-1 ring-accent transition-transform duration-200 ease-out motion-reduce:transition-none",
                    protocol === "UDP" && "translate-x-full",
                  )}
                />
                {protocols.map((p) => (
                  <button
                    key={p}
                    type="button"
                    role="radio"
                    aria-checked={protocol === p}
                    onClick={() => setProtocol(p)}
                    className={cn(
                      "relative rounded font-mono text-[13px] transition-colors duration-200 motion-reduce:transition-none",
                      protocol === p ? "font-medium text-foreground" : "text-muted hover:text-foreground",
                    )}
                  >
                    {p}
                  </button>
                ))}
              </div>
            </div>
            <FormField label="Target port" className="mt-0">
              <Input value={port} onChange={(e) => setPort(e.target.value.trim())} inputMode="numeric" aria-label="Target port" spellCheck={false} />
            </FormField>
          </div>
          {shownInbound && (
            <div className="mt-4">
              <InboundNotice t={t!} protocol={protocol} port={portN} inbound={shownInbound} />
            </div>
          )}
        </div>
      </FormSection>
      <FormSection n={3} title="Public port" hint="Assigned automatically">
        <div className="max-w-xl rounded-lg border border-dashed bg-surface-2/50 px-4 py-3">
          <div className="flex items-center gap-2 font-mono text-[13px]">
            <Shuffle className="size-3.5 text-subtle" />
            <span>shared IPv4</span>
            <span className="text-subtle">:</span>
            <span className="rounded bg-surface px-1.5 text-muted">
              {publicPorts.min}–{publicPorts.max}
            </span>
          </div>
          <p className="mt-2 text-[12.5px] text-muted">
            Drawn at random by Hangar from the free ports {publicPorts.min}–{publicPorts.max} of the platform's shared IPv4 address when the forward is
            made, and shown on its page. It cannot be chosen. It is kept until the forward is deleted; retargeting the forward keeps it.
          </p>
        </div>
      </FormSection>
      <FormSection n={4} title="Port forward name" hint="Unique among this project's port forwards">
        <div className="max-w-xl">
          <Input value={name} onChange={(e) => setTypedName(e.target.value.toLowerCase())} aria-label="Port forward name" spellCheck={false} />
          {typedName === undefined && name && <span className="mt-1 block text-[11.5px] text-subtle">Suggested from the target and the port.</span>}
        </div>
      </FormSection>
    </FormPage>
  );
}
