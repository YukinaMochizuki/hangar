# Streaming transport

Status: accepted, 2026-10.

## Context

Exec, console, port-forward and SSH need bidirectional streams. connect-web
cannot do bidirectional streaming from a browser, and gRPC bidirectional
streams through Cloudflare are risky.

## Decision

- Bidirectional streams use WebSocket, with messages still defined in proto.
- Everything else uses Connect unary and server-streaming calls.
- `Watch` sends a `BOOKMARK` every 30 seconds as a heartbeat, so idle streams
  survive proxies and clients advance their revision.
- The server speaks HTTP/1.1 and h2c on the same port, so Connect streaming
  works whichever protocol cloudflared uses to reach it.

## Consequences

Agents get a CLI (`-o json`, non-interactive, `wait`, defined exit codes) and
generated Go and TypeScript SDKs. An MCP server comes later as `hangar mcp`
over stdio, reusing the CLI's client and login.
