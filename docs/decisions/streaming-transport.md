# Streaming transport

Status: accepted, 2026-10.

## Context

Exec, console, port-forward and SSH need bidirectional streams. connect-web
cannot do bidirectional streaming from a browser, and gRPC bidirectional
streams through Cloudflare are risky.

## Decision

- Bidirectional streams use WebSocket, with messages still defined in proto.
  A machine's exec is the first, and its SSH tunnel follows it
  (`proto/hangar/compute/v1alpha1/machine_capability.proto`,
  `internal/execws`):
  - `GET /v1/projects/{project}/{plural}/{name}:exec`, such as
    `containers/web:exec`, authenticated as
    any call on the listener is, with a subprotocol naming the messages'
    version (`hangar.exec.v1alpha1`); one proto message per binary
    WebSocket message.
  - Everything that can refuse it does so before the upgrade, as an HTTP
    error whose body is a Connect error's JSON, so clients read it as they
    read any Connect error. After the upgrade, a failure closes the
    WebSocket with 4000 plus the Connect code and the message as its
    reason; a normal close follows the last message.
  - Each end pings the other every 30 s, as `Watch` sends bookmarks, and
    writes a message with no field set, which the other ignores; how
    either tells the other gone is in `node-tunnel.md`.
  - The server checks the handshake's `Origin`, refusing another site's
    with 403. A browser sends the console's session cookie with any
    WebSocket to this host, whichever site opens it, so without the check
    any page its user visits could exec in their containers. It is never
    turned off, nor widened to match a `Host` a proxy rewrote: a proxy in
    front must pass `Host` through.
- Everything else uses Connect unary and server-streaming calls.
- `Watch` sends a `BOOKMARK` every 30 seconds as a heartbeat, so idle streams
  survive proxies and clients advance their revision.
- The server speaks HTTP/1.1 and h2c on the same port, so Connect streaming
  works whichever a client on the host speaks; the front door, hangar-edge,
  forwards to it over HTTP/1.1 and flushes at once (`front-door.md`).

## Consequences

Agents get a CLI (`-o json`, non-interactive, `wait`, defined exit codes) and
generated Go and TypeScript SDKs. An MCP server comes later as `hangar mcp`
over stdio, reusing the CLI's client and login.
