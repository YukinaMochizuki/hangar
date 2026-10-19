import { describe, expect, it } from "vitest";
import { parseAnsi } from "@/lib/ansi";

const ESC = "\x1b";

describe("parseAnsi", () => {
  it("leaves a line without escapes as one plain span", () => {
    expect(parseAnsi("hello world").spans).toEqual([{ text: "hello world", style: {} }]);
  });

  it("styles systemd's boot lines, and drops the escapes", () => {
    const line = `[${ESC}[0;32m  OK  ${ESC}[0m] Finished ${ESC}[0;1;39msystemd-networkd-wait-onl…${ESC}[0m Wait for Network to be Configured.`;
    const { spans, end } = parseAnsi(line);
    expect(spans).toEqual([
      { text: "[", style: {} },
      { text: "  OK  ", style: { fg: 2 } },
      { text: "] Finished ", style: {} },
      { text: "systemd-networkd-wait-onl…", style: { bold: true } },
      { text: " Wait for Network to be Configured.", style: {} },
    ]);
    expect(end).toEqual({});
  });

  it("reads bright, 256 and true colours, and backgrounds", () => {
    expect(parseAnsi(`${ESC}[91ma${ESC}[38;5;196mb${ESC}[38;5;3mc${ESC}[38;2;10;20;30;44md`).spans).toEqual([
      { text: "a", style: { fg: 9 } },
      { text: "b", style: { fg: "rgb(255, 0, 0)" } },
      { text: "c", style: { fg: 3 } },
      { text: "d", style: { fg: "rgb(10, 20, 30)", bg: 4 } },
    ]);
  });

  it("turns attributes off one by one", () => {
    expect(parseAnsi(`${ESC}[1;3;4mx${ESC}[22my${ESC}[23;24mz`).spans).toEqual([
      { text: "x", style: { bold: true, italic: true, underline: true } },
      { text: "y", style: { italic: true, underline: true } },
      { text: "z", style: {} },
    ]);
  });

  it("carries a style from the line before, and says the one it ends in", () => {
    const first = parseAnsi(`${ESC}[33mwarn`);
    expect(first.end).toEqual({ fg: 3 });
    expect(parseAnsi("still yellow", first.end).spans).toEqual([{ text: "still yellow", style: { fg: 3 } }]);
  });

  it("drops cursor moves, erases, titles and control characters", () => {
    expect(parseAnsi(`${ESC}[?25l${ESC}[2K\r${ESC}]0;title\x07done\b${ESC}[K${ESC}`).spans).toEqual([{ text: "done", style: {} }]);
  });

  it("keeps tabs", () => {
    expect(parseAnsi("a\tb").spans).toEqual([{ text: "a\tb", style: {} }]);
  });
});
