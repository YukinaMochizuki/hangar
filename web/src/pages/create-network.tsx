// A new network (docs/decisions/networking.md). Nothing is sent until the
// project's networks are known and the name is not among them, and the one
// apply is create-only (lib/create.ts).

import { create } from "@bufbuild/protobuf";
import { Link, useNavigate } from "@tanstack/react-router";
import { NetworkSchema } from "@hangar/api/hangar/network/v1alpha1/network_pb";
import { useState, type ReactNode } from "react";
import { Input } from "@/components/form";
import { CreateGate, FormManifest, FormPage, FormSection, FormSummary } from "@/components/form-page";
import { useProjectName } from "@/components/shell";
import { checkName } from "@/lib/create";
import { permission, useCreate } from "@/lib/network-actions";
import { gatewayOf, nameOf, networkYaml, useNetworks } from "@/lib/network";
import { useCan } from "@/lib/permissions";

const applyPerm = permission("network", "apply");

/** CreateNetwork is the New network page. */
export function CreateNetwork() {
  const project = useProjectName();
  const perm = useCan({ level: "project", name: project }, [applyPerm]);
  const crumbs = [
    <Link key="list" to="/projects/$project/networks" params={{ project }} className="hover:text-foreground">
      Networks
    </Link>,
    "New network",
  ];
  return (
    <CreateGate crumbs={crumbs} title="New network" perm={perm} may={perm.can(applyPerm)} refusal="creating networks">
      <NetworkForm crumbs={crumbs} project={project} />
    </CreateGate>
  );
}

function NetworkForm({ crumbs, project }: { crumbs: ReactNode[]; project: string }) {
  const navigate = useNavigate();
  const nets = useNetworks(project);
  const submit = useCreate("network", project);
  const [name, setName] = useState("");
  const [cidr, setCidr] = useState("10.1.0.0/24");
  const obj = create(NetworkSchema, { metadata: { name }, spec: { ipv4Cidr: cidr } });
  const check = checkName(name, "network", { loading: nets.loading, error: nets.error, names: nets.items.map(nameOf) });
  const problems = [...(check.ok ? [] : [check.reason]), ...(cidr ? [] : ["The IPv4 range is empty."])];
  const send = () => {
    if (problems.length > 0) return;
    submit.mutate(obj, { onSuccess: () => void navigate({ to: "/projects/$project/networks/$name", params: { project, name }, replace: true }) });
  };
  const gateway = gatewayOf(cidr);
  return (
    <FormPage
      crumbs={crumbs}
      title="New network"
      onSubmit={send}
      side={
        <>
          <FormSummary
            summary={
              <>
                <div className="font-mono text-[15px] font-medium">{cidr || "–"}</div>
                {gateway && <div className="mt-0.5 text-[12px] text-muted">Gateway <span className="font-mono">{gateway}</span></div>}
              </>
            }
            problems={problems}
            error={submit.error}
            failed="Creating the network failed"
            pending={submit.isPending}
            pendingLabel="Creating…"
            label={nets.loading ? "Loading names…" : "Create network"}
            cancelTo={`/projects/${project}/networks`}
          />
          <FormManifest yaml={networkYaml(obj)} cli={[`hangar apply --create-only -p ${project} -f ${name || "network"}.yaml`]} />
        </>
      }
    >
      <FormSection n={1} title="IPv4 range">
        <div className="max-w-xl">
          <Input value={cidr} onChange={(e) => setCidr(e.target.value)} aria-label="IPv4 range" spellCheck={false} className="w-56" />
          <p className="mt-1.5 text-[12px] text-subtle">Private (RFC 1918), /16 to /28. Cannot be changed later.</p>
        </div>
      </FormSection>
      <FormSection n={2} title="Network name">
        <div className="max-w-xl">
          <Input value={name} onChange={(e) => setName(e.target.value.toLowerCase())} aria-label="Network name" spellCheck={false} />
        </div>
      </FormSection>
    </FormPage>
  );
}
