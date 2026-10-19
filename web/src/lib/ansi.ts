// What a machine writes to its log, as a terminal would colour it. Only
// SGR sequences (ESC [ … m: colours, bold, dim, italic, underline,
// inverse) are kept, as styles; every other escape sequence (cursor moves,
// erasing, titles) and control character is dropped, as it means nothing
// in a log read later.

/** A colour: one of the 16 of the palette, by index, or an rgb() of its own. */
export type AnsiColor = number | string;

export type AnsiStyle = {
  fg?: AnsiColor;
  bg?: AnsiColor;
  bold?: boolean;
  dim?: boolean;
  italic?: boolean;
  underline?: boolean;
  inverse?: boolean;
};

export type AnsiSpan = { text: string; style: AnsiStyle };

// CSI (ESC [ params final), OSC (ESC ] … BEL or ESC \), or a two-character
// escape. The ESC may have been lost to a byte boundary, so a lone one is
// matched too. Control characters are what these match.
// eslint-disable-next-line no-control-regex
const escape = /\x1b(?:\[([0-9;:?<=>]*)[ -/]*([@-~])|\][^\x07\x1b]*(?:\x07|\x1b\\)?|[@-Z\\-_]|$)/g;
// Control characters but tab, which a log line keeps.
// eslint-disable-next-line no-control-regex
const control = /[\x00-\x08\x0a-\x1f\x7f]/g;

/** The 256-colour palette's colour n, past the first 16. */
function color256(n: number): AnsiColor {
  if (n < 16) return n;
  if (n >= 232) {
    const v = 8 + (n - 232) * 10;
    return `rgb(${v}, ${v}, ${v})`;
  }
  const i = n - 16;
  const level = (c: number) => (c === 0 ? 0 : 55 + c * 40);
  return `rgb(${level(Math.floor(i / 36))}, ${level(Math.floor(i / 6) % 6)}, ${level(i % 6)})`;
}

/** Applies one SGR sequence's parameters to style, as a terminal would. */
function sgr(params: string, style: AnsiStyle): AnsiStyle {
  const s = { ...style };
  // ":" separates sub-parameters (38:5:n); they read as ";" here.
  const p = params === "" ? [0] : params.split(/[;:]/).map((x) => (x === "" ? 0 : Number(x)));
  for (let i = 0; i < p.length; i++) {
    const c = p[i];
    if (c === 0) {
      for (const k of Object.keys(s) as (keyof AnsiStyle)[]) delete s[k];
    } else if (c === 1) s.bold = true;
    else if (c === 2) s.dim = true;
    else if (c === 3) s.italic = true;
    else if (c === 4) s.underline = true;
    else if (c === 7) s.inverse = true;
    else if (c === 22) s.bold = s.dim = undefined;
    else if (c === 23) s.italic = undefined;
    else if (c === 24) s.underline = undefined;
    else if (c === 27) s.inverse = undefined;
    else if (c >= 30 && c <= 37) s.fg = c - 30;
    else if (c === 39) s.fg = undefined;
    else if (c >= 40 && c <= 47) s.bg = c - 40;
    else if (c === 49) s.bg = undefined;
    else if (c >= 90 && c <= 97) s.fg = c - 90 + 8;
    else if (c >= 100 && c <= 107) s.bg = c - 100 + 8;
    else if (c === 38 || c === 48) {
      let color: AnsiColor | undefined;
      if (p[i + 1] === 5 && i + 2 < p.length) {
        color = color256(p[i + 2]);
        i += 2;
      } else if (p[i + 1] === 2 && i + 4 < p.length) {
        color = `rgb(${p[i + 2]}, ${p[i + 3]}, ${p[i + 4]})`;
        i += 4;
      } else break;
      if (c === 38) s.fg = color;
      else s.bg = color;
    }
  }
  for (const k of Object.keys(s) as (keyof AnsiStyle)[]) if (s[k] === undefined) delete s[k];
  return s;
}

/**
 * Splits one line into spans of one style each, starting in the style the
 * line before it ended in, and says the style it ends in.
 */
export function parseAnsi(line: string, start: AnsiStyle = {}): { spans: AnsiSpan[]; end: AnsiStyle } {
  const spans: AnsiSpan[] = [];
  let style = start;
  let at = 0;
  const push = (text: string) => {
    const t = text.replace(control, "");
    if (t === "") return;
    const last = spans[spans.length - 1];
    if (last && last.style === style) last.text += t;
    else spans.push({ text: t, style });
  };
  for (const m of line.matchAll(escape)) {
    push(line.slice(at, m.index));
    at = m.index + m[0].length;
    if (m[2] === "m" && !m[1]?.match(/[?<=>]/)) style = sgr(m[1] ?? "", style);
  }
  push(line.slice(at));
  return { spans, end: style };
}
