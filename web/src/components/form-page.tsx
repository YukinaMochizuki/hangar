// The frame of a page that creates or edits an object: its title, numbered
// sections on the left, and a column on the right that stays in view with
// the summary, why it cannot be sent yet, the API's error, the submit
// button and the manifest it amounts to. The new-machine page is laid out
// the same way (web/src/pages/create-machine.tsx).

import { Link } from "@tanstack/react-router";
import { Check } from "lucide-react";
import { useState, type FormEvent, type ReactNode } from "react";
import { CodeBlock } from "@/components/object";
import { Notice } from "@/components/notice";
import { Topbar } from "@/components/shell";
import { ErrorNotice } from "@/components/states";
import { Button, buttonVariants, Card, CopyButton } from "@/components/ui";
import { errorMessage } from "@/lib/api";
import { cn } from "@/lib/cn";

/** FormPage is a create or edit page: what it makes on the left, what it amounts to and its button on the right. */
export function FormPage({
  crumbs,
  title,
  description,
  side,
  onSubmit,
  children,
}: {
  /** The top bar's crumbs; none when the page sits in a frame that draws the top bar, such as DetailFrame. */
  crumbs?: ReactNode[];
  title: string;
  description?: ReactNode;
  /** The right column: a FormSummary, then a FormManifest. */
  side: ReactNode;
  onSubmit: () => void;
  children: ReactNode;
}) {
  const submit = (e: FormEvent) => {
    e.preventDefault();
    onSubmit();
  };
  return (
    <>
      {crumbs && <Topbar crumbs={crumbs} />}
      <div className="px-4 pb-2 pt-6 md:px-8">
        <h1 className="text-[22px] font-semibold tracking-tight">{title}</h1>
        {description && <p className="mt-1 max-w-2xl text-[13px] text-muted">{description}</p>}
      </div>
      {/* Its fields are never this site's own login. */}
      <form onSubmit={submit} autoComplete="off" className="grid grid-cols-1 gap-x-8 px-4 pb-10 md:px-8 xl:grid-cols-[minmax(0,1fr)_360px]">
        <div className="min-w-0">{children}</div>
        <aside className="xl:pt-6">
          <div className="space-y-4 xl:sticky xl:top-20">{side}</div>
        </aside>
      </form>
    </>
  );
}

/**
 * CreateGate stands in for a create page until the caller is known to hold
 * the permission it needs: nothing while the answer is unknown, the refusal
 * once the answer says the caller lacks it, and the error when the answer
 * failed. A form once shown stays, whatever a later answer says, so what
 * was typed, or a secret shown once, is never dropped under the person.
 */
export function CreateGate({
  crumbs,
  title,
  perm,
  may,
  refusal,
  children,
}: {
  crumbs: ReactNode[];
  title: string;
  /** What useCan said. */
  perm: { known: boolean; error: unknown };
  may: boolean;
  /** What the caller's roles do not include, such as "creating disks". */
  refusal: string;
  children: ReactNode;
}) {
  const [shown, setShown] = useState(may);
  if (may && !shown) setShown(true);
  if (may || shown) return children;
  return (
    <>
      <Topbar crumbs={crumbs} />
      {perm.known ? (
        <div className="px-4 pb-10 pt-6 md:px-8">
          <h1 className="text-[22px] font-semibold tracking-tight">{title}</h1>
          <p className="mt-1 max-w-2xl text-[13px] text-muted">This account's roles do not include {refusal} in this project.</p>
        </div>
      ) : perm.error ? (
        <ErrorNotice error={perm.error} title="Whether this account may do this did not load" className="px-4 py-6 md:px-8" />
      ) : null}
    </>
  );
}

/** FormSection is one numbered step of a FormPage. */
export function FormSection({ n, title, hint, children }: { n: number; title: string; hint?: ReactNode; children: ReactNode }) {
  return (
    <section className="border-b py-6 last:border-0">
      <div className="mb-3 flex flex-wrap items-baseline gap-x-3 gap-y-1">
        <span className="font-mono text-[12px] text-subtle">{String(n).padStart(2, "0")}</span>
        <h2 className="text-[15px] font-medium">{title}</h2>
        {hint && <span className="text-[12.5px] text-muted">{hint}</span>}
      </div>
      <div className="md:pl-8">{children}</div>
    </section>
  );
}

/** Choice is one card of a radio group, such as a size or a disk's type. */
export function Choice({
  selected,
  onClick,
  disabled,
  label,
  className,
  children,
}: {
  selected: boolean;
  onClick: () => void;
  disabled?: boolean;
  label?: string;
  className?: string;
  children: ReactNode;
}) {
  return (
    <button
      type="button"
      role="radio"
      aria-checked={selected}
      aria-label={label}
      disabled={disabled}
      onClick={onClick}
      className={cn(
        "relative flex flex-col items-stretch justify-start rounded-lg border bg-surface p-4 text-left transition-colors disabled:cursor-default disabled:opacity-50",
        selected ? "border-accent bg-accent-soft ring-1 ring-accent" : "hover:border-border-strong",
        className,
      )}
    >
      {selected && (
        <span className="absolute right-2.5 top-2.5 flex size-4 items-center justify-center rounded-full bg-accent text-accent-fg">
          <Check className="size-2.5" strokeWidth={3} />
        </span>
      )}
      {children}
    </button>
  );
}

/**
 * FormSummary is the right column's card: what the form would make, the
 * facts that keep it from being sent, the API's error, and the submit
 * button, named for what it does.
 */
export function FormSummary({
  summary,
  problems,
  notices,
  error,
  failed,
  pending,
  pendingLabel,
  label,
  blocked,
  cancelTo,
}: {
  summary: ReactNode;
  /** Why it cannot be sent yet, each a fact; the button is off while there is one. */
  problems: string[];
  /** Notices that stand above the button, such as a list that did not load. */
  notices?: ReactNode;
  error: unknown;
  /** The title of the API's error, such as "Creating the disk failed". */
  failed: string;
  pending: boolean;
  pendingLabel: string;
  label: ReactNode;
  /** Off for a reason a notice states. */
  blocked?: boolean;
  /** Where Cancel goes, such as the list's path; no Cancel when unset. */
  cancelTo?: string;
}) {
  return (
    <Card>
      <div className="border-b px-4 py-3">
        <div className="text-[11.5px] text-subtle">Summary</div>
        <div className="mt-1">{summary}</div>
      </div>
      <div className="space-y-3 px-4 py-3">
        {notices}
        {error != null && (
          <Notice tone="error" title={failed}>
            {errorMessage(error)}
          </Notice>
        )}
        {problems.length > 0 && <div className="text-[12px] text-subtle">{problems.join(" ")}</div>}
        <div className="flex gap-2">
          {/* Off while the call is in flight: leaving then would drop what it answers, such as a token's only secret. */}
          {cancelTo &&
            (pending ? (
              <Button type="button" disabled>
                Cancel
              </Button>
            ) : (
              <Link to={cancelTo} className={buttonVariants()}>
                Cancel
              </Link>
            ))}
          <Button type="submit" variant="primary" className="min-w-0 flex-1" disabled={problems.length > 0 || !!blocked || pending}>
            {pending ? pendingLabel : label}
          </Button>
        </div>
      </div>
    </Card>
  );
}

/** FormManifest is the manifest a form applies and the command that applies it, each copyable. */
export function FormManifest({ yaml, cli }: { yaml?: string; cli: string[] }) {
  return (
    <Card className="overflow-hidden">
      <div className="flex h-10 items-center justify-between border-b px-4">
        <span className="text-[12px] font-medium">{yaml ? "Equivalent manifest" : "CLI"}</span>
        <CopyButton text={yaml ?? cli.join("\n")} />
      </div>
      {yaml && <pre className="max-h-96 overflow-auto px-4 py-3 font-mono text-[11.5px] leading-5 text-muted">{yaml}</pre>}
      <div className={cn("flex items-start justify-between gap-2", yaml && "border-t")}>
        <CodeBlock cli={cli} className="min-w-0 flex-1 py-2.5 text-[11.5px]" />
        {yaml && <CopyButton text={cli.join("\n")} className="mr-2 mt-2 shrink-0" />}
      </div>
    </Card>
  );
}
