# Cloudflare: DNS, and the cutover from the tunnel

Cloudflare is the platform's DNS and nothing else: every name points
straight at the host, DNS-only (grey-cloud), and hangar-edge, the front
door, takes the traffic itself (`docs/decisions/front-door.md`). Cloudflare's
DNS API also answers the front door's ACME DNS-01 challenges, with a token
of the edge's own ("The front door's DNS token" below).

`hangar-cloudflare` sets up the records, the way `deploy/host` sets up the
host. It is idempotent and safe to re-run. Through the Cloudflare API it
converges, each pointing at the host's addresses in the config, proxied off,
TTL automatic (300 s), commented `managed by hangar-cloudflare`:

- `api.<zone>`, A and AAAA: the public API;
- `console.<zone>`, A and AAAA: the console, and sign-in;
- with `endpoints.zone` in the config, `*.<endpoints zone>`, A and AAAA:
  every public endpoint (`docs/decisions/public-endpoints.md`).

It changes only what it created. The IDs of those records are in the output
directory's `state.json`. It stops, naming the record's ID, before it
changes anything, when a record not in the state answers for one of its
names: an A, AAAA or CNAME there. Records of other types at those names are
not its business. It never deletes anything.

The zones cannot be changed: the state records them, and a config naming
another is refused. To move, remove the old records by hand and start
again, or run with a new `--out` directory.

The code is `cmd/hangar-cloudflare` and `internal/cloudflare`.

## One-time steps (a human, in the dashboards)

1. **Zones.** Buy the domains and add each to the Cloudflare account
   (Add a domain, Free plan), then set the registrar's nameservers to the ones
   Cloudflare shows. Wait until the zone is Active. The API and the console
   live in the control zone, endpoints in the user-content zone.
2. **API token.** My Profile → API Tokens → Create Token → Custom token.
   Permissions: Zone → Zone → Read (to find the zones' IDs from their
   names) and Zone → DNS → Edit (the records). Zone resources: Include →
   Specific zone, the control zone and the user-content zone, nothing
   else. Restricting it to the privileged box's IP and giving it an expiry
   are good practice. It needs no account permission.

## Config

Copy `deploy/cloudflare/example.yaml` to a file outside the repo, in the
privileged box, and fill it in: the account, the control zone, the host's
public IPv4 and IPv6 addresses (the host env file's `HANGAR_PUBLIC_IPV4`,
and `HANGAR_IPV6_ADDR` without its length), and the user-content zone.
Unknown keys are errors, so a config written for the tunnel and Access
(`hostname`, `tunnel_name`, `allowed_emails`, `identity_providers`) is
refused: keep it, for a rollback, but write the new one apart. The API
token is never in the file.

## Running it

From the repo, in the privileged box, with the state in place
(`<out>/state.json`; production keeps it in another repository, which says
where). Read the token without echoing it, so it stays out of the shell
history:

```sh
read -rs CLOUDFLARE_API_TOKEN && export CLOUDFLARE_API_TOKEN
go run ./cmd/hangar-cloudflare plan   --config ~/cloudflare.yaml --out ~/hangar-cloudflare
go run ./cmd/hangar-cloudflare apply  --config ~/cloudflare.yaml --out ~/hangar-cloudflare
go run ./cmd/hangar-cloudflare verify --config ~/cloudflare.yaml --out ~/hangar-cloudflare
```

- `plan` prints what `apply` would create, update or write, and changes
  nothing: its client refuses any request but GET, and it writes no file.
- `apply` converges and prints what it changed. A second `apply` prints
  `No changes.` Copy `state.json` back to where it is kept, and commit it,
  whenever it changed.
- `verify` is `plan` with an exit code: 0 when nothing differs, 3 on drift,
  1 on an error (including a refusal), 2 on a usage error. It reports a
  record's type, name, content, proxied flag, TTL or comment changed, a
  managed record deleted, and a state file still in the old format.

`plan` and `verify` also print notes, which are not drift: the IDs of what
an earlier version made for the tunnel and Access (the tunnel, the Access
application and policies, the smoke service token), kept in the state's
`retired` entry, for someone to delete by hand once the cutover is settled
(below).

Requests that get a 429 or a 5xx are retried with backoff (honouring
`Retry-After`), except a create that got a 5xx, which may have succeeded; the
next run then finds the duplicate and refuses it, naming its ID. Each request
times out after 30 s and the whole run after `--timeout` (5m).

## The output directory and the state

The output directory is created mode 0700, every file in it 0600. An
existing directory that group or others can access is refused, before any
API call, with the `chmod` that fixes it: an earlier version kept the
tunnel's token there, and a rollback runs that version again.

`state.json` holds IDs alone, nothing secret, but it is what proves
ownership: keep it. This version writes `version: 2`: the records by name
and type, under `records`, the zones, and `retired`. It reads a
`version: 1` state, the tunnel's, as well: the API's CNAME becomes the
API's A record, and the endpoints' wildcard CNAME theirs, each changed in
place (one record, one ID), so neither name is ever without an answer; the
tunnel and Access objects go under `retired`. `plan` shows that as
`> write state.json`, and `apply` writes it. The previous release's tool
reads only `version: 1`: keep the old file (the other repository's history
has it) for a rollback.

## The front door's DNS token

The front door (`docs/decisions/front-door.md`) gets its certificates over
ACME DNS-01: `hangar-edge` writes the challenge's TXT record in the zone
through Cloudflare's DNS API, with a token of its own, apart from the
tool's. Its ACME account also needs an email, `HANGAR_ACME_EMAIL` in
`/etc/hangar/local.env` (`docs/runbook/host-bootstrap.md`): without one,
certmagic's process-wide email discovery can register the account as
`mailto:default` on a first start, which the CA refuses, so the `edge` step
refuses the front door. Make the token in My Profile → API Tokens → Create
Token → Custom token:

- Permissions: Zone → Zone → Read, and Zone → DNS → Edit.
- Zone resources: Include → Specific zone, the control plane's zone
  (`hangarlab.cloud`) and the user-content zone (`hangarlab.dev`), nothing
  else.
- Client IP address filtering: the host's IPv4 and IPv6 addresses.

Install it as root's, 0600, without it passing through a terminal. From a
box that holds it in its environment:

```sh
printenv HANGAR_EDGE_DNS_TOKEN | ssh <host> 'sudo install -m 0600 -o root -g root /dev/stdin /etc/hangar/cloudflare-dns-token'
```

or, typed: `read -rs T && printf '%s' "$T" | ssh <host> 'sudo install -m
0600 -o root -g root /dev/stdin /etc/hangar/cloudflare-dns-token'; unset T`.
The edge reads the file trimmed, so `printenv`'s newline does no harm.

systemd hands it to the edge as a credential; nothing else on the host
reads it. To roll it, make a new one, install it the same way, run
`sudo systemctl restart hangar-edge`, then delete the old one in the
dashboard. Then follow "The front door" in `docs/runbook/host-bootstrap.md`.

## The cutover from the tunnel

Before it, the public API ran behind a Cloudflare Tunnel and Access: a
proxied CNAME `api.<zone>` → `<tunnel id>.cfargotunnel.com`, cloudflared on
the host, an Access application, two reusable policies and the
`hangar-smoke` service token, all made by the earlier version of this tool,
and the apiserver in `cloudflare` auth. After it, DNS points at the host,
the front door serves `api.`, `console.` and the endpoints, and the
apiserver signs people in itself (`docs/decisions/authentication.md`). It is
one switch: there is no time with both paths in use.

### What the owner provides, and where it lands

| Value | Where it lands | Notes |
|---|---|---|
| Google OAuth client, type Web application, redirect URI `https://console.<cp zone>/auth/callback`, consent screen External and in production | Its ID: `HANGAR_OIDC_CLIENT_ID` in `/etc/hangar/local.env`. Its secret: `/var/lib/hangar/state/oidc-client-secret`, `hangar`'s, 0600 | No JavaScript origin is needed; the CLI and the console share the client |
| The front door's DNS token (above) | `/etc/hangar/cloudflare-dns-token`, root's, 0600 | Zone Read and DNS Edit on both zones, the host's addresses only |
| The ACME account's email | `HANGAR_ACME_EMAIL` in `local.env` | Let's Encrypt writes to it about the certificates |
| The Google account the owner signs in with | `HANGAR_BOOTSTRAP_ADMIN` in `local.env` | Sign-in admits only invited emails; it was the Access identity before |
| The tool's API token gains Zone Read and DNS Edit on the user-content zone | The privileged box's `CLOUDFLARE_API_TOKEN` | Keep its account permissions until a rollback is off the table: the previous release's tool needs them |
| `state.json` | `<out>/state.json` in the privileged box, from where it is kept | Commit it back after each apply that changed it |

Not values, but settled before the day: a release built from the commit to
deploy, with its `SHA256SUMS` digest ("Deploying a new hangar version" in
`docs/runbook/host-bootstrap.md`); the previous release's directory and its
`deploy/` kept on the host, as `/var/tmp/hangar/release/hangar-<previous>`
and `/var/tmp/hangar/deploy-previous`; the new config file in the
privileged box, beside the old one; a CLI built from the new release on
the owner's workstation, with the previous one kept.

### Order

From step 4 until DNS points at the host (step 6) and old answers have
expired, **only the admin socket on the host works**: the tunnel still
carries `api.<zone>`, but Access tokens no longer count with the apiserver
in oidc auth, and signing in needs `console.<zone>`, which has no record
yet. Plan for about a quarter of an hour, most of it waiting for
certificates and for DNS caches. VMs and containers keep running
throughout.

1. **Back up the host's settings.**

   ```sh
   sudo cp -a /etc/hangar/local.env /etc/hangar/local.env.pre-cutover
   ```

   The store is snapshotted at step 4, right before the deploy, since the
   platform keeps taking writes until then.
2. **Plan the DNS, changing nothing.** In the privileged box, with the
   state copied in and the new config: `plan`. Expect `> write
   state.json`, `~ update DNS A record api.<zone>` with `type: CNAME -> A`,
   `content: <tunnel id>.cfargotunnel.com -> <host IPv4>` and `proxied:
   true -> false`, then `+ create` for the API's AAAA, the console's A and
   AAAA, and the wildcard's A and AAAA, and notes naming the retired
   tunnel and Access objects. Anything else: stop and find out why.
3. **Install the secrets and the settings on the host.** From the box that
   holds the values in its environment, without them passing through a
   terminal:

   ```sh
   printenv HANGAR_OIDC_CLIENT_SECRET | ssh <host> 'sudo install -o hangar -g hangar -m 0600 /dev/stdin /var/lib/hangar/state/oidc-client-secret'
   printenv HANGAR_EDGE_DNS_TOKEN | ssh <host> 'sudo install -o root -g root -m 0600 /dev/stdin /etc/hangar/cloudflare-dns-token'
   ```

   In `/etc/hangar/local.env`, delete `HANGAR_AUTH_MODE=cloudflare`,
   `HANGAR_CF_TEAM_DOMAIN`, `HANGAR_CF_AUDIENCE` and any other `HANGAR_CF_*`
   or `HANGAR_EDGE_CF_*` line, check `HANGAR_BOOTSTRAP_ADMIN`, and add:

   ```sh
   HANGAR_AUTH_MODE=oidc
   HANGAR_OIDC_CLIENT_ID=<the client ID>
   HANGAR_PUBLIC_URL=https://console.hangarlab.cloud
   HANGAR_EDGE_DOMAIN=hangarlab.dev
   HANGAR_CP_DOMAIN=hangarlab.cloud
   HANGAR_ACME_EMAIL=<the owner's address>
   ```

   Check: `sudo stat -c '%U %G %a %s' /var/lib/hangar/state/oidc-client-secret
   /etc/hangar/cloudflare-dns-token` says `hangar hangar 600` and `root root
   600`, sizes not 0. Never `cat` them.
4. **Snapshot the store, then deploy the release.** First, so the
   snapshot holds every write the old release took:

   ```sh
   sudo zfs snapshot hangar/state@pre-cutover
   zfs list -t snapshot hangar/state
   ```

   The listing must end with `hangar/state@pre-cutover`: a rollback can
   only go back to the latest snapshot without destroying the ones after
   it. Then deploy, as "Deploying a new hangar version" in
   `docs/runbook/host-bootstrap.md` says, with `deploy/` and the release
   from the commit. The dry run lists the new units and `would: restart`
   for the apiserver, the agent (its unit changed) and the edge's start;
   the real run warns that the tunnel is left while `cloudflared.env` is
   there, and, the first time, to copy the secrets key-encryption key to
   the vault: do that now. The window above starts here.

   Check: `sudo hangar --context local get nodes` shows the node Ready;
   `systemctl list-units --no-pager 'hangar-*'` shows the edge, its two
   sockets and the apiserver's public socket active; `verify.sh` passes
   but for the front door's certificates, which come next.
5. **The certificates.** `journalctl -u hangar-edge | grep 'certificate
   obtained'` shows one for each of `api.`, `console.` and `*.<user zone>`
   within a minute or two.

   Check: `sudo deploy/host/verify.sh` passes, its `edge on :443 serves …`
   lines included. From outside the host, before DNS:
   `curl --resolve api.hangarlab.cloud:443:15.235.221.177 https://api.hangarlab.cloud/healthz`
   prints `ok`, and the same over `[2402:1f00:8001:27b1::1]`.
6. **Switch DNS.** In the privileged box: `apply`, then `verify` (exit 0),
   then commit `state.json` where it is kept.

   Check: `dig +short api.hangarlab.cloud A @1.1.1.1` is the host's IPv4,
   `AAAA` its IPv6, and so for `console.hangarlab.cloud` and
   `anything.hangarlab.dev`; then `curl https://api.hangarlab.cloud/healthz`
   prints `ok` with no `--resolve`. Wait five minutes, the old record's TTL,
   before the next step, so no cache still sends anyone to the tunnel.

   **If the API refuses the type change**, `apply` stops at `DNS A record
   api.<zone>` with that record unchanged, still the tunnel's CNAME, and
   says so. Delete that CNAME in the dashboard (its ID is the state's
   `api.<zone> A`), then `apply` again, which creates the A record. Between
   the two the name has no answer, and a resolver that asks then may keep
   that for up to the zone's negative TTL (the SOA minimum, 30 minutes on
   Cloudflare): do it quickly.
7. **Stop cloudflared.** Move the tunnel's token aside, for a rollback,
   then a full run retires the tunnel: it stops and disables
   `hangar-cloudflared` and removes its unit, its guard and the binary.

   ```sh
   sudo mv /etc/hangar/cloudflared.env /etc/hangar/cloudflared.env.retired
   sudo systemd-run --unit=hangar-bootstrap --wait --collect /var/tmp/hangar/deploy/host/bootstrap.sh
   sudo journalctl -u hangar-bootstrap --no-pager | grep 'retire the Cloudflare tunnel'
   sudo /var/tmp/hangar/deploy/host/verify.sh
   sudo /var/tmp/hangar/deploy/host/bootstrap.sh
   ```

   Check: `verify.sh` says `PASS Cloudflare tunnel retired: nothing of it
   left`, and the next run `changes: 0`.
8. **The owner signs in again**, on the workstation, with the new CLI:
   `hangar config set-context prod --endpoint https://api.hangarlab.cloud
   --auth oidc`, then `hangar login` (or `hangar login --device`), then
   `hangar whoami`. A person who never signed up runs `hangar signup
   --username NAME` once. `https://console.hangarlab.cloud/auth/login`
   signs in a browser; the console's own pages answer 503 until a release
   carries its files.
9. **Recreate the smoke token**, as "The smoke token" in
   `docs/runbook/host-bootstrap.md` says, and delete the binding the
   Access-era token held (`smoke-token-owner`).

   Check: `verify.sh` says `PASS smoke token …`.

### Afterwards

Once rolling back is off the table (a week, say):

- Delete in the dashboard what `plan`'s notes name: the tunnel (Zero Trust
  → Networks → Tunnels), the Access application, its two policies and the
  `hangar-smoke` service token; and, on a host that ran public endpoints
  behind Access, what "Retiring the endpoints' Access objects" below says.
  Then remove the `retired` entry from `state.json`, `plan` again (no
  notes), and commit it.
- Revoke the apiserver's old Cloudflare API token, if there was one, and
  delete `/var/lib/hangar/state/cloudflare-api-token` on the host;
  `deploy/host` warns while it is there.
- Cut the tool's token down to Zone Read and DNS Edit on the two zones.
- On the host: `sudo zfs destroy hangar/state@pre-cutover` and
  `sudo rm /etc/hangar/local.env.pre-cutover /etc/hangar/cloudflared.env.retired`.

### Rollback

At any step, back to the previous release and DNS through the tunnel. The
host goes first, so the tunnel is up when DNS comes back to it. Everything
the platform stored since the snapshot of step 4 is lost with it: sign-ins
and anything applied in between. Before step 4 nothing has changed but
`local.env` and two new files: put `local.env.pre-cutover` back, and that
is all.

1. **The host.** Stop the new release's front door and apiserver, put the
   settings and the store back, and run the previous release's `deploy/`:

   ```sh
   sudo systemctl disable --now hangar-edge.service hangar-edge-https.socket hangar-edge-http.socket
   sudo systemctl stop hangar-apiserver
   sudo cp -a /etc/hangar/local.env.pre-cutover /etc/hangar/local.env
   sudo zfs rollback hangar/state@pre-cutover
   sudo systemd-run --unit=hangar-bootstrap --wait --collect /var/tmp/hangar/deploy-previous/host/bootstrap.sh --release /var/tmp/hangar/release/hangar-<previous>
   ```

   `zfs rollback` goes back to the latest snapshot only: it refuses while
   a later one exists (`zfs list -t snapshot hangar/state` shows them,
   oldest first). `zfs rollback -r` then destroys every snapshot after
   `@pre-cutover` on the way; do not use it until you know what they hold.

   After step 7, put the tunnel's token back first, before that run:
   `sudo mv /etc/hangar/cloudflared.env.retired /etc/hangar/cloudflared.env`.
   The previous release's `deploy/` reinstalls cloudflared from it. The
   firewall and `/etc/hangar/cloudflare-dns-token` stay: the firewall lets
   in all an earlier release needs, and nothing reads the token.

   Check: `journalctl -u hangar-cloudflared` shows `Registered tunnel
   connection`; `cat /usr/local/lib/hangar/VERSION` is the previous
   revision; `verify.sh` from `deploy-previous` passes.
2. **DNS.** In the dashboard, delete the records this version created: the
   AAAA of `api.<zone>`, and the A and AAAA of `console.<zone>` and of
   `*.<user zone>`. Then, in the privileged box, check out the previous
   release, put the `version: 1` `state.json` from before the cutover in
   an output directory, and run its tool's `apply` with the old config: it
   turns `api.<zone>`'s A record, the same record, back into the proxied
   CNAME to the tunnel. Keep that `state.json` as the current one.

   Check: `dig +short api.hangarlab.cloud @1.1.1.1` answers Cloudflare's
   addresses again, and the previous CLI (`hangar.old`) signs in through
   Access.

## Retiring the endpoints' Access objects

Before hangar-edge signed private endpoints' callers in itself, the tool
made a wildcard Access application for `*.<zone>` and a reusable bypass
policy, and the apiserver made one application per PUBLIC endpoint, named
`hangar public endpoint: <hostname>`, with an API token of its own. None of
them is used any more, and the tool deletes nothing, so delete them by
hand, once the new release runs (before, the old release's edge still needs
them: its private endpoints would refuse everyone, and Access would ask its
public ones' visitors to sign in):

1. `plan` names the wildcard application's and the bypass policy's IDs in
   its notes (`endpoints_access_app` and `access_policy_endpoints_bypass`
   under `retired` in `state.json`).
2. In the dashboard (Zero Trust → Access → Applications), delete every
   application named `hangar public endpoint: …`, then the wildcard
   application, then (Access → Policies) the bypass policy.
3. Revoke the apiserver's Cloudflare API token (My Profile → API Tokens),
   and on the host delete `/var/lib/hangar/state/cloudflare-api-token`;
   `deploy/host` warns while the file is there.
4. Remove those entries from `state.json`, then `plan` again: no notes
   about them.

A host whose state names no endpoints' zone never had them.

## When something is off

- **A refusal names a record you know is ours.** `state.json` was lost or
  is from another directory. Restore it. Failing that, write it by hand
  with the IDs from the dashboard (fields `version: 2`, `account_id`,
  `zone`, `zone_id`, `endpoints_zone`, `endpoints_zone_id`, and `records`
  mapping `"<name> <type>"` to the record's ID), then run `plan`.
- **verify reports `proxied: true -> false`.** Someone turned the proxy on
  in the dashboard: traffic then goes through Cloudflare again, which the
  front door does not expect (its client addresses become Cloudflare's).
  `apply` turns it off.
- **Removing everything.** The tool deletes nothing. Delete the records in
  the dashboard, then `state.json`. The names then stop answering: the
  host serves nothing anyone can find.
