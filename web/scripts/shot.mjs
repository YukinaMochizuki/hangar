// Screenshots console pages with headless Chromium, for design review.
// It shoots the production build served by `vite preview` (the dev server's
// HMR socket keeps a headless page from settling).
//
// Usage: node scripts/shot.mjs <out-dir> <name>=<path>[@<width>x<height>][#<x>,<y>,<w>,<h>][!<ms>][~<selector>] ...
// Each path gets ?still, which stops the animations. A #clip shoots only
// that rectangle of the viewport; !ms waits that long after load, to catch a
// frame of something that moves; ~selector clicks the first element it
// matches first, to open a menu. BASE is the server, http://localhost:4173
// unless set; WAIT is the page load to wait for, networkidle0 unless set
// (domcontentloaded with a !ms shoots a page still loading). Errors the page
// logs, a Content-Security-Policy violation among them, go to stderr.

import puppeteer from "puppeteer-core";

const [outDir, ...specs] = process.argv.slice(2);
const base = process.env.BASE ?? "http://localhost:4173";
const waitUntil = process.env.WAIT ?? "networkidle0";

// Pages are shot at a device pixel ratio of 2, the browser's own rather than
// one the viewport emulates: under emulation Chromium reports a canvas's
// device-pixel size at 1x, and xterm's WebGL renderer, which sizes its
// canvas by it, then draws a terminal magnified to a quarter of its screen
// (hangar#214). A viewport's deviceScaleFactor of 0 leaves the ratio alone.
const browser = await puppeteer.launch({
  executablePath: process.env.CHROME_PATH ?? "chromium",
  args: ["--no-sandbox", "--disable-gpu", "--hide-scrollbars", "--force-device-scale-factor=2"],
});
try {
  for (const spec of specs) {
    const m = spec.match(/^([^=]+)=([^@#!~]+)(?:@(\d+)x(\d+))?(?:#(\d+),(\d+),(\d+),(\d+))?(?:!(\d+))?(?:~(.+))?$/);
    if (!m) throw new Error(`bad spec ${spec}`);
    const [, name, path, w = "1440", h = "900", cx, cy, cw, ch, wait, click] = m;
    const clip = cx ? { x: Number(cx), y: Number(cy), width: Number(cw), height: Number(ch) } : undefined;
    const page = await browser.newPage();
    page.on("console", (msg) => msg.type() === "error" && console.error(`${name}: ${msg.text()}`));
    page.on("pageerror", (err) => console.error(`${name}: ${err.message}`));
    await page.setViewport({ width: Number(w), height: Number(h), deviceScaleFactor: 0 });
    const url = base + path + (path.includes("?") ? "&" : "?") + "still";
    await page.goto(url, { waitUntil });
    await page.evaluate(() => document.fonts.ready);
    if (click) {
      await page.click(click);
      await new Promise((r) => setTimeout(r, 300));
    }
    if (wait) await new Promise((r) => setTimeout(r, Number(wait)));
    // A whole-page shot is taken with a viewport as tall as the page, so
    // what sticks to the viewport, such as the sidebar, spans all of it.
    if (!clip && !click) {
      const height = await page.evaluate(() => document.documentElement.scrollHeight);
      if (height > Number(h)) {
        await page.setViewport({ width: Number(w), height, deviceScaleFactor: 0 });
        await new Promise((r) => setTimeout(r, 200));
      }
    }
    const file = `${outDir}/${name}.png`;
    await page.screenshot(clip ? { path: file, clip } : { path: file, fullPage: !click });
    await page.close();
    console.log(file);
  }
} finally {
  await browser.close();
}
