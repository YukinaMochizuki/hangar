// The pages before the console: signed out, signed in without an
// invitation, and signing up. Gate shows the one that applies, or the
// console once the person is a member.

import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { AccountService } from "@hangar/api/hangar/iam/v1alpha1/account_pb";
import { NameService } from "@hangar/api/hangar/tenancy/v1alpha1/names_pb";
import { Check, LoaderCircle, X } from "lucide-react";
import { useEffect, useState, type ReactNode } from "react";
import { Input } from "@/components/form";
import { HangarMark } from "@/components/marks";
import { Notice } from "@/components/notice";
import { StandalonePage } from "@/components/states";
import { Button, buttonVariants } from "@/components/ui";
import { errorMessage, signInURL, signOut } from "@/lib/api";
import { cn } from "@/lib/cn";
import { MemberProvider, useSessionState } from "@/lib/session";

export function Gate({ children }: { children: ReactNode }) {
  const { session, refetch } = useSessionState();
  switch (session.state) {
    case "loading":
      return <LoadingPage />;
    case "signedOut":
      return <SignInPage />;
    case "error":
      return <GateErrorPage error={session.error} onRetry={refetch} />;
    case "notInvited":
      return <WaitingPage email={session.who.id} type={session.who.type} onCheck={refetch} />;
    case "noUser":
      return <SignupPage email={session.who.id} />;
    case "member":
      return <MemberProvider value={session.who}>{children}</MemberProvider>;
  }
}

function LoadingPage() {
  return (
    <div aria-busy="true" className="flex min-h-screen items-center justify-center bg-background">
      <HangarMark className="size-8 animate-pulse text-subtle motion-reduce:animate-none" />
    </div>
  );
}

export function SignInPage() {
  return (
    <StandalonePage title="Not signed in" footer="Hangar is open to invited people only.">
      <p>
        The console signs in with a Google account. A session ends after 3 days unused, and 14 days after signing in in any
        case.
      </p>
      <a href={signInURL()} className={cn(buttonVariants({ variant: "primary" }), "mt-5 w-full")}>
        Sign in with Google
      </a>
    </StandalonePage>
  );
}

function GateErrorPage({ error, onRetry }: { error: unknown; onRetry: () => void }) {
  return (
    <StandalonePage title="The API did not answer">
      <p className="font-mono text-[12px]">{errorMessage(error)}</p>
      <Button className="mt-5 w-full" onClick={onRetry}>
        Retry
      </Button>
    </StandalonePage>
  );
}

function SignOutButton({ className }: { className?: string }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  return (
    <>
      {error !== null && <Notice tone="error" title={errorMessage(error)} className="mt-4" />}
      <Button
        className={className}
        disabled={busy}
        onClick={async () => {
          setBusy(true);
          try {
            await signOut();
            window.location.assign("/");
          } catch (e) {
            setError(e);
            setBusy(false);
          }
        }}
      >
        Sign out
      </Button>
    </>
  );
}

// How often the waiting page asks again whether an invitation has come.
const waitingRecheck = 30_000;

export function WaitingPage({ email, type, onCheck }: { email: string; type: string; onCheck: () => void }) {
  useEffect(() => {
    const t = setInterval(onCheck, waitingRecheck);
    return () => clearInterval(t);
  }, [onCheck]);
  return (
    <StandalonePage title="No invitation yet" footer="This page checks again every 30 seconds.">
      <p>
        <span className="font-mono text-[12.5px] text-foreground">{email}</span> is signed in
        {type !== "user" && <> as a {type}</>}, and holds no invitation to Hangar. Administrators invite people by email; the
        console opens here once an invitation exists.
      </p>
      <SignOutButton className="mt-5 w-full" />
    </StandalonePage>
  );
}

/** A username suggested from an email: its local part, as a DNS label. */
function suggestUsername(email: string): string {
  return email
    .split("@")[0]
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 63);
}

const dnsLabel = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

/** value, once it has stopped changing for ms. */
function useDebounced<T>(value: T, ms: number): T {
  const [v, setV] = useState(value);
  useEffect(() => {
    const t = setTimeout(() => setV(value), ms);
    return () => clearTimeout(t);
  }, [value, ms]);
  return v;
}

export function SignupPage({ email }: { email: string }) {
  const [username, setUsername] = useState(() => suggestUsername(email));
  const [displayName, setDisplayName] = useState("");
  const queryClient = useQueryClient();
  const checked = useDebounced(username, 300);
  const valid = dnsLabel.test(username);
  const check = useQuery(NameService.method.checkName, { kind: "User", name: checked }, { enabled: dnsLabel.test(checked) });
  const signUp = useMutation(AccountService.method.signUp, {
    onSuccess: () => queryClient.invalidateQueries(),
  });
  const current = checked === username && check.data !== undefined && !check.isFetching;
  const available = current && check.data?.available === true;

  let hint: ReactNode = null;
  if (username && !valid) {
    hint = <span className="text-fail">Lowercase letters, digits and “-”, at most 63, starting and ending with a letter or digit.</span>;
  } else if (valid && !current) {
    hint = (
      <span className="inline-flex items-center gap-1 text-subtle">
        <LoaderCircle className="size-3 animate-spin motion-reduce:animate-none" /> Checking
      </span>
    );
  } else if (check.error) {
    hint = <span className="text-fail">{errorMessage(check.error)}</span>;
  } else if (current) {
    hint = available ? (
      <span className="inline-flex items-center gap-1 text-ok">
        <Check className="size-3" /> Available
      </span>
    ) : (
      <span className="inline-flex items-center gap-1 text-fail">
        <X className="size-3" /> Taken or reserved
      </span>
    );
  }

  return (
    <StandalonePage title="Hangar account">
      <p>
        <span className="font-mono text-[12.5px] text-foreground">{email}</span> is invited and has no account yet. A
        username is global and permanent: it never changes, and no one else can ever have it.
      </p>
      <form
        className="mt-5 space-y-4"
        onSubmit={(e) => {
          e.preventDefault();
          if (available) signUp.mutate({ username, displayName: displayName.trim() });
        }}
      >
        <label className="block">
          <span className="text-[12.5px] font-medium text-foreground">Username</span>
          <Input
            value={username}
            onChange={(e) => setUsername(e.target.value.toLowerCase())}
            autoComplete="username"
            data-1p-ignore={undefined}
            spellCheck={false}
            className="mt-1.5"
          />
          <span className="mt-1 block min-h-4 text-[12px]">{hint}</span>
        </label>
        <label className="block">
          <span className="text-[12.5px] font-medium text-foreground">Display name</span>
          <span className="ml-1.5 text-[12px] text-subtle">optional, can change later</span>
          <Input
            value={displayName}
            onChange={(e) => setDisplayName(e.target.value)}
            autoComplete="name"
            data-1p-ignore={undefined}
            className="mt-1.5 font-sans"
          />
        </label>
        {signUp.error && <Notice tone="error" title="Signing up failed">{errorMessage(signUp.error)}</Notice>}
        <Button type="submit" variant="primary" className="w-full" disabled={!available || signUp.isPending}>
          {signUp.isPending ? "Creating account…" : "Create account"}
        </Button>
      </form>
      <SignOutButton className="mt-3 w-full" />
    </StandalonePage>
  );
}
