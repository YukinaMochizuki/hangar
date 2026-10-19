import { describe, expect, it } from "vitest";
import { splitWords } from "@/lib/shell-words";

describe("splitWords", () => {
  it.each([
    ["", []],
    ["   ", []],
    ["sleep infinity", ["sleep", "infinity"]],
    ["  a   b  ", ["a", "b"]],
    [`sh -c 'echo hi && sleep 1d'`, ["sh", "-c", "echo hi && sleep 1d"]],
    [`echo "a \\"b\\" $HOME"`, ["echo", `a "b" $HOME`]],
    [`echo "a\\nb"`, ["echo", "a\\nb"]],
    [`a\\ b c`, ["a b", "c"]],
    [`'' ""`, ["", ""]],
    [`pre'mid'"post"`, ["premidpost"]],
    [`'it'\\''s'`, ["it's"]],
  ])("splits %j", (line, words) => {
    expect(splitWords(line)).toEqual({ words });
  });

  it.each([
    [`"/bin/sh`, "A double quote is not closed."],
    [`'/bin/sh`, "A single quote is not closed."],
    [`sh \\`, "The line ends in a backslash."],
  ])("refuses %j", (line, error) => {
    expect(splitWords(line)).toEqual({ error });
  });
});
