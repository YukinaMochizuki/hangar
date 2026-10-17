/// <reference types="vitest/config" />
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { fileURLToPath, URL } from "node:url";
import { defineConfig, type ProxyOptions } from "vite";

// In development the console talks to an apiserver run by `make dev`, which
// takes the caller from a header (dev auth). HANGAR_DEV_USER is the email the
// proxy sends; unset, every call is unauthenticated, which is the signed-out
// console. In production hangar-edge serves the built files and the API on one
// origin (docs/decisions/console-design.md), so none of this is used there.
const api = process.env.HANGAR_DEV_API ?? "http://127.0.0.1:8080";
const devUser = process.env.HANGAR_DEV_USER;

const proxy: ProxyOptions = {
  target: api,
  ws: true,
  configure: (p) => {
    p.on("proxyReq", (req) => {
      if (devUser) req.setHeader("X-Hangar-Dev-User", devUser);
    });
    p.on("proxyReqWs", (req) => {
      if (devUser) req.setHeader("X-Hangar-Dev-User", devUser);
    });
  },
};

// The paths hangar-edge sends to the apiserver on the console's origin
// (internal/edge/front/front.go): Connect's, the WebSocket routes and
// sign-in's.
const apiPaths = { "/hangar.": proxy, "/v1/": proxy, "/auth/": proxy };

// The headers hangar-edge serves the console's files with
// (internal/edge/front/console.go), so the built console is tried under its
// real policy. The dev server needs inline scripts for hot reload and goes
// without.
const consoleHeaders = {
  "Content-Security-Policy":
    "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; " +
    "connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'",
  "Referrer-Policy": "same-origin",
};

// The dev servers answer on loopback only: their proxy signs every call in
// as HANGAR_DEV_USER, so anyone who could reach them would act as that
// person. HANGAR_DEV_HOST widens it, such as to 0.0.0.0 in a container whose
// port is forwarded (docs/runbook/dev-env.md).
const host = process.env.HANGAR_DEV_HOST || "127.0.0.1";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) },
  },
  server: { host, port: 5173, proxy: apiPaths },
  preview: { host, port: 4173, proxy: apiPaths, headers: consoleHeaders },
  // Components' tests ask for jsdom in their first line.
  test: { include: ["src/**/*.test.{ts,tsx}"], environment: "node" },
});
