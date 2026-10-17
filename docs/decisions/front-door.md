# The front door: hangar-edge on :443, with certificates of its own

Status: accepted, 2026-10.

## Context

HiNet routes traffic to Cloudflare through the US west coast, so the
project owner took Cloudflare out of the traffic path. Everything now
connects straight to the host: the API, the console and the public
endpoints. Cloudflare stays only as DNS-only (grey-cloud) DNS, and its DNS
API answers ACME's DNS-01 challenges. The host has one IPv4 address and an
IPv6 prefix, and something on it must take :443 on both, end TLS, and
stand in front of the internet with nothing else in front of it.

## Decision

- **hangar-edge is the only listener on :443 and :80** (decided by the
  owner). It is never root and holds no capability. systemd binds both ports
  in two socket units, `hangar-edge-https.socket` and
  `hangar-edge-http.socket`, each one `[::]` socket with
  `BindIPv6Only=both`, so IPv4 and IPv6 come in on it. systemd passes the
  sockets to the edge by name (`FileDescriptorName=https`, `http`;
  `internal/sdsocket`). The socket units are `PartOf=` the edge's
  service. So a restart that systemd does on its own after a crash
  (`Restart=always`) keeps the listening sockets: new connections wait
  in their backlog for the edge to come back. An explicit restart
  (`systemctl restart hangar-edge`, or the `edge` step after a unit or
  binary changed) restarts the sockets too: for that moment :443 and :80
  are closed, and new connections are refused. The edge is the front door
  when it is given `--cp-domain`, and on a host it runs only as the front
  door: public endpoints are served nowhere else. Its plain-HTTP listener
  on loopback (`--listen`) is for tests alone, off unless given.
- **TLS with certificates it gets itself** (`internal/edge/front/certs.go`):
  certmagic, over ACME DNS-01 only, with libdns/cloudflare and a Cloudflare
  token that may edit DNS in the two zones and nothing else.
  - The certificates are `api.<cp-domain>`, `console.<cp-domain>` and
    `*.<user-domain>`, one each.
  - DNS-01 needs no traffic to the host, so certificates come before DNS
    points at it.
  - They are kept in the edge's state directory (`/var/lib/hangar-edge`,
    0700, keys 0600), so a restart serves them at once and asks the CA for
    nothing. Renewal is certmagic's; `verify.sh` fails a name whose
    certificate in storage has 14 days left or fewer.
  - The token is a root-owned 0600 file that systemd hands the edge as a
    credential (`LoadCredential=`). It is not `hangar-cloudflare`'s.
  - `--acme-ca` names the CA: Let's Encrypt by default, its staging
    directory or pebble for tests. `--acme-ca-root` trusts a test CA's
    directory.
  - `--acme-email` is required with `--cp-domain`. Without an email,
    certmagic takes one it discovers for the whole process: when one name
    looks in storage while another is saving the new account, it can
    discover `default`, and register the account as `mailto:default`,
    which the CA refuses. The front door's names are obtained at once, so a
    first start can hit this; an email set is never replaced.
  - Until a certificate exists, a handshake for its name fails, and nothing
    else does. A server name the front door has no certificate for, or
    none, fails the handshake: there is no default certificate and no
    self-signed fallback.
- **Routing by Host** (`internal/edge/front/front.go`):
  - `api.<cp-domain>` goes to the apiserver.
  - `console.<cp-domain>` sends Connect's paths (`/hangar.`), the
    WebSocket routes (`/v1/`) and sign-in's (`/auth/`, where the provider
    returns, `authentication.md`) to the apiserver. Everything else is the
    console's files from a directory (`--console-dir`), with every path
    that is neither a file nor has an extension served `index.html`. The
    files are opened inside the directory alone (`os.Root`), and served
    with the console's `Content-Security-Policy` and `Referrer-Policy`
    (`console-design.md`).
  - `<label>.<user-domain>` goes to the public endpoints
    (`public-endpoints.md`), whose callers the edge signs in itself.
  - Responses of `api.` and `console.` on :443 carry
    `Strict-Transport-Security: max-age=31536000`, without
    `includeSubDomains` or preload; the endpoints' carry none, as each
    endpoint answers for its own.
  - Any other Host is 404. A Host of another site than the certificate the
    connection was opened for is 421. :80 only redirects a known Host to
    https (308) and serves nothing else, ACME included.
- **The API from the front door reaches the apiserver over a unix socket of
  its own**, `/run/hangar/public.sock` (root's and the edge group's,
  0660, made by systemd), never TCP.
  - The apiserver serves only the edge's uid there (`SO_PEERCRED`).
  - It is the public listener's handler and chain (`authentication.md`),
    sign-in and its tokens included: the edge vouches for the client's
    address, never for the caller.
  - It refuses the socket in dev auth, which trusts a header.
  - The edge proxies over HTTP/1.1 and flushes at once. Server streams flow,
    WebSocket upgrades pass through, and `Host`, `Origin` and the cookies
    are passed on unchanged, as sign-in's exact-origin check, its
    `__Host-` cookies and the WebSocket `Origin` check need
    (`streaming-transport.md`). The edge ends TLS, so the browser's origin
    is https.
  - The apiserver's loopback TCP listener stays, for the host itself;
    the firewall keeps it from everyone else.
- **The client's address is `Hangar-Client-Ip`, and only the edge says it.**
  - The edge drops whatever the client sent of `Hangar-Client-Ip`,
    `Forwarded`, `X-Real-Ip`, `Cf-Connecting-Ip` and `True-Client-Ip`, and
    sets `X-Forwarded-For`, `-Host` and `-Proto` afresh.
  - To the apiserver it sets `Hangar-Client-Ip` to the address of its own
    TCP peer.
  - The apiserver believes the header only on the public socket from the
    edge's uid (`auth.TrustClientIPFrom`). On its TCP listener it drops the
    header and takes the connection's address (`auth.ClientIPFromConn`).
    Its audit lines, sign-in's included, carry it as `client_ip`.
- **Limits** (`internal/edge/front/limits.go`; each is a flag of
  hangar-edge). A client is an IPv4 address or an IPv6 /64, but in the
  host's own /64 (`--own-prefix`): every machine on the host has an
  address of it, so there each address is a client, and the /56 and /48
  caps do not count it. A machine's IPv4, NATed, arrives as its network's
  address on the host's side, so the machines of one network are one
  client over IPv4.

  | Limit | Default | Past it |
  |---|---|---|
  | open connections, everyone | 4096 | closed at accept |
  | open connections, one client | 64 | closed at accept |
  | open connections, every client in one IPv6 /56 | 256 | closed at accept |
  | open connections, every client in one IPv6 /48 | 1024 | closed at accept |
  | new connections, one client | 10/s, burst 50 | closed at accept |
  | requests, one client (each HTTP/2 stream one) | 20/s, burst 100 | 429 with `Retry-After` |
  | requests in flight, one client (streams and WebSockets included) | 128 | 429 |
  | request header | 64 KiB (net/http reads 4 KiB past it) | 431 |
  | request body to the API | 8 MiB | 413 |
  | request body to an endpoint | 100 MiB | 413 |
  | TLS handshake, then each request header | 10 s | closed |
  | idle connection | 2 min | closed |
  | an endpoint's response header | 100 s | 502 |
  | one console file | 1 min | closed |
  | HTTP/2 streams on a connection | 100 | refused |
  | HTTP/2 ping when idle, answer, a write | 30 s, 15 s, 30 s | closed |

  There is no overall read or write timeout: Watch, server streams and
  WebSockets stay open for as long as they are used. A slow request body is
  bounded only by the connection limits.
- **The host's firewall opens 22, 80 and 443 alone** (decided by the
  owner; `deploy/host/steps/45-firewall.sh`).
  - It is one nft table of its own, `inet hangar-host`. Its input chain
    drops everything, IPv4 and IPv6, but loopback, replies, ICMP and ICMPv6
    (neighbour discovery, the proxy NDP guests' IPv6 needs, path MTU), the
    uplink's DHCPv4 replies, and TCP 22, 80 and 443.
  - It has no forward chain: the guests' traffic is the node agent's tables'
    alone (`networking.md`).
  - Machines on the host reach the front door at the host's public
    addresses, TCP 80 and 443 alone, as the internet does: the node
    agent's input rule lets those in from its networks and drops the rest
    (`networking.md`). A machine calling the platform by its public names,
    `api.` or an endpoint, so gets through without leaving the host.
  - A unit of its own loads it before the network comes up. It is never
    loaded by `nftables.service`, whose stock config flushes every table.
- **The pages the front door answers with itself are plain-text
  placeholders**, all in `internal/edge/front/pages.go`. What a person sees
  in a browser is designed with the owner separately.

## Consequences

- Cloudflare no longer sees the traffic, and nothing filters it before the
  host: the limits and the firewall are all there is. Cloudflare is DNS
  alone (`cloudflare-setup.md`).
- The cutover from the tunnel is one switch: deploy, the certificates,
  then DNS, with the tunnel removed afterwards; between the deploy and DNS
  only the admin socket works (`docs/runbook/cloudflare.md`).
- Tests: `internal/edge/front` issues real certificates from pebble, in the
  test process, with a DNS server of its own (`acmetest`). On a testbed,
  `test/testbed/host-bootstrap.sh` runs the deploy, the socket activation,
  the firewall and pebble against a hangar-edge built with the `testbed`
  tag, which can answer DNS-01 itself (`--acme-dns testbed:<addr>`). A
  release is never built with that tag. The L4 flow
  (`test/testbed/flow`, `TestTheDirectPath`) runs the whole direct path
  with real machines: a sign-in against a fake provider through the front
  door, the API through it, a private endpoint's handoff and a public
  endpoint.
