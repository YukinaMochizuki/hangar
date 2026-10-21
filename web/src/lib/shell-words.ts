// A command line typed as one line, split into its words as a POSIX shell
// splits them, which is how Compose reads an entrypoint or command written
// as a string. Only quoting and escapes count: no variables, globs or
// operators, which are passed on as they are written.

export type Words = { words: string[]; error?: undefined } | { words?: undefined; error: string };

/** Splits a line into words: spaces part them, quotes and backslashes keep them together. */
export function splitWords(line: string): Words {
  const words: string[] = [];
  let word = "";
  // Whether a word has begun, so that "" is a word of its own.
  let inWord = false;
  let i = 0;
  while (i < line.length) {
    const c = line[i];
    if (c === " " || c === "\t" || c === "\n") {
      if (inWord) words.push(word);
      word = "";
      inWord = false;
      i++;
    } else if (c === "\\") {
      if (i + 1 >= line.length) return { error: "The line ends in a backslash." };
      // A backslash before a newline joins the lines.
      if (line[i + 1] !== "\n") {
        word += line[i + 1];
        inWord = true;
      }
      i += 2;
    } else if (c === "'") {
      const end = line.indexOf("'", i + 1);
      if (end < 0) return { error: "A single quote is not closed." };
      word += line.slice(i + 1, end);
      inWord = true;
      i = end + 1;
    } else if (c === '"') {
      i++;
      inWord = true;
      for (;;) {
        if (i >= line.length) return { error: "A double quote is not closed." };
        const d = line[i];
        if (d === '"') {
          i++;
          break;
        }
        // Within double quotes a backslash escapes only these.
        if (d === "\\" && i + 1 < line.length && '"\\$`\n'.includes(line[i + 1])) {
          if (line[i + 1] !== "\n") word += line[i + 1];
          i += 2;
        } else {
          word += d;
          i++;
        }
      }
    } else {
      word += c;
      inWord = true;
      i++;
    }
  }
  if (inWord) words.push(word);
  return { words };
}

// A word that splitWords reads back as it is, unquoted.
const bare = /^[A-Za-z0-9_@%+=:,./-]+$/;

/** Writes words as one line that splitWords splits back into them: a word with anything else in it is single-quoted. */
export function joinWords(words: string[]): string {
  return words.map((w) => (bare.test(w) ? w : `'${w.replaceAll("'", `'\\''`)}'`)).join(" ");
}
