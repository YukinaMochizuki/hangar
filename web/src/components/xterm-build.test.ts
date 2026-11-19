/// <reference types="node" />

// The xterm chunk the console's build emits answers a DECRQM and goes on drawing
// (docs/decisions/console-design.md, "Stack"). In node, not jsdom: esbuild
// refuses jsdom's TextEncoder, and an unopened terminal needs no DOM.

import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { build, type Rollup } from "vite";
import { afterAll, beforeAll, expect, it } from "vitest";

let xterm: typeof import("@xterm/xterm");
let dir: string | undefined;

// The whole console is built, which takes seconds.
beforeAll(async () => {
  const configFile = fileURLToPath(new URL("../../vite.config.ts", import.meta.url));
  const out = await build({ configFile, logLevel: "silent", build: { write: false } });
  const outputs = (Array.isArray(out) ? out : [out]) as Rollup.RollupOutput[];
  const chunk = outputs
    .flatMap((o) => o.output)
    .find((f): f is Rollup.OutputChunk => f.type === "chunk" && f.fileName.startsWith("assets/xterm-"));
  if (!chunk) throw new Error("the build emitted no xterm chunk");
  // A file under the project, which vitest's loader serves, so a failure's stack names it briefly.
  const tmp = fileURLToPath(new URL("../../node_modules/.tmp", import.meta.url));
  await mkdir(tmp, { recursive: true });
  dir = await mkdtemp(join(tmp, "xterm-build-"));
  const file = join(dir, "xterm.mjs");
  await writeFile(file, chunk.code);
  xterm = await import(/* @vite-ignore */ pathToFileURL(file).href);
}, 60_000);

afterAll(async () => {
  if (dir) await rm(dir, { recursive: true, force: true });
});

it("answers a DECRQM and goes on drawing, as built", async () => {
  const term = new xterm.Terminal({ cols: 80, rows: 24 });
  const replies: string[] = [];
  term.onData((d) => replies.push(d));
  // The write's callback runs once the parser has taken the whole chunk.
  await new Promise<void>((done) => term.write("\x1b[?12$pdrawn", done));
  expect(replies).toEqual(["\x1b[?12;2$y"]);
  expect(term.buffer.active.getLine(0)?.translateToString(true)).toBe("drawn");
  term.dispose();
});
