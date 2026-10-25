// A virtual machine's serial console, its ttyS0, as a page of its own:
// `hangar console` in the browser (docs/decisions/serial-console.md). It is
// slower than a terminal over SSH and meant for what SSH cannot reach, the
// boot loader, a boot that stalls, a guest with no network, so the
// machine's page offers it only in its "..." menu. The screen is the
// console's, shared by every client attached, at the size the one holding
// the input set, 80x24 until then; the terminal is drawn at that size and
// scrolls in a smaller window.

import { Link } from "@tanstack/react-router";
import { Check, ChevronDown, Keyboard, Plug, RotateCw, Unplug } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";
import { StaleBanner } from "@/components/credentials";
import { AnchoredLayer } from "@/components/menu";
import { Topbar, useProjectName } from "@/components/shell";
import { EmptyState, ErrorNotice } from "@/components/states";
import { useXterm, type Opener, type TerminalState } from "@/components/terminal";
import { Button, Card, CopyButton, StatusDot } from "@/components/ui";
import { cn } from "@/lib/cn";
import { consoleBounds, openConsole, type ConsoleSize, type ConsoleState } from "@/lib/console";
import { machinePath, useMachines, type MachineView } from "@/lib/machines";
import { useCan } from "@/lib/permissions";
import { execPermission, sshPermission } from "@/pages/machine-terminal";

/** The permission a serial console needs: an exec's, as it reaches the guest past its login. */
export const consolePermission = execPermission;

/**
 * Whether the caller may attach to a VM's serial console in the project;
 * false while unknown or failed. It asks what useMayTerminal asks, so the
 * machine's page asks once.
 */
export function useMayConsole(project: string): boolean {
  return useCan({ level: "project", name: project }, [execPermission, sshPermission]).can(consolePermission);
}

/** Why the VM's console cannot be attached to now, or undefined when it may be. */
export function consoleBlock(m: MachineView): string | undefined {
  if (m.deleting) return `${m.name} is being deleted.`;
  if (m.stopped) return `${m.name} is stopped.`;
  return undefined;
}

const cliOf = (m: { name: string; project: string }) => `hangar console vm ${m.name} -p ${m.project}`;

/** The sizes offered, beside the one that fills the window. */
const presets: ConsoleSize[] = [
  { cols: 80, rows: 24 },
  { cols: 100, rows: 30 },
  { cols: 120, rows: 40 },
  { cols: 160, rows: 50 },
];

/** What the screen's frame takes each way, in pixels: its padding (p-2) and border, both sides. */
const frame = 2 * (8 + 1);

const sizeText = (s: ConsoleSize) => `${s.cols}×${s.rows}`;

/** within keeps a size inside the bounds the console takes. */
const within = (s: ConsoleSize | undefined): ConsoleSize | undefined =>
  s && { cols: Math.min(Math.max(s.cols, consoleBounds.cols.min), consoleBounds.cols.max), rows: Math.min(Math.max(s.rows, consoleBounds.rows.min), consoleBounds.rows.max) };

function StateText({ state, console: cs }: { state: TerminalState; console?: ConsoleState }) {
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
      if (!cs) return <span className="text-muted">Attaching…</span>;
      return cs.input ? (
        <span className="flex items-center gap-2">
          <span className="size-2 rounded-full bg-ok" />
          Typing
        </span>
      ) : (
        <span className="flex items-center gap-2">
          <span className="size-2 rounded-full bg-pending" />
          Watching <span className="text-muted">· someone else is typing</span>
        </span>
      );
    case "ended":
      return (
        <span className="flex items-center gap-2">
          <span className={cn("size-2 rounded-full", state.end.kind === "closed" ? "bg-off" : "bg-fail")} />
          {state.end.kind === "closed" ? "Disconnected" : state.end.kind === "failed" ? "Failed" : "Not connected"}
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

/** The screen's size, and the sizes the one holding the input may set it to, for every client. */
function SizeMenu({ size, onPick, fit }: { size: ConsoleSize; onPick: (s: ConsoleSize) => void; fit: () => ConsoleSize | undefined }) {
  const [open, setOpen] = useState(false);
  const [anchor, setAnchor] = useState<HTMLDivElement | null>(null);
  const close = useCallback(() => setOpen(false), []);
  const fitting = open ? fit() : undefined;
  const pick = (s: ConsoleSize) => {
    setOpen(false);
    onPick(s);
  };
  const item = (label: string, s: ConsoleSize | undefined, note?: string) => (
    <button
      key={label}
      role="menuitemradio"
      aria-checked={!!s && s.cols === size.cols && s.rows === size.rows}
      disabled={!s}
      onClick={() => s && pick(s)}
      className="flex w-full items-center gap-2 rounded px-2 py-1.5 text-left text-[13px] hover:bg-surface-2 disabled:opacity-50"
    >
      <Check className={cn("size-3.5", !(s && s.cols === size.cols && s.rows === size.rows) && "invisible")} />
      <span className="tabular font-mono text-[12.5px]">{label}</span>
      {note && <span className="ml-auto text-[12px] text-muted">{note}</span>}
    </button>
  );
  return (
    <div ref={setAnchor}>
      <Button size="sm" aria-haspopup="menu" aria-expanded={open} onClick={() => setOpen(!open)}>
        <span className="tabular font-mono text-[12px]">{sizeText(size)}</span>
        <ChevronDown />
      </Button>
      {open && (
        <AnchoredLayer anchor={anchor} onAway={close} role="menu" aria-label="Screen size" className="w-56 rounded-md border bg-surface p-1 shadow-lg">
          {presets.map((s) => item(sizeText(s), s))}
          <div className="my-1 border-t" />
          {item(fitting ? sizeText(fitting) : "Fit window", fitting, "Fit window")}
          <p className="px-2 pb-1 pt-1.5 text-[11.5px] leading-snug text-subtle">Everyone attached sees this size.</p>
        </AnchoredLayer>
      )}
    </div>
  );
}

function ConsoleBody({ m }: { m: MachineView }) {
  const [cs, setCs] = useState<ConsoleState | undefined>();
  // Whether the next attach takes the input, read once by the attach it is set for.
  const force = useRef(false);
  const open = useCallback<Opener>(
    (term, events) => {
      const take = force.current;
      force.current = false;
      setCs(undefined);
      return openConsole(
        { project: m.project, plural: "virtualmachines", name: m.name, force: take },
        {
          onOpen: events.onOpen,
          onOutput: events.onOutput,
          onState: (s) => {
            // The state comes before the screen it is drawn at.
            if (term.cols !== s.size.cols || term.rows !== s.size.rows) term.resize(s.size.cols, s.size.rows);
            term.options.disableStdin = !s.input;
            term.options.cursorBlink = s.input;
            setCs(s);
          },
          onEnd: (e) => {
            setCs(undefined);
            events.onEnd(e);
          },
        },
      );
    },
    [m.project, m.name],
  );
  const { attach, state, connect, disconnect, fits, resize } = useXterm(open, "session");
  const area = useRef<HTMLDivElement | null>(null);
  const block = consoleBlock(m);
  useEffect(() => {
    if (!block) void connect();
    // The page attaches once, on opening; Reconnect attaches again.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  const cli = cliOf(m);
  const waiting = !!block && state.kind === "idle";
  const drawn = !waiting && state.kind !== "unsupported";
  const live = state.kind === "open" || state.kind === "connecting";
  const takeInput = () => {
    force.current = true;
    void connect();
  };
  return (
    <>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2 border-b px-4 py-2.5 text-[13px] md:px-8">
        {waiting ? <span className="text-muted">{block}</span> : <StateText state={state} console={cs} />}
        <span className="flex-1" />
        <code className="hidden items-center gap-1 rounded-md border bg-surface-2 py-1 pl-2 pr-1 font-mono text-[11.5px] text-muted lg:flex">
          <span className="select-none text-accent">$</span> {cli}
          <CopyButton text={cli} />
        </code>
        {!waiting && (
          <span className="flex flex-wrap items-center gap-2">
            {cs?.input && <SizeMenu size={cs.size} onPick={(s) => resize(s.cols, s.rows)} fit={() => within(area.current ? fits(area.current, frame) : undefined)} />}
            {cs && !cs.input && (
              <span className="tabular font-mono text-[12px] text-muted" title="The size the one typing set">
                {sizeText(cs.size)}
              </span>
            )}
            {cs && !cs.input && (
              <Button size="sm" onClick={takeInput}>
                <Keyboard /> Take input
              </Button>
            )}
            {live ? (
              <Button size="sm" variant="danger" onClick={disconnect}>
                <Unplug /> Disconnect
              </Button>
            ) : state.kind === "idle" ? (
              <Button size="sm" variant="primary" onClick={() => void connect()}>
                <Plug /> Connect
              </Button>
            ) : (
              <Button size="sm" onClick={() => void connect()}>
                <RotateCw /> Reconnect
              </Button>
            )}
          </span>
        )}
      </div>
      <p className="px-4 py-2 text-[12px] text-muted md:px-8">The serial console is slower than SSH.</p>
      {state.kind === "unsupported" && (
        <p className="px-4 py-2.5 text-[12.5px] text-muted md:px-8">
          This browser has no WebGL. <span className="font-mono">{cli}</span> attaches to the same console.
        </p>
      )}
      {state.kind === "ended" && state.end.kind === "unreachable" && (
        <p className="px-4 py-2.5 text-[12.5px] text-muted md:px-8">
          The console could not connect, and <span className="font-mono">{cli}</span> shows why.
        </p>
      )}
      {drawn && (
        <div className="flex min-h-0 min-w-0 flex-1 flex-col px-4 pb-4 md:px-8 md:pb-6">
          {/* The screen keeps the console's size, framed as it is: a smaller window scrolls it. */}
          <div ref={area} className="min-h-0 flex-1 overflow-auto">
            <div className="w-max rounded-lg border bg-[#18181b] p-2">
              <div ref={attach} />
            </div>
          </div>
        </div>
      )}
    </>
  );
}

/** A VM's serial console as a page of its own, filling the window under the top bar. */
export function ConsolePage({ name }: { name: string }) {
  const project = useProjectName();
  const { held, loading, error, refetch } = useMachines(project, "VirtualMachine");
  const may = useMayConsole(project);
  const perm = useCan({ level: "project", name: project }, [execPermission, sshPermission]);
  const m = held.find((x) => x.name === name);
  const crumbs = [
    "Virtual machines",
    <Link key="name" to={machinePath("VirtualMachine")} params={{ project, name }} className="flex items-center gap-2 hover:text-foreground">
      {m && <StatusDot phase={m.phase} />}
      {name}
    </Link>,
    "Serial console",
  ];
  return (
    <div className="flex h-dvh flex-col">
      <Topbar crumbs={crumbs} />
      {error && m ? <StaleBanner what="virtual machines" error={error} onRetry={refetch} /> : null}
      {loading ? null : error && !m ? (
        <div className="px-4 py-6 md:px-8">
          <Card>
            <ErrorNotice error={error} title="The virtual machine did not load" onRetry={refetch} />
          </Card>
        </div>
      ) : !m ? (
        <EmptyState title="No such virtual machine">
          This project has no virtual machine named <span className="font-mono">{name}</span>.
        </EmptyState>
      ) : may ? (
        <ConsoleBody key={m.uid} m={m} />
      ) : perm.known ? (
        <EmptyState title="No serial console">This account's roles do not include attaching to this project's virtual machines' serial consoles.</EmptyState>
      ) : null}
    </div>
  );
}
