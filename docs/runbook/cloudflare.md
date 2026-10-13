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
- one service token, `hangar-smoke`, for smoke tests only.

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
   | Zone | DNS: Edit | The CNAME |
   | Zone | Zone: Read | Find the zone's ID from its name |

   Account resources: include only this account. Zone resources: include
   only the control zone. Restricting it to the privileged box's IP and giving
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
| `access.env` | `HANGAR_CF_TEAM_DOMAIN=…` and `HANGAR_CF_AUDIENCE=…` (the application's AUD tag) | Merged into the host's `/etc/hangar/local.env` |
| `smoke-token.env` | `HANGAR_CF_CLIENT_ID=…` and `HANGAR_CF_CLIENT_SECRET=…`, written only when the token is created | Wherever smoke tests run; the CLI reads those variables |

Copy files to the host without them passing through a terminal (below).

## Putting it on the host

**Switch the auth mode to `cloudflare` first, then install the tunnel
token. Roll back in reverse.** Until the apiserver runs in `cloudflare` mode
it runs `dev` auth, which takes the caller's identity from a header any
client can set (`X-Hangar-Dev-User`). Once cloudflared connects, the public
hostname reaches that listener: with the origin still in `dev` mode,
anyone whose request gets through (every allowed person and service token,
and everyone if Access is ever misconfigured) could claim any identity,
`platform.root` included, with a header. The ingress's own access check
does not help there: it checks that the request has a valid Access token,
not who the header says it is.

1. **Auth mode.** Add the two lines of `access.env` and
   `HANGAR_AUTH_MODE=cloudflare` to the host's `/etc/hangar/local.env`
   (root, 0600), each key once:

   ```sh
   ssh <host> 'sudo tee -a /etc/hangar/local.env >/dev/null' < ~/hangar-cloudflare/access.env
   ssh <host> 'echo HANGAR_AUTH_MODE=cloudflare | sudo tee -a /etc/hangar/local.env >/dev/null'
   ```

   Re-run `deploy/host`, whose services step restarts the apiserver with
   `--auth-mode cloudflare`, and check with `verify.sh` that the running
   flags match `local.env`. Do not go on until they do.
2. **Tunnel token.** Install it, then re-run `deploy/host`, which installs
   and starts cloudflared once the file exists:

   ```sh
   ssh <host> 'sudo install -m 0600 -o root -g root /dev/stdin /etc/hangar/cloudflared.env' < ~/hangar-cloudflare/cloudflared.env
   ```
3. **Check** from outside: `hangar` through Access works, and so does a
   call with the smoke token's `HANGAR_CF_CLIENT_ID` and
   `HANGAR_CF_CLIENT_SECRET`.

To roll back: first take the tunnel down (remove
`/etc/hangar/cloudflared.env` and re-run `deploy/host`, whose cloudflared
step then stops and disables the service), then set `HANGAR_AUTH_MODE=dev`
in `local.env` and re-run `deploy/host`. The host's side of each step is in
`docs/runbook/host-bootstrap.md`; the order is the one above, and the host
enforces it: cloudflared does not start, by any route, unless the apiserver
runs in `cloudflare` mode.

## When something is off

- **A refusal names an object you know is ours.** `state.json` was lost or
  is from another directory. Restore it. Failing that, write it by hand with
  the IDs from the dashboard (fields `version: 1`, `account_id`, `zone_id`,
  `hostname`, `tunnel_id`, `dns_record_id`, `app_id`, `policies.emails`,
  `policies.service_tokens`, `service_token_id`), then run `plan`.
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
