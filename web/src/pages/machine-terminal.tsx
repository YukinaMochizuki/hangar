// A container's terminal: a page of its own, and a card on the container's
// page that stays folded to its header until Connect. Nothing connects on
// its own but the page, which is opened to connect. Each is
// `hangar exec -it` in the browser (web/src/components/terminal.tsx).

import { MachinePhase } from "@hangar/api/hangar/compute/v1alpha1/machine_pb";
import { Link } from "@tanstack/react-router";
import { Maximize2, Plug, RotateCw, TerminalSquare, Unplug, X } from "lucide-react";
import { useEffect, useState, type ReactNode } from "react";
import { Topbar, useProjectName } from "@/components/shell";
import { StaleBanner } from "@/components/credentials";
import { EmptyState, ErrorNotice } from "@/components/states";
import { useTerminal, type TerminalState } from "@/components/terminal";
import { Button, buttonVariants, Card, CopyButton, StatusDot } from "@/components/ui";
import { cn } from "@/lib/cn";
import { useMachines, type MachineView } from "@/lib/machines";
import { useCan } from "@/lib/permissions";

/** The permission an exec needs, at the object's project. */
export const execPermission = "compute.machines.exec";

// The program a terminal runs: a shell every image is taken to have.
const shell = ["sh"];

/** Whether the caller may open a terminal in the project's machines; false while unknown or failed. */
export function useMayExec(project: string): boolean {
  return useCan({ level: "project", name: project }, [execPermission]).can(execPermission);
}

/** Why a container's terminal cannot connect now, or undefined when it can. */
export function terminalBlock(m: MachineView): string | undefined {
  if (m.deleting) return `${m.name} is being deleted.`;
  if (m.phase !== MachinePhase.RUNNING) return `${m.name} is not running.`;
  return undefined;
}

function execCli(m: { name: string; project: string }) {
  return `hangar exec -it container ${m.name} -p ${m.project} -- sh`;
}

const terminalBg = "bg-[#18181b]";

function StateText({ state }: { state: TerminalState }) {
  switch (state.kind) {
    case "idle":
      return <span className="text-muted">Not connected</span>;
    case "connecting":
      return (
        <span className="flex items-center gap-2">
          <span className="size-2 animate-pulse rounded-full bg-pending motion-reduce:animate-none" />
          Connecting…
        </span>
      );
    case "open":
      return (
        <span className="flex items-center gap-2">
          <span className="size-2 rounded-full bg-ok" />
          Connected
          <span className="font-mono text-[12px] text-muted">
            sh · {state.cols}×{state.rows}
          </span>
        </span>
      );
    case "ended":
      return (
        <span className="flex items-center gap-2">
          <span className={cn("size-2 rounded-full", state.end.kind === "exited" || state.end.kind === "closed" ? "bg-off" : "bg-fail")} />
          {state.end.kind === "exited" ? "Exited" : state.end.kind === "closed" ? "Disconnected" : state.end.kind === "failed" ? "Failed" : "Not connected"}
        </span>
      );
    case "unsupported":
      return (
        <span className="flex items-center gap-2">
          <span className="size-2 rounded-full bg-fail" />
          No terminal in this browser
        </span>
      );
  }
}

/**
 * What follows a terminal that did not connect, or cannot draw: the CLI
 * shows the API's reason, which a browser is not told, and needs no WebGL.
 */
function EndNote({ state, cli, className }: { state: TerminalState; cli: string; className?: string }) {
  if (state.kind === "unsupported") {
    return (
      <p className={cn("border-t px-4 py-2.5 text-[12.5px] text-muted", className)}>
        The terminal draws with WebGL, which this browser does not give it. <span className="font-mono">{cli}</span> opens the same shell.
      </p>
    );
  }
  if (state.kind !== "ended" || state.end.kind !== "unreachable") return null;
  return (
    <p className={cn("border-t px-4 py-2.5 text-[12.5px] text-muted", className)}>
      The API refused the connection or could not be reached; a browser is not told why. <span className="font-mono">{cli}</span> says.
    </p>
  );
}

function Actions({ state, onConnect, onDisconnect, extra }: { state: TerminalState; onConnect: () => void; onDisconnect: () => void; extra?: ReactNode }) {
  const live = state.kind === "open" || state.kind === "connecting";
  return (
    <span className="flex flex-wrap items-center gap-2">
      {live ? (
        <Button size="sm" variant="danger" onClick={onDisconnect}>
          <Unplug /> Disconnect
        </Button>
      ) : state.kind === "idle" ? (
        <Button size="sm" variant="primary" onClick={onConnect}>
          <Plug /> Connect
        </Button>
      ) : (
        <Button size="sm" onClick={onConnect}>
          <RotateCw /> Reconnect
        </Button>
      )}
      {extra}
    </span>
  );
}

/** A Terminal card on a container's page: its header alone until Connect, then the terminal under it. */
export function TerminalCard({ m }: { m: MachineView }) {
  const { attach, state, connect, disconnect, close: end } = useTerminal({ project: m.project, plural: "containers", name: m.name, command: shell });
  const [shown, setShown] = useState(false);
  const block = terminalBlock(m);
  useEffect(() => {
    if (shown) void connect();
    // Connect once the terminal's element is drawn and sized.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [shown]);
  const close = () => {
    end();
    setShown(false);
  };
  return (
    <Card className="overflow-hidden">
      <div className="flex min-h-11 flex-wrap items-center justify-between gap-2 px-4 py-2">
        <span className="flex items-center gap-3 text-[13px]">
          <span className="flex items-center gap-2 font-medium">
            <TerminalSquare className="size-4 text-subtle" /> Terminal
          </span>
          {shown ? <StateText state={state} /> : block && <span className="text-muted">{block}</span>}
        </span>
        {shown ? (
          <Actions
            state={state}
            onConnect={() => void connect()}
            onDisconnect={disconnect}
            extra={
              <>
                <Link to="/projects/$project/containers/$name/terminal" params={{ project: m.project, name: m.name }} className={buttonVariants({ size: "sm" })}>
                  <Maximize2 /> Full page
                </Link>
                <Button size="sm" variant="ghost" aria-label="Close the terminal" onClick={close}>
                  <X />
                </Button>
              </>
            }
          />
        ) : (
          <Button size="sm" variant="primary" disabled={!!block} onClick={() => setShown(true)}>
            <Plug /> Connect
          </Button>
        )}
      </div>
      {shown && (
        <>
          {state.kind !== "unsupported" && (
            <div className={cn("h-80 overflow-hidden border-t p-2", terminalBg)}>
              <div ref={attach} className="h-full w-full" />
            </div>
          )}
          <EndNote state={state} cli={execCli(m)} />
        </>
      )}
    </Card>
  );
}

function TerminalBody({ m }: { m: MachineView }) {
  const { attach, state, connect, disconnect } = useTerminal({ project: m.project, plural: "containers", name: m.name, command: shell });
  const block = terminalBlock(m);
  useEffect(() => {
    if (!block) void connect();
    // The page connects once, on opening; Reconnect starts again.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  const cli = execCli(m);
  // A container that cannot take an exec yet has no terminal drawn; Connect comes once it can.
  const waiting = !!block && state.kind === "idle";
  const drawn = !waiting && state.kind !== "unsupported";
  return (
    <>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2 border-b px-4 py-2.5 text-[13px] md:px-8">
        {waiting ? <span className="text-muted">{block}</span> : <StateText state={state} />}
        <span className="flex-1" />
        <code className="hidden items-center gap-1 rounded-md border bg-surface-2 py-1 pl-2 pr-1 font-mono text-[11.5px] text-muted lg:flex">
          <span className="select-none text-accent">$</span> {cli}
          <CopyButton text={cli} />
        </code>
        {!waiting && <Actions state={state} onConnect={() => void connect()} onDisconnect={disconnect} />}
      </div>
      <EndNote state={state} cli={cli} className="border-t-0 md:px-8" />
      {drawn && (
        // Inset as the console's other cards are, with their margins and rounded border.
        <div className="flex min-h-0 min-w-0 flex-1 flex-col px-4 py-4 md:px-8 md:pb-6">
          <div className={cn("min-h-0 flex-1 overflow-hidden rounded-lg border p-2", terminalBg)}>
            <div ref={attach} className="h-full w-full" />
          </div>
        </div>
      )}
    </>
  );
}

/** A container's terminal as a page of its own, filling the window under the top bar. */
export function TerminalPage({ name }: { name: string }) {
  const project = useProjectName();
  const { held, loading, error, refetch } = useMachines(project, "Container");
  const perm = useCan({ level: "project", name: project }, [execPermission]);
  // Asking again failing keeps the terminal on the last answer: unmounting it would end the process.
  const m = held.find((x) => x.name === name);
  const crumbs = [
    "Containers",
    <Link key="name" to="/projects/$project/containers/$name" params={{ project, name }} className="flex items-center gap-2 hover:text-foreground">
      {m && <StatusDot phase={m.phase} />}
      {name}
    </Link>,
    "Terminal",
  ];
  return (
    <div className="flex h-dvh flex-col">
      <Topbar crumbs={crumbs} />
      {error && m ? <StaleBanner what="containers" error={error} onRetry={refetch} /> : null}
      {loading ? null : error && !m ? (
        <div className="px-4 py-6 md:px-8">
          <Card>
            <ErrorNotice error={error} title="The container did not load" onRetry={refetch} />
          </Card>
        </div>
      ) : !m ? (
        <EmptyState title="No such container">
          This project has no container named <span className="font-mono">{name}</span>.
        </EmptyState>
      ) : perm.can(execPermission) ? (
        <TerminalBody key={m.uid} m={m} />
      ) : perm.known ? (
        <EmptyState title="No terminal">This account's roles do not include opening a terminal in this project's containers.</EmptyState>
      ) : null}
    </div>
  );
}
