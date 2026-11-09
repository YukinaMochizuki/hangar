// A terminal on an exec in a machine: xterm.js drawing what the process
// writes, and sending what is typed and the terminal's size
// (web/src/lib/exec.ts); and the same terminal on a virtual machine's
// serial console (web/src/lib/console.ts), whose size is the console's
// rather than the window's. It draws through WebGL alone, loaded before the
// terminal opens: xterm's DOM renderer lays out its text with injected
// <style> elements (fonts, cell sizes, colours), which the console's
// Content-Security-Policy (style-src 'self') blocks, so where WebGL is
// missing it draws nothing. xterm 6 still injects a <style> for its
// scrollbar (xtermjs/xterm.js#4445), whose rules styles.css holds instead.

import type { ITheme, Terminal as XTerm } from "@xterm/xterm";
import { useCallback, useEffect, useMemo, useRef, useSyncExternalStore } from "react";
import { openExec, type ExecEnd } from "@/lib/exec";

/** Where a terminal's exec stands. */
export type TerminalState =
  | { kind: "idle" }
  | { kind: "connecting" }
  | { kind: "open"; cols: number; rows: number }
  | { kind: "ended"; end: ExecEnd }
  /** The browser gives no WebGL, which the terminal draws with. */
  | { kind: "unsupported" };

/** Whether the browser can give the terminal the WebGL 2 it draws with. */
export function webglAvailable(): boolean {
  const gl = document.createElement("canvas").getContext("webgl2");
  // The probe's context is let go at once: browsers cap how many a page holds.
  gl?.getExtension("WEBGL_lose_context")?.loseContext();
  return !!gl;
}

/** The object a terminal runs its process in. */
export interface TerminalTarget {
  project: string;
  /** The kind's plural, such as "containers". */
  plural: string;
  name: string;
  command: string[];
}

// One palette in both themes, as a terminal is dark in either; the cursor and
// the selection take the accent.
const theme: ITheme = {
  background: "#18181b",
  foreground: "#e4e4e7",
  cursor: "#f97316",
  cursorAccent: "#18181b",
  selectionBackground: "#f9731655",
  black: "#27272a",
  red: "#f87171",
  green: "#4ade80",
  yellow: "#facc15",
  blue: "#60a5fa",
  magenta: "#c084fc",
  cyan: "#22d3ee",
  white: "#e4e4e7",
  brightBlack: "#71717a",
  brightRed: "#fca5a5",
  brightGreen: "#86efac",
  brightYellow: "#fde047",
  brightBlue: "#93c5fd",
  brightMagenta: "#d8b4fe",
  brightCyan: "#67e8f9",
  brightWhite: "#fafafa",
};
const fontFamily = '"Geist Mono Variable", ui-monospace, monospace';

/** endLine is what the terminal writes under the process's last output when its exec ends. */
export function endLine(end: ExecEnd): string {
  switch (end.kind) {
    case "exited":
      return end.signal ? `Exited on signal ${end.signal}.` : `Exited with code ${end.code}.`;
    case "failed":
      return `The exec failed: ${end.error.rawMessage}`;
    case "unreachable":
      return "Could not connect.";
    case "lost":
      return "The connection was lost.";
    case "closed":
      return "Disconnected.";
  }
}

/** What a terminal is connected to: an exec, or a serial console. */
export interface Session {
  send: (data: string | Uint8Array) => void;
  resize: (cols: number, rows: number) => void;
  close: () => void;
}

/** What a session tells the terminal drawing it. */
export interface SessionEvents {
  onOpen: () => void;
  onOutput: (data: Uint8Array) => void;
  onEnd: (end: ExecEnd) => void;
}

/** Starts a session on a terminal that has opened. */
export type Opener = (term: XTerm, events: SessionEvents) => Session;

/**
 * How a terminal is sized: to its element, sending each new size to its
 * session (an exec's), or by its session alone (a console's, shared by
 * every client), drawn at that size whatever the element's.
 */
export type Sizing = "fit" | "session";

/**
 * useTerminal runs a terminal on an exec of the target's: see useXterm. It
 * is the target's one terminal in this tab, so a view that unmounts hands
 * it to the next one using it.
 */
export function useTerminal(target: TerminalTarget) {
  const { project, plural, name } = target;
  const command = target.command.join("\u0000");
  const open = useCallback<Opener>(
    (term, events) =>
      openExec(
        // No command is a login shell, so "" stands for none, not for one empty argument.
        { project, plural, name, command: command === "" ? [] : command.split("\u0000"), cols: term.cols, rows: term.rows },
        events,
      ),
    [project, plural, name, command],
  );
  return useXterm(open, "fit", [project, plural, name, command].join("\u0001"));
}

type Live = { term: XTerm; host: HTMLDivElement; session?: Session; observer?: ResizeObserver; fit?: () => void };

/**
 * A terminal and its session, apart from the views showing it: the element
 * xterm opened in is moved into whichever view shows it, as xterm cannot
 * open twice. What uses it holds it, and once nothing does after a flush
 * of effects, its session ends; a page that holds it but settles on showing
 * none closes it itself.
 */
class Held {
  private state: TerminalState = { kind: "idle" };
  private live: Live | null = null;
  /** The element of the view showing the terminal. */
  private element: HTMLDivElement | null = null;
  // Counts connects and teardowns, so a connect overtaken while it waited (for the font, or for xterm to load) does nothing.
  private generation = 0;
  private holders = 0;
  private listeners = new Set<() => void>();

  /** key is what views share it by, or undefined for a terminal of one view alone. */
  constructor(private readonly key?: string) {}

  subscribe = (listener: () => void) => {
    this.listeners.add(listener);
    return () => void this.listeners.delete(listener);
  };

  getState = () => this.state;

  private setState(next: TerminalState | ((s: TerminalState) => TerminalState)) {
    const s = typeof next === "function" ? next(this.state) : next;
    if (s === this.state) return;
    this.state = s;
    for (const l of this.listeners) l();
  }

  hold() {
    this.holders++;
    if (this.key !== undefined && !shared.has(this.key)) shared.set(this.key, this);
  }

  // A view handing the terminal to the next releases it before the next holds
  // it, in the same flush of effects, so the end waits for that flush.
  release() {
    this.holders--;
    queueMicrotask(() => {
      if (this.holders > 0) return;
      this.close();
      if (this.key !== undefined && shared.get(this.key) === this) shared.delete(this.key);
    });
  }

  show(el: HTMLDivElement) {
    this.element = el;
    const l = this.live;
    if (l && l.host.parentElement !== el) {
      el.appendChild(l.host);
      l.fit?.();
      l.term.focus();
    }
  }

  hide(el: HTMLDivElement) {
    if (this.element === el) this.element = null;
  }

  private teardown() {
    this.generation++;
    const l = this.live;
    this.live = null;
    if (!l) return;
    l.observer?.disconnect();
    l.session?.close();
    l.term.dispose();
    l.host.remove();
  }

  connect = async (open: Opener, sizing: Sizing) => {
    this.teardown();
    const g = this.generation;
    if (!webglAvailable()) {
      this.setState({ kind: "unsupported" });
      return;
    }
    this.setState({ kind: "connecting" });
    let x: typeof import("@/components/xterm");
    try {
      [x] = await Promise.all([
        import("@/components/xterm"),
        // xterm measures its cells once it opens, so the face must be there.
        document.fonts?.load(`13px ${fontFamily}`).catch(() => undefined),
      ]);
    } catch {
      // The terminal's own code did not load, as when a release replaced it.
      if (g === this.generation) this.setState({ kind: "ended", end: { kind: "unreachable" } });
      return;
    }
    const el = this.element;
    if (g !== this.generation) return;
    if (!el) {
      this.setState({ kind: "idle" });
      return;
    }
    const term = new x.Terminal({ fontFamily, fontSize: 13, lineHeight: 1.15, cursorBlink: true, theme, scrollback: 5000 });
    const fitAddon = new x.FitAddon();
    term.loadAddon(fitAddon);
    // A context the GPU took back is asked for again; when none comes, the exec ends rather than draw with the DOM renderer.
    const draw = () => {
      const webgl = new x.WebglAddon();
      webgl.onContextLoss(() => {
        webgl.dispose();
        try {
          draw();
        } catch {
          this.teardown();
          this.setState({ kind: "unsupported" });
        }
      });
      term.loadAddon(webgl);
    };
    draw();
    const host = document.createElement("div");
    host.className = "h-full w-full";
    el.appendChild(host);
    term.open(host);
    const l: Live = { term, host };
    if (sizing === "fit") {
      // Between views the element is in none, and has no size to fit.
      l.fit = () => {
        if (host.isConnected) fitAddon.fit();
      };
      l.fit();
      l.observer = new ResizeObserver(l.fit);
      l.observer.observe(host);
    }
    this.live = l;
    const session = open(term, {
      onOpen: () => {
        this.setState({ kind: "open", cols: term.cols, rows: term.rows });
        term.focus();
      },
      onOutput: (d) => term.write(d),
      onEnd: (end) => {
        if (this.live !== l) return;
        term.write(`\r\n\x1b[2m${endLine(end)}\x1b[0m\r\n`);
        term.options.cursorBlink = false;
        this.setState({ kind: "ended", end });
      },
    });
    l.session = session;
    term.onData((d) => session.send(d));
    // Mouse reports in xterm's default encoding are bytes, not text.
    term.onBinary((d) => session.send(Uint8Array.from(d, (c) => c.charCodeAt(0))));
    term.onResize(({ cols, rows }) => {
      // A session that sizes the terminal is not told the size it gave.
      if (sizing === "fit") session.resize(cols, rows);
      this.setState((s) => (s.kind === "open" ? { kind: "open", cols, rows } : s));
    });
  };

  disconnect = () => this.live?.session?.close();

  close = () => {
    this.teardown();
    this.setState({ kind: "idle" });
  };

  fits = (area: HTMLElement, inset = 0): { cols: number; rows: number } | undefined => {
    const l = this.live;
    const screen = l?.term.element?.querySelector(".xterm-screen");
    if (!l || !screen) return undefined;
    const r = screen.getBoundingClientRect();
    if (r.width === 0 || r.height === 0) return undefined;
    const cellW = r.width / l.term.cols;
    const cellH = r.height / l.term.rows;
    // The area's padding is no room for cells.
    const pad = getComputedStyle(area);
    const width = area.clientWidth - parseFloat(pad.paddingLeft) - parseFloat(pad.paddingRight) - inset;
    const height = area.clientHeight - parseFloat(pad.paddingTop) - parseFloat(pad.paddingBottom) - inset;
    return { cols: Math.max(1, Math.floor(width / cellW)), rows: Math.max(1, Math.floor(height / cellH)) };
  };

  resize = (cols: number, rows: number) => this.live?.session?.resize(cols, rows);
}

/** The terminals of this tab that are shared by key, as useTerminal's are by target. */
const shared = new Map<string, Held>();

// A render may be thrown away, so only hold puts a terminal in shared.
function heldFor(key: string | undefined): Held {
  return (key !== undefined && shared.get(key)) || new Held(key);
}

/**
 * useXterm runs a terminal in the element given to attach, on the session
 * open starts. Nothing connects until connect is called; connect again
 * starts a new session, and leaving the page ends it. With a key, the
 * terminal is the one every view using that key shows: a view leaving hands
 * it to the next, and it ends only when the last one leaves. fits gives the
 * size that fills an element at the terminal's cell size, once it is drawn.
 */
export function useXterm(open: Opener, sizing: Sizing, key?: string) {
  const held = useMemo(() => heldFor(key), [key]);
  useEffect(() => {
    held.hold();
    return () => held.release();
  }, [held]);
  const state = useSyncExternalStore(held.subscribe, held.getState);

  const shown = useRef<HTMLDivElement | null>(null);
  const attach = useCallback(
    (el: HTMLDivElement | null) => {
      if (el) held.show(el);
      else if (shown.current) held.hide(shown.current);
      shown.current = el;
    },
    [held],
  );

  const connect = useCallback(() => held.connect(open, sizing), [held, open, sizing]);

  // A page left for the back/forward cache keeps its WebSocket, and with it
  // the process, until it is restored; leaving ends the exec instead.
  useEffect(() => {
    window.addEventListener("pagehide", held.disconnect);
    return () => window.removeEventListener("pagehide", held.disconnect);
  }, [held]);

  return {
    attach,
    state,
    connect,
    disconnect: held.disconnect,
    /** close ends the exec and lets the terminal go, as a card folded away does. */
    close: held.close,
    fits: held.fits,
    /** resize asks the live session for a size, as a console's holder of the input does for everyone's screen. */
    resize: held.resize,
  };
}
