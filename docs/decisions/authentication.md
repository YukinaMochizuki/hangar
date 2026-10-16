# Authentication

Status: accepted, 2026-10. Revised 2026-10 when Hangar took identity over
from Cloudflare Access.

## Context

Cloudflare is leaving the traffic path (the route from Taiwan to its edge
runs through the US west coast), so Access no longer proves who someone is.
The apiserver signs people in itself. People need a Google account and
nothing else; bindings stay keyed by email. Agents need non-interactive
credentials. The node is a separate root process on the same host.

## Decision

- Each listener has a chain of authenticators, and the first that succeeds
  produces a `Principal` of type User, ServiceToken, Node, System, Local or
  Edge. One that recognizes its credential and finds it invalid refuses the
  request; the chain does not fall through to the next.
- **People sign in through Google**, with the apiserver as the OpenID
  Connect relying party (`internal/auth/login`, go-oidc + oauth2). It runs
  the authorization-code flow with PKCE, a state bound to the browser that
  started the sign-in, and a nonce checked in the ID token. A sign-in in
  progress (state, nonce, PKCE verifier, where to go next) is kept in the
  browser itself, in the short-lived `__Host-hangar_login` cookie, sealed
  with AES-256-GCM, so starting one writes nothing on the server. The key
  is made at start and held in memory only; restarting the apiserver makes
  a new one, which cancels the sign-ins in progress (each person starts
  again) and is how it rotates. Only an `email_verified` email counts, in
  lower case, so bindings by email apply unchanged. Only an invited email,
  one holding a platform role (`tenancy-and-iam.md`), may sign in: anyone
  else sees the not-invited page and gets no session, so strangers write
  nothing. The provider is a flag (`--oidc-issuer`), so others can be
  added as config. There is no email OTP.
- **Hangar issues its own credentials** after a sign-in
  (`internal/auth/session`). Every one is opaque: a prefix that says what
  it is, then 32 random bytes (`internal/auth/token`). Only its SHA-256 is
  stored, in a store table rather than a kind (`credentials`, migration
  `00002`), so it never reaches the revision, the event log or a watch, and
  deleting the row revokes it at once. Every call looks its credential up.
  Prefixes:
  - `hgs_`: a browser session.
  - `hga_`: a CLI access token.
  - `hgf_`: a CLI refresh token.
  - `hgr_`: an agent's service token.
  - `hge_`: a browser's session on one private endpoint's host
    (`public-endpoints.md`), in the login of the console session it was
    handed off from. A credential may name one host, its audience, and then
    counts there alone.
- **Revoking a login marks its family revoked**, in the same transaction
  that deletes its credentials, until the last of them would have expired.
  Issuing checks the mark in the transaction that inserts, so a refresh
  still in flight when its login is revoked (by a replay, a logout or
  `RevokeSessions`) issues nothing that works. A CLI sign-in approved but
  not yet claimed, a loopback code not exchanged or a device login not
  polled, reserves its family when it is approved, recorded under the
  person, so revoking their logins in between ends it too.
- **The console** gets the session in the `__Host-hangar_session` cookie:
  `HttpOnly`, `Secure`, `SameSite=Lax`, for the whole host. It ends 3 days
  after its last use and 14 days after sign-in, whatever its use.
  `POST /auth/logout` ends it.
- **CSRF**: a cookie is ambient, so it counts only on a request the
  console's own origin made. Its `Origin` must be the public origin
  exactly, and its `Sec-Fetch-Site`, when the browser sends one, must be
  `same-origin`. Browsers send `Origin` on every POST and every WebSocket
  handshake, which is all the API takes from a browser (Connect unary calls
  are POSTs; exec and ssh are WebSockets). A form's POST carries the page's
  real origin only if the page's referrer policy allows it: under
  `no-referrer` it is `Origin: null`, which the check refuses. The sign-in
  pages are therefore served with `Referrer-Policy: same-origin`, which
  still sends no referrer to another site, and the console must be too. So a
  cross-site form, fetch or WebSocket gets nothing from the cookie, and
  cross-site WebSocket hijacking of exec and ssh is closed too.
  `SameSite=Lax` is a second layer. There is no CORS. The device approval
  form and the logout are held to the same rule. No GET the cookie
  authenticates changes anything, with two exceptions, named so no other
  slips in (`docs/checklists/review.md`): the WebSocket handshake, which
  carries `Origin` and is held to the rule, and `/auth/endpoint-login`, a
  GET from an endpoint's site by design, which writes a one-time code
  (`public-endpoints.md`). It is safe because the code goes only to that
  endpoint's own callback, and the edge takes it only from the browser
  holding the state cookie it set when that handoff started: whoever sent
  the browser there gets nothing from it, and cannot plant their own code
  in someone else's browser.
- **The CLI** (`hangar login`) holds a 1-hour access token, sent as
  `Authorization: Bearer`, and a refresh token. Each refresh spends the
  refresh token and issues a new pair in the same login, the family.
  Presenting a spent refresh token again revokes the whole family, since
  then two parties hold it. A refresh token ends 30 days after it was
  issued, and the login 90 days after sign-in, however often it is
  refreshed. A refresh also asks whether the person is still invited, and
  ends the login if not. The CLI holds a file lock while it refreshes, so
  two `hangar` processes do not trip the reuse detection.
  - **Loopback** (the default): the CLI listens on `127.0.0.1` on a free
    port and opens `<endpoint>/auth/cli` with its own state and S256 PKCE
    challenge. The apiserver runs the Google sign-in, then redirects to
    the loopback with a one-time code valid for 60 s. The CLI exchanges
    the code, with its verifier and the same redirect, through
    `SessionService.ExchangeCode`. The redirect must be
    `http://127.0.0.1:PORT/callback` or `[::1]` (RFC 8252), never
    `localhost`.
  - **Device** (`hangar login --device`): Hangar's own RFC 8628 flow, so
    Google needs only one web client. The CLI gets a device code and an
    8-letter user code. The person opens `/auth/device` in any browser,
    signs in to the console there, checks the code and approves it. The
    CLI polls (`SessionService.PollDeviceLogin`) and gets its tokens once.
  - The tokens live in `credentials.yaml` next to the config file, never
    in it, mode 0600, written atomically. The CLI refuses a file others may
    read. The default config directory, `…/hangar`, is the CLI's own and is
    kept 0700; a directory `HANGAR_CONFIG` puts it in is left as it is,
    with a warning if others may read it. It never prints a token, in output or
    in an error.
  - `hangar logout` revokes the login; `hangar logout --all` revokes every
    login of the person (`SessionService.RevokeSessions`), which
    `platform.root`, and so root on the host, may also do for anyone.
- **Admission is checked again at every issue**, not only at the Google
  callback: exchanging a loopback code, approving a device login, handing
  out its tokens, and every refresh, so a person uninvited meanwhile gets
  nothing more.
- **Where sign-in is served**: the `/auth/` pages, `/auth/endpoint-login`
  among them, and `SessionService` are on the public listener and on the
  public socket, which is that listener for the front door, and nowhere
  else. The admin socket serves `RevokeSessions` alone, so root on the host
  can end anyone's logins; the node and edge sockets serve none of it.
- **Limits on what anyone can make it keep**: a browser's or a CLI's
  sign-in keeps nothing until the provider has verified someone invited.
  The rows that are kept, device logins started, loopback codes and
  endpoint handoff codes not yet exchanged, are capped at a few thousand of
  each kind (more are refused as unavailable), each kind on its own, so a
  flood of one blocks neither the others nor sign-in. Endpoint codes, which
  any signed-in person can make, are also capped at 20 waiting per person,
  so no one fills the shared cap for everyone else. Each cap is counted
  and the row kept in one store transaction, so requests at once get no
  more than it. A browser session may
  try ten device codes a minute. Per-address rate limits are the front door's
  (hangar-edge).
- **Agents use service tokens** (`hgr_`), issued by the same machinery.
  - A `ServiceToken` (group `iam`) is a project's, and is collected with
    it. It authenticates as `serviceToken:<project>/<name>`, which is also
    the subject a RoleBinding names it by (`tenancy-and-iam.md`). The token
    holds only what such bindings grant, and needs no User.
  - `ServiceTokenIssuerService` issues the secret, returned once:
    `CreateServiceToken` makes the token, with a project RoleBinding for an
    optional role, authorized as the caller so no one grants a token more
    than they could by hand. `RotateServiceToken` issues a new secret.
    Both need the token's apply permission, which only owners and
    `platform.root` hold: `project.editor` reads tokens but writes none.
    A token made by plain apply has no secret until it is rotated.
  - A secret lasts a year unless asked otherwise, between an hour and
    three years. Its status shows its first characters, when it was issued
    and expires, and when it was last used, to within an hour.
  - A token holds one credential at a time, in a family of its own:
    issuing replaces whatever it held in one store transaction
    (`ReplaceCredentials`), so the old secret stops working the moment the
    new one is stored, and of two issued at once one survives.
  - Deleting a token revokes its credential once the delete is stored. An
    issue racing the delete reads the token after storing its credential,
    and revokes it again if the token is being deleted, so no secret
    outlives its token. Deleting the project deletes its tokens the same
    way. The token is gone only once a last revoke, before its finalizer
    is removed, has succeeded.
  - A credential's family starts with its token's uid, and every use asks
    whether a token of that uid exists and is not being deleted. So a
    secret whose revoke failed still authenticates nothing, and never
    passes to a later token of the same name.
  - A service token is `Authorization: Bearer hgr_…` on the public
    listener, in oidc and dev auth alike. The lookup is by the SHA-256
    index, then a constant-time compare. Unknown, expired, rotated and
    deleted tokens are refused alike. The node, edge and admin sockets take
    none.
  - The CLI sends `HANGAR_TOKEN` when it is set, in place of the context's
    credentials, or reads the file a context with `auth: token` names
    (`tokenFile`). It follows symlinks, as orchestrators mount secrets
    through them (Kubernetes' `..data/` layout), and opens the file
    without blocking, so a FIFO cannot hang it. Then it checks the file it
    opened: it must be a regular file whose mode gives the group and others
    nothing, or it is refused. The secret is never in `config.yaml`.
    `hangar token create` and `rotate` print it once, alone on stdout, or
    write it to a new 0600 file with `--secret-file`; nothing else prints
    it.
- Every sign-in page the apiserver, the CLI or hangar-edge shows is a plain
  placeholder in one place, `internal/auth/pages`, for the console's design
  to replace.
- **The node** connects over a unix socket and is identified by `SO_PEERCRED`;
  the socket belongs to one host, so the agent acts as the `Node` named by the
  apiserver's `--node-name`. An mTLS authenticator, carrying the node name in
  the certificate, joins the chain when there is more than one host. The
  tunnel the agent opens on the socket, over which the apiserver calls it,
  is that node by the same authentication (`node-tunnel.md`).
- **Root on the host** connects over a third unix socket, the admin socket
  (`/run/hangar/admin.sock`, root's, 0600), and is identified by
  `SO_PEERCRED` alone: uid 0 is the Local principal `local:root@<node>`,
  any other uid is refused. It holds `platform.root` without a RoleBinding,
  and no RoleBinding can name it, so it is the way back in when every
  binding is wrong or sign-in is broken: Kubernetes' `admin.conf`.
  Nothing else runs on that listener, and its authenticator reads only
  what that listener recorded of the connection, never a header, so no
  other listener can produce the principal. Every request on it is logged
  with the caller's pid. systemd makes the socket (a socket unit) in a
  root-owned `/run/hangar` and passes it to the apiserver, which runs as an
  unprivileged user and could neither make a root-owned socket nor keep one
  in its own runtime directory, which systemd chowns to it and removes on
  stop. The node socket is made the same way. The CLI reaches it as a
  `unix://` endpoint with `local` auth, which sends no credential.
- **The edge** (`hangar-edge`, `public-endpoints.md`) connects over a
  fourth unix socket, the edge socket (`/run/hangar/edge.sock`, root's and
  the edge group's, 0660), and is identified by `SO_PEERCRED` alone: the
  uid of the edge's user (`--edge-uid`, never root) is the Edge principal
  `edge:<host>`, any other is refused. Its authenticator reads only what
  that listener recorded, as the admin socket's does, so no other listener
  can produce it. It may get, list and watch public endpoints, role
  bindings, projects and users, everywhere, and do nothing else, whatever
  the bindings say. Users of public endpoints are not principals of the
  API: the edge decides whether they may use an endpoint. To learn who they
  are, it asks the apiserver on the edge socket, which alone serves it
  `EndpointAuthService`: who holds a credential brought to an endpoint (an
  endpoint session for that host, a CLI access token or a service token),
  and an endpoint session for a one-time code the console's
  `/auth/endpoint-login` issued (`public-endpoints.md`). An mTLS
  authenticator joins its chain with more hosts, as the node's.
- **The public API from the front door** (`front-door.md`) comes in on a
  fifth unix socket, the public socket (`/run/hangar/public.sock`, root's
  and the edge group's, 0660). It is the public listener over again, the
  same handler and chain, sign-in and `SessionService` included, served to
  the edge's uid alone (`SO_PEERCRED`); anyone else is refused before the
  request is read. The edge passes `Host`, `Origin` and the cookies on
  unchanged and ends TLS itself, so the exact-origin check and the
  `__Host-` cookies hold as on the public origin. It vouches for nothing
  but the client's address, which it sends in `Hangar-Client-Ip` and which
  is believed nowhere else; sign-in's audit lines carry it. The apiserver
  refuses the socket in dev auth.
- **Dev mode** takes the user from a header. It must be enabled explicitly
  (`--auth-mode dev`) and refuses to start on a non-loopback address, or
  with a public socket. It has no sign-in pages and issues nothing. It also
  refuses any request
  carrying a header Cloudflare adds (`Cf-Warp-Tag-Id`, which cloudflared
  sets on every request it proxies, and `Cf-Connecting-Ip`, `Cdn-Loop` and
  `Cf-Ray` from its edge), and the host runs cloudflared only while the
  apiserver is in oidc auth, which the tunnel's unit checks itself at
  every start (`docs/runbook/host-bootstrap.md`), so a tunnel never
  reaches it.
- Controllers run in-process as the System principal.
- A third principal source, workload identity issued through the metadata
  service, is reserved for later.

## Consequences

- Tests run the real sign-in against a local fake provider
  (`internal/auth/oidctest`): discovery, keys, PKCE and a signed ID token.
  They cover expiry, revocation, rotation and reuse, a forged state, an
  unverified email and an uninvited one.
- Sessions survive an apiserver restart, since they live in the database,
  and are lost with it: restoring a backup brings back the sessions it
  held.
- The apiserver holds the OAuth client's secret, a file only its owner
  reads (`--oidc-client-secret-file`).
