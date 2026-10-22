// A new public endpoint: one TCP port of a machine, served over HTTPS at a
// hostname of its own (docs/decisions/public-endpoints.md), and a public one
// also at custom hostnames under the tenant's custom domains
// (docs/decisions/custom-domains.md). Its own hostname is not
// guessed: the endpoint's page shows the URL its status gives. Nothing is
// sent until the project's endpoints are known and the name is not among
// them, and the one apply is create-only (lib/create.ts).

import { create } from "@bufbuild/protobuf";
import { Link, useNavigate } from "@tanstack/react-router";
import { PublicEndpointAccess, PublicEndpointSchema } from "@hangar/api/hangar/network/v1alpha1/public_endpoint_pb";
import { Globe, Lock } from "lucide-react";
import { useState, type ReactNode } from "react";
import { FormField, Input, Select } from "@/components/form";
import { Choice, CreateGate, FormManifest, FormPage, FormSection, FormSummary } from "@/components/form-page";
import { HostnameRows } from "@/components/hostnames";
import { useProjectName } from "@/components/shell";
import { checkName } from "@/lib/create";
import { hostnameOf, rowProblems, useProjectDomains, type HostnameRow } from "@/lib/custom-domains";
import { useMachines, type MachineKind } from "@/lib/machines";
import { accessLabel, endpointYaml, kindNoun, nameOf, usePublicEndpoints } from "@/lib/network";
import { permission, useCreate } from "@/lib/network-actions";
import { useCan } from "@/lib/permissions";

const applyPerm = permission("publicEndpoint", "apply");

const accesses = [
  { id: PublicEndpointAccess.PRIVATE, icon: Lock, label: "Private", text: "Sign-in required." },
  { id: PublicEndpointAccess.PUBLIC, icon: Globe, label: "Public", text: "Anyone, no sign-in." },
];

/** CreatePublicEndpoint is the New public endpoint page. */
export function CreatePublicEndpoint() {
  const project = useProjectName();
  const perm = useCan({ level: "project", name: project }, [applyPerm]);
  const crumbs = [
    <Link key="list" to="/projects/$project/public-endpoints" params={{ project }} className="hover:text-foreground">
      Public endpoints
    </Link>,
    "New public endpoint",
  ];
  return (
    <CreateGate crumbs={crumbs} title="New public endpoint" perm={perm} may={perm.can(applyPerm)} refusal="creating public endpoints">
      <EndpointForm crumbs={crumbs} project={project} />
    </CreateGate>
  );
}

function EndpointForm({ crumbs, project }: { crumbs: ReactNode[]; project: string }) {
  const navigate = useNavigate();
  const eps = usePublicEndpoints(project);
  const all = useMachines(project);
  const submit = useCreate("publicEndpoint", project);
  const [name, setName] = useState("");
  const [choice, setChoice] = useState("");
  const [port, setPort] = useState("8080");
  const [access, setAccess] = useState(PublicEndpointAccess.PRIVATE);
  const domains = useProjectDomains(project);
  const [rows, setRows] = useState<HostnameRow[]>([]);
  const isPublic = access === PublicEndpointAccess.PUBLIC;
  // A private endpoint takes no custom hostname; the rows are kept for when it is public again.
  const hostnames = isPublic ? rows.map(hostnameOf) : [];
  const first = all.items[0];
  const target = choice || (first ? `${first.kind}/${first.name}` : "");
  const [kind, machine] = target.split("/") as [MachineKind, string];
  const obj = create(PublicEndpointSchema, {
    metadata: { name },
    spec: {
      target: machine ? { object: { case: kind === "Container" ? "container" : "virtualMachine", value: machine } } : undefined,
      port: Number(port) || 0,
      access,
      hostnames,
    },
  });
  const check = checkName(name, "public endpoint", { loading: eps.loading, error: eps.error, names: eps.items.map(nameOf) });
  const problems = [
    ...(check.ok ? [] : [check.reason]),
    ...(machine ? [] : ["No machine is chosen."]),
    ...(Number(port) >= 1 && Number(port) <= 65535 ? [] : ["The port is not between 1 and 65535."]),
    ...(isPublic ? rowProblems(rows) : []),
  ];
  const send = () => {
    if (problems.length > 0) return;
    submit.mutate(obj, { onSuccess: () => void navigate({ to: "/projects/$project/public-endpoints/$name", params: { project, name }, replace: true }) });
  };
  return (
    <FormPage
      crumbs={crumbs}
      title="New public endpoint"
      onSubmit={send}
      side={
        <>
          <FormSummary
            summary={
              <>
                <div className="text-[13px]">
                  {machine ? <span className="font-mono font-medium">{machine}</span> : <span className="text-muted">No machine</span>}
                  <span className="font-mono text-muted">:{port || "–"}</span>
                  {machine && <span className="ml-1.5 text-[12px] text-subtle">{kindNoun(kind)}</span>}
                </div>
                <div className="mt-0.5 text-[12px] text-muted">{accessLabel(access)}</div>
                {hostnames.length > 0 && (
                  <ul className="mt-1.5 space-y-0.5 font-mono text-[12px] text-muted">
                    {hostnames.map((h, i) => (
                      <li key={i} className="truncate">
                        {h}
                      </li>
                    ))}
                  </ul>
                )}
              </>
            }
            problems={problems}
            error={submit.error}
            failed="Creating the public endpoint failed"
            pending={submit.isPending}
            pendingLabel="Creating…"
            label={eps.loading ? "Loading names…" : "Create public endpoint"}
            cancelTo={`/projects/${project}/public-endpoints`}
          />
          <FormManifest yaml={endpointYaml(obj)} cli={[`hangar apply --create-only -p ${project} -f ${name || "endpoint"}.yaml`]} />
        </>
      }
    >
      <FormSection n={1} title="Target">
        <div className="grid max-w-xl grid-cols-[1fr_96px] gap-2">
          <FormField label="Machine" className="mt-0">
            <Select value={target} onChange={(ev) => setChoice(ev.target.value)}>
              {all.items.map((m) => (
                <option key={`${m.kind}/${m.name}`} value={`${m.kind}/${m.name}`}>
                  {m.name} ({kindNoun(m.kind)})
                </option>
              ))}
            </Select>
          </FormField>
          <FormField label="Port" className="mt-0">
            <Input value={port} onChange={(ev) => setPort(ev.target.value.replace(/\D/g, ""))} inputMode="numeric" />
          </FormField>
          <span className="col-span-2 text-[11.5px] text-subtle">Plain HTTP, on eth0.</span>
        </div>
      </FormSection>
      <FormSection n={2} title="Access">
        <div role="radiogroup" aria-label="Access" className="grid max-w-xl grid-cols-1 gap-3 sm:grid-cols-2">
          {accesses.map((o) => (
            <Choice key={o.id} selected={access === o.id} onClick={() => setAccess(o.id)} label={o.label}>
              <o.icon className="size-4 text-muted" strokeWidth={1.75} />
              <div className="mt-2 text-[13.5px] font-medium">{o.label}</div>
              <div className="mt-0.5 text-[12.5px] leading-snug text-muted">{o.text}</div>
            </Choice>
          ))}
        </div>
      </FormSection>
      <FormSection n={3} title="Endpoint name">
        <div className="max-w-xl">
          <Input value={name} onChange={(ev) => setName(ev.target.value.toLowerCase())} aria-label="Endpoint name" spellCheck={false} />
          <p className="mt-1.5 text-[12px] text-subtle">Sets the hostname, which is claimed for good.</p>
        </div>
      </FormSection>
      <FormSection n={4} title="Custom hostnames" hint="Optional">
        {isPublic ? (
          <>
            <p className="mb-3 max-w-xl text-[12.5px] text-muted">Each a custom domain of the tenant or one label below one.</p>
            <HostnameRows rows={rows} onChange={setRows} domains={domains} />
          </>
        ) : (
          <p className="max-w-xl text-[12.5px] text-muted">Public endpoints only.</p>
        )}
      </FormSection>
    </FormPage>
  );
}
