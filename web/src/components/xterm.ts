// xterm.js and the addons the console's terminal uses, in a module of its
// own: the terminal imports it when it connects, so pages that open none do
// not load it (web/src/components/terminal.tsx).

import "@xterm/xterm/css/xterm.css";

export { FitAddon } from "@xterm/addon-fit";
export { WebglAddon } from "@xterm/addon-webgl";
export { Terminal } from "@xterm/xterm";
