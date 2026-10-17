// Screenshots console pages with headless Chromium, for design review.
// It shoots the production build served by `vite preview` (the dev server's
// HMR socket keeps a headless page from settling).
//
// Usage: node scripts/shot.mjs <out-dir> <name>=<path>[@<width>x<height>][#<x>,<y>,<w>,<h>][!<ms>] ...
// Each path gets ?still, which stops the LED animations. A #clip shoots only
// that rectangle of the viewport; !ms waits that long after load, to catch a
// frame of something that moves.

import puppeteer from "puppeteer-core";

const [outDir, ...specs] = process.argv.slice(2);
const base = process.env.BASE ?? "http://localhost:4173";

const browser = await puppeteer.launch({
  executablePath: process.env.CHROME_PATH ?? "chromium",
  args: ["--no-sandbox", "--disable-gpu", "--hide-scrollbars"],
});
try {
  for (const spec of specs) {
    const m = spec.match(/^([^=]+)=([^@#!]+)(?:@(\d+)x(\d+))?(?:#(\d+),(\d+),(\d+),(\d+))?(?:!(\d+))?$/);
    if (!m) throw new Error(`bad spec ${spec}`);
    const [, name, path, w = "1440", h = "900", cx, cy, cw, ch, wait] = m;
    const clip = cx ? { x: Number(cx), y: Number(cy), width: Number(cw), height: Number(ch) } : undefined;
    const page = await browser.newPage();
    await page.setViewport({ width: Number(w), height: Number(h), deviceScaleFactor: 2 });
    const url = base + path + (path.includes("?") ? "&" : "?") + "still";
    await page.goto(url, { waitUntil: "networkidle0" });
    await page.evaluate(() => document.fonts.ready);
    if (wait) await new Promise((r) => setTimeout(r, Number(wait)));
    const file = `${outDir}/${name}.png`;
    await page.screenshot(clip ? { path: file, clip } : { path: file, fullPage: true });
    await page.close();
    console.log(file);
  }
} finally {
  await browser.close();
}
