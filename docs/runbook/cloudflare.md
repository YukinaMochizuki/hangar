# Cloudflare: tunnel, DNS and Access

`hangar-cloudflare` sets up Cloudflare's side of the public API, the way
`deploy/host` sets up the host. It is idempotent and safe to re-run. Through
the Cloudflare API it converges:

- a remotely-managed tunnel (`config_src: cloudflare`) whose ingress sends
  `<hostname>` to `http://127.0.0.1:8080` and everything else to
  `http_status:404`; the hostname's rule sets `originRequest.access`
  (`required: true`, the team name, the application's AUD), so cloudflared
  itself drops any request without a valid Access token for the application;
- a proxied CNAME `<hostname>` → `<tunnel id>.cfargotunnel.com`, commented
  `managed by hangar-cloudflare`;
- two reusable Access policies, `<hostname>: allow listed emails` (allow,
  the config's emails) and `<hostname>: service tokens` (`non_identity`, any
  valid service token);
- a self-hosted Access application for `<hostname>`: session 24h, the Google
  and One-time PIN login methods, no auto-redirect, and those two policies;
- one service token, `hangar-smoke`, for smoke tests only;
- with `endpoints.zone` in the config, public endpoints' side
  (`docs/decisions/public-endpoints.md`): an ingress rule sending
  `*.<zone>` to the edge, `http://127.0.0.1:8090`, with no
  `originRequest.access` (the edge signs a private endpoint's callers in
  itself, and a public one takes anyone), and a proxied CNAME `*.<zone>` to
  the tunnel in that zone. No Access object stands in front of an
  endpoint.

It changes only what it created. The IDs of those objects are in the output
directory's `state.json`. It stops, naming the object's ID, before it changes
anything, when an object not in the state collides with one of its own: a
tunnel or policy of the same name, a service token called `hangar-smoke`, a
DNS record at the hostname, or an Access application that covers the
hostname or a path under it (through its domain, a destination, or a legacy
self-hosted domain). It never deletes anything.

On a first apply it creates the policies and the application first, then
the tunnel and its ingress, and the DNS record last, so the hostname is never
routed before Access covers it.

The hostname cannot be changed: the state records it, and a config naming
another is refused. To move, remove the old setup by hand (see the end of
this file) and start again, or run with a new `--out` directory.

The code is `cmd/hangar-cloudflare` and `internal/cloudflare`.

## One-time steps (a human, in the dashboards)

1. **Zones.** Buy the domains and add each to the Cloudflare account
   (Add a domain, Free plan), then set the registrar's nameservers to the ones
   Cloudflare shows. Wait until the zone is Active. The API hostname lives in
   the control zone.
2. **Zero Trust.** Open Zero Trust in the dashboard, pick the team name, and
   choose the Free plan. The team domain is
   `https://<team>.cloudflareaccess.com`; the tool reads it from the API, so
   it is never typed into the config.
3. **Google OAuth client.** In the Google Cloud console, in a project for
   this, configure the OAuth consent screen (user type External, in
   production), then create an OAuth client ID:
   - Application type: Web application.
   - Authorized JavaScript origins: `https://<team>.cloudflareaccess.com`.
   - Authorized redirect URIs:
     `https://<team>.cloudflareaccess.com/cdn-cgi/access/callback`.

   Keep the client ID and secret for the next step only.
4. **Login methods.** Zero Trust → Settings → Authentication → Login methods
   → Add new → Google. Paste the client ID and secret, name it (the name goes
   in the config's `identity_providers.google`), save, and use Test. Check
   that One-time PIN is in the list too; add it the same way if not.
5. **API token.** My Profile → API Tokens → Create Token → Custom token.
   Permissions:

   | Scope | Permission | Used for |
   |---|---|---|
   | Account | Cloudflare Tunnel: Edit | Create the tunnel, set its ingress, and read its token (the token endpoint needs Edit) |
   | Account | Access: Apps and Policies: Edit | The application and its reusable policies |
   | Account | Access: Service Tokens: Edit | Create `hangar-smoke` |
   | Account | Access: Organizations, Identity Providers, and Groups: Read | The team domain and the login methods' IDs |
   | Zone | DNS: Edit | The CNAMEs |
   | Zone | Zone: Read | Find the zones' IDs from their names |

   Account resources: include only this account. Zone resources: include
   only the control zone, and the public endpoints' zone if the config
   names one. Restricting it to the privileged box's IP and giving
   it an expiry are good practice. Newer dashboards may list the tunnel
   permission as Cloudflare One Connector: cloudflared, which the API accepts
   in its place.

## Config

Copy `deploy/cloudflare/example.yaml` to a file outside the repo, in the
privileged box, and fill it in. Unknown keys are errors. The API token is
never in the file.

## Running it

From the repo, in the privileged box. Read the token without echoing it, so
it stays out of the shell history:

```sh
read -rs CLOUDFLARE_API_TOKEN && export CLOUDFLARE_API_TOKEN
go run ./cmd/hangar-cloudflare plan   --config ~/cloudflare.yaml --out ~/hangar-cloudflare
go run ./cmd/hangar-cloudflare apply  --config ~/cloudflare.yaml --out ~/hangar-cloudflare
go run ./cmd/hangar-cloudflare verify --config ~/cloudflare.yaml --out ~/hangar-cloudflare
```

- `plan` prints what `apply` would create, update or write, and changes
  nothing: its client refuses any request but GET, and it writes no file.
- `apply` converges and prints what it changed. A second `apply` prints
  `No changes.`
- `verify` is `plan` with an exit code: 0 when nothing differs, 3 on drift,
  1 on an error (including a refusal), 2 on a usage error. It reports:
  - the tunnel: an ingress rule changed, added or removed, including its
    `originRequest` (the access check removed, not required, another AUD or
    team), and a top-level `originRequest` set;
  - the DNS record: its type, name, content, proxied flag or comment;
  - a policy: a rule added, removed or changed (include, exclude, require),
    its decision or its session;
  - the application: its name, domain, session, login methods,
    auto-redirect, destinations, self-hosted domains or attached policies,
    and any setting that lets a request in with less than a full login or
    weakens its cookie (`options_preflight_bypass`,
    `allow_authenticate_via_warp`, `cors_headers`, `allow_iframe`,
    `use_clientless_isolation_app_launcher_url`,
    `http_only_cookie_attribute` off, `same_site_cookie_attribute` none);
  - any managed object deleted, and an output file missing or stale.

  A tunnel with `warp-routing` enabled is a note, not drift: the field is
  read-only (assigning the tunnel a private network route turns it on), so
  `apply` could not change it.

Requests that get a 429 or a 5xx are retried with backoff (honouring
`Retry-After`), except a create that got a 5xx, which may have succeeded; the
next run then finds the duplicate and refuses it, naming its ID. Each request
times out after 30 s and the whole run after `--timeout` (5m).

## The output directory

Created mode 0700, every file in it 0600. An existing directory that group
or others can access is refused, before any API call, with the `chmod` that
fixes it. Nothing secret is ever printed.

| File | Holds | Goes to |
|---|---|---|
| `state.json` | The IDs of the objects the tool created. Not secret, but it is what proves ownership: keep it | Stays here |
| `cloudflared.env` | `TUNNEL_TOKEN=…` | The host's `/etc/hangar/cloudflared.env`, root 0600 |
| `access.env` | `HANGAR_CF_TEAM_DOMAIN=…` and `HANGAR_CF_AUDIENCE=…` (the application's AUD tag) | Nowhere: neither the apiserver nor hangar-edge reads either any more |
| `smoke-token.env` | `HANGAR_CF_CLIENT_ID=…` and `HANGAR_CF_CLIENT_SECRET=…`, written only when the token is created | Nowhere: the CLI no longer reads those variables, and the API no longer takes Access service tokens. Smoke tests use the Hangar service token `smoke/smoke` instead ("The smoke token" in `docs/runbook/host-bootstrap.md`) |
| `edge.env` | With `endpoints.zone`: `HANGAR_EDGE_DOMAIN`. Not secret | Merged into the host's `/etc/hangar/local.env` (below) |

Copy files to the host without them passing through a terminal (below).

## Putting it on the host

**Switch the auth mode to `oidc` first, then install the tunnel token. Roll
back in reverse.** Until the apiserver runs in `oidc` mode it runs `dev`
auth, which takes the caller's identity from a header any client can set
(`X-Hangar-Dev-User`). Once cloudflared connects, the public hostname
reaches that listener: with the origin still in `dev` mode, anyone whose
request gets through could claim any identity, `platform.root` included,
with a header.

1. **Auth mode.** Set up sign-in through Google on the host
   (`docs/runbook/host-bootstrap.md`, "Switching to sign-in through
   Google"): the OAuth client's secret, then `HANGAR_AUTH_MODE=oidc`,
   `HANGAR_OIDC_CLIENT_ID` and `HANGAR_PUBLIC_URL` in
   `/etc/hangar/local.env`. Re-run `deploy/host`, whose services step
   restarts the apiserver with `--auth-mode oidc`, and check with
   `verify.sh` that the running flags match `local.env`. Do not go on until
   they do.
2. **Tunnel token.** Install it, then re-run `deploy/host`, which installs
   and starts cloudflared once the file exists:

   ```sh
   ssh <host> 'sudo install -m 0600 -o root -g root /dev/stdin /etc/hangar/cloudflared.env' < ~/hangar-cloudflare/cloudflared.env
   ```
3. **Check** from outside: `hangar login`, then `hangar whoami`.

To roll back: first take the tunnel down (remove
`/etc/hangar/cloudflared.env` and re-run `deploy/host`, whose cloudflared
step then stops and disables the service), then set `HANGAR_AUTH_MODE=dev`
in `local.env` and re-run `deploy/host`. The host's side of each step is in
`docs/runbook/host-bootstrap.md`; the order is the one above, and the host
enforces it: cloudflared does not start, by any route, unless the apiserver
runs in `oidc` mode.

## Public endpoints

Public endpoints need the API in `oidc` auth first (above): the console
signs a private endpoint's callers in, and hands them off to the edge
(`docs/decisions/public-endpoints.md`), which sends them to
`HANGAR_PUBLIC_URL`.

1. **The zone.** Add the user-content domain (`hangarlab.dev`) to the
   account as the first one-time step says, and give the tool's token DNS
   Edit and Zone Read on it too.
2. **Converge.** Add `endpoints: {zone: hangarlab.dev}` to the config,
   `plan`, then `apply`. The ingress comes before the record, so no
   endpoint's hostname is routed before the tunnel knows where to send it.
   The zone cannot be changed or removed afterwards, as the hostname
   cannot.
3. **The host.** Add `edge.env` to `/etc/hangar/local.env` and re-run
   `deploy/host`, which restarts the apiserver with the endpoint flags and
   starts `hangar-edge` (`docs/runbook/host-bootstrap.md`). A host that
   had them still has `HANGAR_EDGE_CF_AUDIENCE`, `HANGAR_CF_ACCOUNT_ID` and
   `HANGAR_CF_BYPASS_POLICY_ID` in `local.env`: nothing reads them, delete
   them.

   ```sh
   ssh <host> 'sudo tee -a /etc/hangar/local.env >/dev/null' < ~/hangar-cloudflare/edge.env
   ```
4. **Check.** Make a container with a public port, or a PublicEndpoint,
   wait for `Ready`, and open its URL: a PRIVATE one sends you to the
   console to sign in, and then answers; a PUBLIC one answers anyone. A
   hostname of no endpoint is 404.

To take endpoints down: remove `HANGAR_EDGE_DOMAIN` from `local.env` and
re-run `deploy/host`, which stops the edge, so every endpoint's hostname
answers 502. Removing them from Cloudflare is a human's job in the
dashboard, as for the rest: the wildcard record.

### Retiring the endpoints' Access objects

Before hangar-edge signed private endpoints' callers in itself, the tool
made a wildcard Access application for `*.<zone>` and a reusable bypass
policy, and the apiserver made one application per PUBLIC endpoint, named
`hangar public endpoint: <hostname>`, with an API token of its own. None of
them is used any more, and the tool deletes nothing, so at the cutover
delete them by hand, once the new release runs (before, the old release's
edge still needs them: its private endpoints would refuse everyone, and
Access would ask its public ones' visitors to sign in):

1. `plan` names the wildcard application's and the bypass policy's IDs in
   its notes (`endpoints_app_id` and `policies.endpoints_bypass` in
   `state.json`).
2. In the dashboard (Zero Trust → Access → Applications), delete every
   application named `hangar public endpoint: …`, then the wildcard
   application, then (Access → Policies) the bypass policy.
3. Revoke the apiserver's Cloudflare API token (My Profile → API Tokens),
   and on the host delete `/var/lib/hangar/state/cloudflare-api-token`;
   `deploy/host` warns while the file is there.
4. Remove `endpoints_app_id` and `policies.endpoints_bypass` from
   `state.json`, then `plan` again: no notes about them.

## The front door's DNS token

The front door (`docs/decisions/front-door.md`) gets its certificates over
ACME DNS-01: `hangar-edge` writes the challenge's TXT record in the zone
through Cloudflare's DNS API, with a token of its own, apart from the
tool's and the apiserver's. Its ACME account also needs an email,
`HANGAR_ACME_EMAIL` in `/etc/hangar/local.env`
(`docs/runbook/host-bootstrap.md`): without one, certmagic's process-wide
email discovery can register the account as `mailto:default` on a first
start, which the CA refuses, so the `edge` step refuses the front door.
Make the token in My Profile → API Tokens → Create Token → Custom token:

- Permissions: Zone → Zone → Read, and Zone → DNS → Edit.
- Zone resources: Include → Specific zone, the control plane's zone
  (`hangarlab.cloud`) and the user-content zone (`hangarlab.dev`), nothing
  else.
- Client IP address filtering: the host's IPv4 and IPv6 addresses.

Install it as root's, 0600, without it passing through a terminal:

```sh
read -rs T && printf '%s' "$T" | ssh <host> 'sudo install -m 0600 -o root -g root /dev/stdin /etc/hangar/cloudflare-dns-token'; unset T
```

systemd hands it to the edge as a credential; nothing else on the host
reads it. To roll it, make a new one, install it the same way, run
`sudo systemctl restart hangar-edge`, then delete the old one in the
dashboard. Then follow "The front door" in `docs/runbook/host-bootstrap.md`.

## When something is off

- **The tunnel is down after the apiserver was stopped and started by
  hand.** The tunnel's unit stops with the apiserver and does not start
  with it again; on the host, `sudo systemctl start hangar-cloudflared` or
  re-run `deploy/host` (`docs/runbook/host-bootstrap.md`).
- **A refusal names an object you know is ours.** `state.json` was lost or
  is from another directory. Restore it. Failing that, write it by hand with
  the IDs from the dashboard (fields `version: 1`, `account_id`, `zone_id`,
  `hostname`, `tunnel_id`, `dns_record_id`, `app_id`, `policies.emails`,
  `policies.service_tokens`, `service_token_id`, and with public endpoints
  `endpoints_zone`, `endpoints_zone_id`, `endpoints_dns_record_id`), then
  run `plan`.
- **`note: … smoke-token.env is not in the output directory`.** Cloudflare
  returns a client secret only when the token is created. Delete
  `hangar-smoke` in the dashboard (Zero Trust → Access → Service auth) and
  run `apply`, which creates a new one and writes the file.
- **`note: … expires at …`.** The smoke token lasts a year. Delete it and
  `apply`, as above.
- **Removing everything.** The tool deletes nothing. Take the host off the
  tunnel first (the rollback above), then in the dashboard delete the DNS
  record, the tunnel, the application, the two policies and the service
  token, in that order, so the hostname is never routed without Access in
  front. Then delete `state.json`.
