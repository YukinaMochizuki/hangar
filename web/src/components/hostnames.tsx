// An endpoint's custom hostnames as rows of a label and a custom domain,
// for New public endpoint and Edit hostnames alike
// (docs/decisions/custom-domains.md). An empty label is the domain itself;
// a hostname is the domain or one label below it.

import type { CustomDomain } from "@hangar/api/hangar/network/v1alpha1/custom_domain_pb";
import { Plus, X } from "lucide-react";
import { Input, Select } from "@/components/form";
import { ErrorNotice, Skeleton } from "@/components/states";
import { Button, Led } from "@/components/ui";
import { domainOf, hostnameOf, isVerified, type HostnameRow } from "@/lib/custom-domains";
import type { ListState } from "@/lib/network";

/** The rows, each with what it names under it, and Add hostname. */
export function HostnameRows({ rows, onChange, domains }: { rows: HostnameRow[]; onChange: (rows: HostnameRow[]) => void; domains: ListState<CustomDomain> }) {
  if (domains.loading) return <Skeleton className="h-9 w-full max-w-xl" />;
  if (domains.error) return <ErrorNotice error={domains.error} title="The project's custom domains did not load" onRetry={domains.refetch} className="max-w-xl px-0 py-0" />;
  if (domains.items.length === 0 && rows.length === 0) {
    return <p className="text-[12.5px] text-muted">No custom domain of the tenant lets this project use it.</p>;
  }
  const first = domains.items.find(isVerified) ?? domains.items[0];
  const set = (i: number, r: Partial<HostnameRow>) => onChange(rows.map((x, j) => (j === i ? { ...x, ...r } : x)));
  return (
    <div className="max-w-xl">
      {rows.length > 0 && (
        <ul className="space-y-3">
          {rows.map((r, i) => {
            const d = domains.items.find((x) => domainOf(x) === r.domain);
            return (
              <li key={i}>
                {/* On a phone the domain goes under the label, where its "(not verified)" fits. */}
                <div className="grid grid-cols-[minmax(0,1fr)_auto] items-center gap-1.5 sm:grid-cols-[minmax(0,1fr)_auto_minmax(0,1.3fr)_auto]">
                  <Input
                    className="order-1 sm:order-none"
                    value={r.label}
                    onChange={(e) => set(i, { label: e.target.value.toLowerCase() })}
                    placeholder="the domain itself"
                    aria-label={`Hostname ${i + 1} label`}
                    spellCheck={false}
                  />
                  <span className="hidden font-mono text-[13px] text-subtle sm:block">.</span>
                  <Select className="order-3 sm:order-none" value={r.domain} onChange={(e) => set(i, { domain: e.target.value })} aria-label={`Hostname ${i + 1} domain`}>
                    {domains.items.map((x) => (
                      <option key={domainOf(x)} value={domainOf(x)}>
                        {domainOf(x)}
                        {isVerified(x) ? "" : " (not verified)"}
                      </option>
                    ))}
                    {!d && <option value={r.domain}>{r.domain} (not a domain this project may use)</option>}
                  </Select>
                  <Button type="button" size="icon" variant="ghost" className="order-2 sm:order-none" aria-label={`Remove hostname ${i + 1}`} onClick={() => onChange(rows.filter((_, j) => j !== i))}>
                    <X />
                  </Button>
                </div>
                <div className="mt-1 text-[11.5px] text-subtle">
                  <span className="font-mono text-muted">{hostnameOf(r)}</span>
                  {!r.label && ", the domain itself"}
                </div>
                {d && !isVerified(d) && (
                  <div className="mt-0.5 flex items-center gap-1.5 text-[11.5px] text-muted">
                    <Led tone="pending" className="size-1.5" />
                    {r.domain} is not verified; {hostnameOf(r)} serves once it is.
                  </div>
                )}
              </li>
            );
          })}
        </ul>
      )}
      {first && (
        <Button type="button" size="sm" className={rows.length > 0 ? "mt-3" : undefined} onClick={() => onChange([...rows, { label: "", domain: domainOf(first) }])}>
          <Plus /> Add hostname
        </Button>
      )}
    </div>
  );
}
