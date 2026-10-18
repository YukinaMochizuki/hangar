// A terminal on an exec in a machine: xterm.js drawing what the process
// writes, and sending what is typed and the terminal's size
// (web/src/lib/exec.ts). It draws through WebGL alone, loaded before the
// terminal opens: xterm's DOM renderer lays out its text with injected
// <style> elements (fonts, cell sizes, colours), which the console's
// Content-Security-Policy (style-src 'self') blocks, so where WebGL is
// missing it draws nothing. xterm 6 still injects a <style> for its
// scrollbar (xtermjs/xterm.js#4445), whose rules styles.css holds instead.

import type { ITheme, Terminal as XTerm } from "@xterm/xterm";
import { useCallback, useEffect, useRef, useState } from "react";
import { openExec, type Exec, type ExecEnd } from "@/lib/exec";

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

/**
 * useTerminal runs a terminal in the element given to attach. Nothing
 * connects until connect is called; connect again starts a new process, and
 * leaving the page ends it.
 */
export function useTerminal(target: TerminalTarget) {
  const element = useRef<HTMLDivElement | null>(null);
  const attach = useCallback((el: HTMLDivElement | null) => {
    element.current = el;
  }, []);
  const [state, setState] = useState<TerminalState>({ kind: "idle" });
  const live = useRef<{ term: XTerm; exec?: Exec; observer: ResizeObserver } | null>(null);
  // Counts connects and teardowns, so a connect overtaken while it waited (for the font, or for xterm to load) does nothing.
  const generation = useRef(0);
  const { project, plural, name } = target;
  const command = target.command.join("\u0000");

  const teardown = useCallback(() => {
    generation.current++;
    const l = live.current;
    live.current = null;
    if (!l) return;
    l.observer.disconnect();
    l.exec?.close();
    l.term.dispose();
  }, []);

  const connect = useCallback(async () => {
    teardown();
    const g = generation.current;
    if (!webglAvailable()) {
      setState({ kind: "unsupported" });
      return;
    }
    setState({ kind: "connecting" });
    let x: typeof import("@/components/xterm");
    try {
      [x] = await Promise.all([
        import("@/components/xterm"),
        // xterm measures its cells once it opens, so the face must be there.
        document.fonts?.load(`13px ${fontFamily}`).catch(() => undefined),
      ]);
    } catch {
      // The terminal's own code did not load, as when a release replaced it.
      if (g === generation.current) setState({ kind: "ended", end: { kind: "unreachable" } });
      return;
    }
    const el = element.current;
    if (g !== generation.current) return;
    if (!el) {
      setState({ kind: "idle" });
      return;
    }
    const term = new x.Terminal({ fontFamily, fontSize: 13, lineHeight: 1.15, cursorBlink: true, theme, scrollback: 5000 });
    const fit = new x.FitAddon();
    term.loadAddon(fit);
    // A context the GPU took back is asked for again; when none comes, the exec ends rather than draw with the DOM renderer.
    const draw = () => {
      const webgl = new x.WebglAddon();
      webgl.onContextLoss(() => {
        webgl.dispose();
        try {
          draw();
        } catch {
          teardown();
          setState({ kind: "unsupported" });
        }
      });
      term.loadAddon(webgl);
    };
    draw();
    term.open(el);
    fit.fit();
    const observer = new ResizeObserver(() => fit.fit());
    observer.observe(el);
    const l: { term: XTerm; exec?: Exec; observer: ResizeObserver } = { term, observer };
    live.current = l;
    const exec = openExec(
      { project, plural, name, command: command.split("\u0000"), cols: term.cols, rows: term.rows },
      {
        onOpen: () => {
          setState({ kind: "open", cols: term.cols, rows: term.rows });
          term.focus();
        },
        onOutput: (d) => term.write(d),
        onEnd: (end) => {
          if (live.current !== l) return;
          term.write(`\r\n\x1b[2m${endLine(end)}\x1b[0m\r\n`);
          term.options.cursorBlink = false;
          setState({ kind: "ended", end });
        },
      },
    );
    l.exec = exec;
    term.onData((d) => exec.send(d));
    // Mouse reports in xterm's default encoding are bytes, not text.
    term.onBinary((d) => exec.send(Uint8Array.from(d, (c) => c.charCodeAt(0))));
    term.onResize(({ cols, rows }) => {
      exec.resize(cols, rows);
      setState((s) => (s.kind === "open" ? { kind: "open", cols, rows } : s));
    });
  }, [project, plural, name, command, teardown]);

  const disconnect = useCallback(() => live.current?.exec?.close(), []);

  /** close ends the exec and lets the terminal go, as a card folded away does. */
  const close = useCallback(() => {
    teardown();
    setState({ kind: "idle" });
  }, [teardown]);

  useEffect(() => teardown, [teardown]);

  // A page left for the back/forward cache keeps its WebSocket, and with it
  // the process, until it is restored; leaving ends the exec instead.
  useEffect(() => {
    window.addEventListener("pagehide", disconnect);
    return () => window.removeEventListener("pagehide", disconnect);
  }, [disconnect]);

  return { attach, state, connect, disconnect, close };
}
