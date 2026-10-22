// A new custom domain of the tenant (docs/decisions/custom-domains.md): the
// domain, its object name suggested from it, and which of the tenant's
// projects may serve under it. Its page then lists the DNS records it
// needs. The one apply is create-only, so a name taken since, by any
// tenant, is refused rather than replaced.

import { create } from "@bufbuild/protobuf";
import { useQuery } from "@connectrpc/connect-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { CustomDomainSchema } from "@hangar/api/hangar/network/v1alpha1/custom_domain_pb";
import { TenantService } from "@hangar/api/hangar/tenancy/v1alpha1/tenant_pb";
import { Folders, LayoutGrid } from "lucide-react";
import { useState, type ReactNode } from "react";
import { Input } from "@/components/form";
import { Choice, CreateGate, FormManifest, FormPage, FormSection, FormSummary } from "@/components/form-page";
import { useTenantName } from "@/components/shell";
import { domainPerms, domainProblem, suggestName, useCreateDomain, useCustomDomains } from "@/lib/custom-domains";
import { customDomainYaml, nameOf } from "@/lib/network";
import { useCan } from "@/lib/permissions";
import { projectLabel, useProjects } from "@/lib/projects";
import { ProjectChecklist } from "@/pages/custom-domains";

const nameLabel = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

/** CreateCustomDomain is the New custom domain page. */
export function CreateCustomDomain() {
  const tenant = useTenantName();
  const perm = useCan({ level: "tenant", name: tenant }, [domainPerms.apply]);
  const crumbs = [
    <Link key="list" to="/tenants/$tenant/domains" params={{ tenant }} className="hover:text-foreground">
      Domains
    </Link>,
    "New custom domain",
  ];
  return (
    <CreateGate crumbs={crumbs} title="New custom domain" perm={perm} may={perm.can(domainPerms.apply)} refusal="creating custom domains" where="this tenant">
      <DomainForm crumbs={crumbs} tenant={tenant} />
    </CreateGate>
  );
}

function DomainForm({ crumbs, tenant }: { crumbs: ReactNode[]; tenant: string }) {
  const domains = useCustomDomains(tenant);
  const submit = useCreateDomain();
  const navigate = useNavigate();
  const { projects } = useProjects();
  const own = projects.filter((p) => p.spec?.tenant === tenant);
  const names = own.map((p) => p.metadata?.name ?? "");
  const label = (n: string) => projectLabel(own.find((p) => p.metadata?.name === n), n);
  const t = useQuery(TenantService.method.getTenant, { name: tenant }, { retry: false }).data?.tenant;
  const [domain, setDomain] = useState("");
  // The name follows the domain until it is edited.
  const [typedName, setTypedName] = useState<string | null>(null);
  const name = typedName ?? suggestName(domain);
  const [all, setAll] = useState(true);
  const [chosen, setChosen] = useState<string[]>([]);
  const spec = { tenant, domain, projects: all ? [] : names.filter((n) => chosen.includes(n)) };
  const obj = create(CustomDomainSchema, { metadata: { name }, spec });
  const limit = t?.status?.limits?.domains;
  const used = t?.status?.usage?.domains ?? 0;
  const problems = [
    ...(domains.error
      ? ["The tenant's custom domains did not load, so which it has is not known."]
      : domains.loading
        ? ["The tenant's custom domains are still loading."]
        : []),
    ...(domainProblem(domain) ? [domainProblem(domain)!] : []),
    ...(domains.items.some((d) => d.spec?.domain === domain) ? [`The tenant has ${domain} already.`] : []),
    ...(!name
      ? ["The name is empty."]
      : !nameLabel.test(name)
        ? ["The name is not a DNS label: lowercase letters, digits and “-”, at most 63."]
        : domains.items.some((d) => nameOf(d) === name)
          ? [`A custom domain named ${name} exists in this tenant.`]
          : []),
    ...(!all && spec.projects.length === 0 ? ["No project is chosen."] : []),
    ...(limit !== undefined && used >= limit ? [`The tenant holds ${used} of the ${limit} custom domains its plan allows.`] : []),
  ];
  return (
    <FormPage
      crumbs={crumbs}
      title="New custom domain"
      description={
        <>
          A domain of <span className="font-mono text-foreground">{tenant}</span>'s own, at whose hostnames the tenant's public endpoints may serve once its DNS
          proves it is the tenant's.
        </>
      }
      onSubmit={() => {
        if (problems.length > 0) return;
        submit.mutate(obj, { onSuccess: () => void navigate({ to: "/tenants/$tenant/domains/$name", params: { tenant, name }, replace: true }) });
      }}
      side={
        <>
          <FormSummary
            summary={
              <>
                <div className="truncate font-mono text-[13px] font-medium">{domain || <span className="font-sans font-normal text-muted">No domain</span>}</div>
                <div className="mt-0.5 text-[12px] text-muted">{all ? "All projects" : spec.projects.length ? spec.projects.map(label).join(", ") : "No project"}</div>
                <p className="mt-2 text-[12px] text-subtle">Its page lists the DNS records it needs; it is verified once the first of them is in place.</p>
              </>
            }
            problems={problems}
            error={submit.error}
            failed="Creating the custom domain failed"
            pending={submit.isPending}
            pendingLabel="Creating…"
            label="Create custom domain"
            cancelTo={`/tenants/${tenant}/domains`}
          />
          <FormManifest yaml={customDomainYaml(obj)} cli={[`hangar apply --create-only -f ${name || "domain"}.yaml`]} />
        </>
      }
    >
      <FormSection n={1} title="Domain">
        <div className="max-w-xl">
          <Input value={domain} onChange={(e) => setDomain(e.target.value.trim().toLowerCase())} placeholder="example.com" aria-label="Domain" spellCheck={false} />
          <p className="mt-1.5 text-[12px] text-subtle">
            Endpoints serve at the domain itself and one label below it, such as www.example.com. It cannot be changed once made.
          </p>
        </div>
      </FormSection>
      <FormSection n={2} title="Custom domain name" hint="Suggested from the domain">
        <div className="max-w-xl">
          <Input value={name} onChange={(e) => setTypedName(e.target.value.toLowerCase())} aria-label="Custom domain name" spellCheck={false} />
          <p className="mt-1.5 text-[12px] text-subtle">The object's name, a DNS label; names are unique on the platform, across every tenant.</p>
        </div>
      </FormSection>
      <FormSection n={3} title="Projects" hint="Whose endpoints may serve under it">
        <div role="radiogroup" aria-label="Projects" className="grid max-w-xl grid-cols-1 gap-3 sm:grid-cols-2">
          <Choice selected={all} onClick={() => setAll(true)} label="All projects">
            <LayoutGrid className="size-4 text-muted" strokeWidth={1.75} />
            <div className="mt-2 text-[13.5px] font-medium">All projects</div>
            <div className="mt-0.5 text-[12.5px] leading-snug text-muted">Every project of the tenant, and those made later.</div>
          </Choice>
          <Choice selected={!all} onClick={() => setAll(false)} label="Chosen projects">
            <Folders className="size-4 text-muted" strokeWidth={1.75} />
            <div className="mt-2 text-[13.5px] font-medium">Chosen projects</div>
            <div className="mt-0.5 text-[12.5px] leading-snug text-muted">Only those chosen here; others can be let later.</div>
          </Choice>
        </div>
        {!all && <ProjectChecklist projects={names} label={label} chosen={chosen} onChange={setChosen} className="mt-3 max-w-xl" />}
      </FormSection>
    </FormPage>
  );
}
