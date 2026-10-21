import { describe, expect, it } from "vitest";
import { joinWords, splitWords } from "@/lib/shell-words";

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

describe("joinWords", () => {
  it.each([
    [[], ""],
    [["sleep", "infinity"], "sleep infinity"],
    [["echo", "hi there"], "echo 'hi there'"],
    [["sh", "-c", "echo 'a' && echo \"b\""], `sh -c 'echo '\\''a'\\'' && echo "b"'`],
    [["", "$HOME", "a\\b"], `'' '$HOME' 'a\\b'`],
  ])("writes %j as a line splitWords reads back", (words, line) => {
    expect(joinWords(words)).toBe(line);
    expect(splitWords(line)).toEqual({ words });
  });
});
