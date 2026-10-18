// Reserving a public IPv6 address, bound to an interface or not
// (docs/decisions/networking.md). Nothing is sent until the project's
// addresses are known and the name is not among them, and the one apply is
// create-only (lib/create.ts).

import { create } from "@bufbuild/protobuf";
import { Link, useNavigate } from "@tanstack/react-router";
import { AddressSchema } from "@hangar/api/hangar/network/v1alpha1/address_pb";
import { useState, type ReactNode } from "react";
import { Input } from "@/components/form";
import { CreateGate, FormManifest, FormPage, FormSection, FormSummary } from "@/components/form-page";
import { useProjectName } from "@/components/shell";
import { checkName } from "@/lib/create";
import { kindNoun, nameOf, addressYaml, targetOf, useAddresses } from "@/lib/network";
import { permission, useCreate } from "@/lib/network-actions";
import { useCan } from "@/lib/permissions";
import { InterfaceField, pickOf, pickProblem, pickTarget } from "@/pages/addresses";

const applyPerm = permission("address", "apply");

/** CreateAddress is the Reserve address page. */
export function CreateAddress() {
  const project = useProjectName();
  const perm = useCan({ level: "project", name: project }, [applyPerm]);
  const crumbs = [
    <Link key="list" to="/projects/$project/addresses" params={{ project }} className="hover:text-foreground">
      Addresses
    </Link>,
    "Reserve address",
  ];
  return (
    <CreateGate crumbs={crumbs} title="Reserve address" perm={perm} may={perm.can(applyPerm)} refusal="reserving addresses">
      <AddressForm crumbs={crumbs} project={project} />
    </CreateGate>
  );
}

function AddressForm({ crumbs, project }: { crumbs: ReactNode[]; project: string }) {
  const navigate = useNavigate();
  const addrs = useAddresses(project);
  const reserve = useCreate("address", project);
  const [name, setName] = useState("");
  const [pick, setPick] = useState(pickOf(""));
  const target = pickTarget(pick);
  const obj = create(AddressSchema, { metadata: { name }, spec: { target } });
  const check = checkName(name, "address", { loading: addrs.loading, error: addrs.error, names: addrs.items.map(nameOf) });
  const problems: string[] = [];
  if (!check.ok) problems.push(check.reason);
  const typedProblem = pickProblem(pick);
  if (typedProblem) problems.push(typedProblem);
  const send = () => {
    if (problems.length > 0) return;
    reserve.mutate(obj, { onSuccess: () => void navigate({ to: "/projects/$project/addresses/$name", params: { project, name }, replace: true }) });
  };
  const t = targetOf(target);
  return (
    <FormPage
      crumbs={crumbs}
      title="Reserve address"
      description={
        <>
          A public IPv6 address of <span className="font-mono text-foreground">{project}</span>, kept until deleted. Bound to an interface, it replaces
          the interface's automatic one; moving it restarts no machine.
        </>
      }
      onSubmit={send}
      side={
        <>
          <FormSummary
            summary={
              t ? (
                <div className="text-[13px]">
                  Bound to <span className="font-mono font-medium">{t.name}</span>
                  <span className="font-mono text-muted"> · {target?.interface}</span>
                  <span className="ml-1.5 text-[12px] text-subtle">{kindNoun(t.kind)}</span>
                </div>
              ) : (
                <div className="text-[13px]">Reserved, unbound</div>
              )
            }
            problems={problems}
            error={reserve.error}
            failed="Reserving the address failed"
            pending={reserve.isPending}
            pendingLabel="Reserving…"
            label={addrs.loading ? "Loading names…" : "Reserve address"}
            cancelTo={`/projects/${project}/addresses`}
          />
          <FormManifest yaml={addressYaml(obj)} cli={[`hangar apply --create-only -p ${project} -f ${name || "address"}.yaml`]} />
        </>
      }
    >
      <FormSection n={1} title="Interface" hint="Optional">
        <div className="max-w-xl">
          <InterfaceField
            pick={pick}
            onChange={setPick}
            none="No interface: reserved, unbound"
            hint="Bound, the address replaces the interface's automatic one. No machine restarts."
          />
        </div>
      </FormSection>
      <FormSection n={2} title="Address name" hint="Unique among this project's addresses">
        <div className="max-w-xl">
          <Input value={name} onChange={(e) => setName(e.target.value.toLowerCase())} aria-label="Address name" spellCheck={false} />
        </div>
      </FormSection>
    </FormPage>
  );
}
