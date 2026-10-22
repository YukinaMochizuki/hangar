// New port forward: a target interface, a protocol and the port the machine
// listens on. The public port is never chosen: the apiserver draws it at
// random from 10000 to 32767 when the forward is made. Nothing is sent
// until the project's port forwards are known and the name is not among
// them, and the one apply is create-only (lib/create.ts).

import { create } from "@bufbuild/protobuf";
import { PortForwardSchema } from "@hangar/api/hangar/network/v1alpha1/port_forward_pb";
import { Link, useNavigate } from "@tanstack/react-router";
import { ArrowRight } from "lucide-react";
import { useState, type ReactNode } from "react";
import { FormField, Input } from "@/components/form";
import { CreateGate, FormManifest, FormPage, FormSection, FormSummary } from "@/components/form-page";
import { Notice } from "@/components/notice";
import { forwardTargetOfPick, InboundNotice, useInbound } from "@/components/port-forwards";
import { InterfaceField } from "@/components/interface-field";
import { useProjectName } from "@/components/shell";
import { checkName } from "@/lib/create";
import { cn } from "@/lib/cn";
import { nameOf, portForwardYaml, targetOf, usePortForwards } from "@/lib/network";
import { permission, useCreate } from "@/lib/network-actions";
import { useCan } from "@/lib/permissions";
import {
  portLabel,
  portProblem,
  freeName,
  protocolEnum,
  suggestName,
  usePortForwardQuota,
  type ForwardProtocol,
  type ForwardTarget,
} from "@/lib/port-forwards";

const applyPerm = permission("portForward", "apply");

const protocols: ForwardProtocol[] = ["TCP", "UDP"];

/**
 * CreatePortForward is the New port forward page; target, as
 * Kind/machine/interface, starts it on that interface, as a machine's page
 * links to it.
 */
export function CreatePortForward({ target }: { target?: string }) {
  const project = useProjectName();
  const perm = useCan({ level: "project", name: project }, [applyPerm]);
  const crumbs = [
    <Link key="list" to="/projects/$project/port-forwards" params={{ project }} className="hover:text-foreground">
      Port forwards
    </Link>,
    "New port forward",
  ];
  return (
    <CreateGate crumbs={crumbs} title="New port forward" perm={perm} may={perm.can(applyPerm)} refusal="creating port forwards">
      <PortForwardForm crumbs={crumbs} project={project} start={target ?? ""} />
    </CreateGate>
  );
}

function PortForwardForm({ crumbs, project, start }: { crumbs: ReactNode[]; project: string; start: string }) {
  const navigate = useNavigate();
  const forwards = usePortForwards(project);
  const quota = usePortForwardQuota(project);
  const make = useCreate("portForward", project);
  const [pick, setPick] = useState(start);
  const [protocol, setProtocol] = useState<ForwardProtocol>("TCP");
  const [port, setPort] = useState("22");
  const [typedName, setTypedName] = useState<string | undefined>(undefined);
  const apiTarget = forwardTargetOfPick(pick);
  const picked = targetOf(apiTarget);
  const t: ForwardTarget | undefined = picked && { ...picked, iface: apiTarget?.interface || "eth0" };
  const portBad = portProblem(port);
  const portN = portBad ? 0 : Number(port);
  const name = typedName ?? freeName(suggestName(t?.name ?? "", protocol, portN), new Set(forwards.items.map(nameOf)));
  const inbound = useInbound(project, t, protocol, portN);
  const shownInbound = t && !portBad ? inbound : undefined;

  const problems: string[] = [];
  const full = quota.limit !== undefined && quota.used >= quota.limit;
  if (!t) problems.push("No machine is chosen.");
  if (portBad) problems.push(portBad);
  const check = checkName(name, "port forward", { loading: forwards.loading, error: forwards.error, names: forwards.items.map(nameOf) });
  if (!check.ok) problems.push(check.reason);
  const obj = create(PortForwardSchema, { metadata: { name }, spec: { target: apiTarget, protocol: protocolEnum(protocol), targetPort: portN } });
  const send = () => {
    if (problems.length > 0 || full) return;
    make.mutate(obj, { onSuccess: () => void navigate({ to: "/projects/$project/port-forwards/$name", params: { project, name }, replace: true }) });
  };

  const quotaNotice = full && (
    <Notice tone="warning" title="The plan's limit is reached">
      {quota.used} of {quota.limit} port forwards are used.
    </Notice>
  );

  return (
    <FormPage
      crumbs={crumbs}
      title="New port forward"
      onSubmit={send}
      side={
        <>
          <FormSummary
            summary={
              <div className="space-y-1 text-[13px]">
                <div className="flex flex-wrap items-center gap-x-2 font-mono text-[12.5px]">
                  <span className="text-muted">shared IPv4 : random port</span>
                  <ArrowRight className="size-3.5 text-subtle" />
                  {t ? (
                    <span>
                      {t.name} · {t.iface} · {portN ? portLabel(portN, protocol) : "?"}
                    </span>
                  ) : (
                    <span className="font-sans text-subtle">no machine</span>
                  )}
                </div>
              </div>
            }
            notices={quotaNotice}
            problems={problems}
            blocked={full}
            error={make.error}
            failed="Creating the port forward failed"
            pending={make.isPending}
            pendingLabel="Creating…"
            label={forwards.loading ? "Loading names…" : "Create port forward"}
            cancelTo={`/projects/${project}/port-forwards`}
          />
          <FormManifest yaml={portForwardYaml(obj)} cli={[`hangar apply --create-only -p ${project} -f ${name || "port-forward"}.yaml`]} />
        </>
      }
    >
      <FormSection n={1} title="Target">
        <div className="max-w-xl">
          <InterfaceField value={pick} onChange={setPick} />
        </div>
      </FormSection>
      <FormSection n={2} title="Protocol and port" hint="What the machine listens on">
        <div className="max-w-xl">
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
      <FormSection n={3} title="Name">
        <div className="max-w-xl">
          <Input value={name} onChange={(e) => setTypedName(e.target.value.toLowerCase())} aria-label="Port forward name" spellCheck={false} />
        </div>
      </FormSection>
    </FormPage>
  );
}
