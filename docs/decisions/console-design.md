# The console: how it looks, how it reaches the API, how it is served

Status: accepted, 2026-10. The visual direction was settled with the owner on
a proof of concept with mock data; the rest with the first version on real
data.

## Context

People use Hangar through the CLI and, for seeing what runs and changing it
by hand, a web console. The console must sign in with Hangar's own browser
sessions (`authentication.md`), must not widen what a stolen page, a
cross-site request or a framed page can do with them, and must be served by
the front door (`front-door.md`) from the same release as the binaries.

## Decision

### Stack

- Vite, React and TypeScript in `web/`, a pnpm workspace package beside
  `gen/ts`. TanStack Router for pages; the API through the generated
  protobuf-es code in `gen/ts` and connect-query (TanStack Query) over
  connect-web. Tailwind with components in the shadcn/ui style, written in
  the repo. Fonts (Geist, Geist Mono) come from npm and are served with the
  console's files: nothing is loaded from another origin.
- `pnpm --dir web typecheck`, `lint` and `build` are what CI runs on it.

### Look

- **A precise instrument, with hardware in three places.** Neutral greys,
  thin borders, few shadows, dense information; IDs, addresses and sizes in
  the monospace face. Something closer to a consumer product only on the
  size cards when creating a machine, the "where it runs" panel of a
  machine, and the host card of the Overview. At most one illustration a
  page; list pages are data alone. Each action shows its equivalent CLI or
  YAML, copyable.
- **Light and dark themes**, following the system unless chosen. The tokens
  are CSS variables in `web/src/styles.css`, switched by `data-theme` on
  `<html>`.
- **The accent is aviation orange**, and only buttons, the selection, links
  and LEDs take it. States (ok, pending, failed, off) have colours of their
  own that the accent is never confused with. The accent is one set of
  variables per theme, so it can change in one place.
- **Hosts are line drawings**, monochrome, seen from below in perspective,
  one per chassis model (`web/src/components/chassis-art.tsx`); only LEDs
  are coloured. A host is described by its placement alone (region, site,
  CPU model and generation, memory, storage, network, chassis) and by what
  the project itself runs on it, summed in the console; never by core
  counts, clocks, memory size or how loaded the whole host is (decided by
  the owner). A chassis the console has no drawing of is drawn as a
  generic server.
- **The mark** is a fighter shelter, a low wide arch, with a swept tail fin
  in the accent standing in it (`web/src/components/marks.tsx`).
- **Taxi lines** guide and show progress, and are never decoration: the way
  covered is solid, the way ahead dashed. They show a machine's lifecycle
  on its page, folded to one line once it runs, and run into the mark's
  shelter on an empty list (`web/src/components/taxiway.tsx`). Their
  motion stops under `prefers-reduced-motion`.
- **Notices sit in two places only**: next to the action that would fail
  (above a form's submit button, and on the option that causes it), or as a
  page banner under the top bar for the state of a whole project or host
  (`web/src/components/notice.tsx`).
- **Copy states facts.** No "next step", no imperative sentences: "XL needs
  16 GiB; 8 GiB of 24 GiB is left." Buttons are named for what they do.
- **Quota is not shown when creating a machine.** The form warns only when
  this creation would go past a limit, never when a limit is near: creating
  is not to be discouraged. The Overview shows usage against limits.

### Pages and gating

Every page is behind `AccountService.WhoAmI`:

| WhoAmI | Page |
|---|---|
| Unauthenticated | Signed out, with a link to `/auth/login?return=<this page>` |
| A person not invited | Waiting: who is signed in, and that no invitation exists; asked again every 30 s |
| Invited, no User | Signup: a username checked with `NameService.CheckName`, then `AccountService.SignUp` |
| A member | The console |

The project is in the path, `/projects/<project>/…`, so a link names its
project; `/` opens the project used last, or the first one. The console
shows only what works. The sidebar lists only pages that exist, a kind
joining it when its page does (decided by the owner), and the top bar has
no search until search works.

### Machines

- **Start, stop and delete are the API's calls**, so the API's rules
  decide and its error is shown where the action was asked for. Start and
  stop work as `hangar start` and `hangar stop` do: read the object, apply
  it back with only the run state changed at the version read, and read
  again on a conflict. Delete asks for the name to be typed and says what
  goes with the object.
- **Creating never replaces a machine.** The API's apply creates or
  replaces, and offers callers no create-only form, so the console guards
  twice (`web/src/lib/create.ts`). The form sends nothing until the
  project's machines of the kind have loaded and the name is not among
  them; while they load, or when they failed to, it is disabled and says
  so. The write then asks for the object by name and applies only when it
  is not found. Between that Get and the apply a window remains, in which
  an object of the name made by someone else is replaced; only a
  create-only apply in the API closes it, which is tracked on its own. The
  apply's answer says whether it created the object, and when it says
  `created: false` the form shows that the apply replaced one.
- **The quota warning reads the project's status**: its limits, the smaller
  of its own and its tenant's, against its own usage. A creation that goes
  past the tenant's limit through other projects' usage is refused by the
  API, and the form shows that refusal.
- **Logs** are `MachineCapabilityService.Logs`, the server stream `hangar
  logs -f` reads, followed while the page is open.

### How the session reaches the API, and CSRF

- **One origin.** hangar-edge serves the console's files and the API's paths
  (`/hangar.`, `/v1/`, `/auth/`) on `console.<cp-domain>`
  (`front-door.md`). The console calls the origin it was loaded from; there
  is no CORS, and no other origin can call the API with the cookie.
- **The cookie is the only credential, and script never holds it.** The
  session is `__Host-hangar_session`, `HttpOnly`, `Secure`, `SameSite=Lax`
  (`authentication.md`). The transport sends it with `credentials:
  "same-origin"`; the console never reads, stores or forwards a token, and
  keeps nothing about the session in storage (only the theme and the last
  project).
- **CSRF is the apiserver's Origin check.** It believes the cookie only on a
  request whose `Origin` is the public origin exactly and whose
  `Sec-Fetch-Site`, when sent, is `same-origin`. Every Connect call the
  console makes is a POST, which always carries `Origin`: connect-web's GET
  form for side-effect-free methods is turned off (`useHttpGet: false` in
  `web/src/lib/api.ts`), and must stay off, since a GET may come without
  `Origin` and is then refused. The WebSockets of exec and ssh
  (`streaming-transport.md`) carry `Origin` on their handshake and are held
  to the same check.
- **Signing in and out are the apiserver's pages.** Signing in is a top-level
  navigation to `/auth/login?return=<path>`; the apiserver keeps only a
  relative path for the return. Signing out is a same-origin
  `POST /auth/logout`, which answers 204, after which the console loads
  `/` again.
- **What the edge sends with the console's files.** `Referrer-Policy:
  same-origin`, which `authentication.md` requires so that a form's POST
  carries the real `Origin`, and this `Content-Security-Policy`:
  `default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'
  data:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri
  'none'; form-action 'self'; frame-ancestors 'none'`. The build has no
  inline script or style, so injected markup cannot run, and no other site
  may frame the console to dress up a click on it. `vite preview` sends the
  same headers, so the built console is tried under them.

### Serving and release

- `make release` builds the console from the lockfile (`pnpm install
  --frozen-lockfile`, `pnpm --dir web build`) and packs `web/dist` into
  `console.tar` with no time, owner or umask in it, so the same commit gives
  the same checksum; it is in the release's `SHA256SUMS` with everything
  else.
- The host's `release` step installs `console.tar` with the other files,
  unpacks it into `/usr/local/share/hangar/console.d/<checksum>` with the
  previous release's own assets copied in beside its own, and makes
  `/usr/local/share/hangar/console`, hangar-edge's `--console-dir`, a
  symbolic link to it, replaced in one rename. The edge opens the directory
  at each request, so a release's console is served at once, with no
  restart. A page loaded just before the switch still gets its assets:
  they are named by their content, so two releases' never collide. The
  previous release's directory is kept; older ones are removed. Going back
  to the kept release copies the newer one's assets into it the same way.
  `index.html` is never cached and assets are cached for good
  (`internal/edge/front/console.go`).

### Data

- Lists are asked again every few seconds (TanStack Query's
  `refetchInterval`) rather than watched; the API's Watch streams can
  replace that without changing a page.
- A Container or VirtualMachine is shown with the Machine it runs as, found
  by the Machine's owner reference: its phase, node, addresses and boot
  time are there (`machine-model.md`).
- Where a machine runs is the container's or VM's `status.placement`
  (`machine-placement.md`), read through one adapter,
  `web/src/lib/placement.ts`. A machine without one is shown as not placed
  yet; an empty field shows nothing; a chassis the console has no drawing
  of, or none, is drawn as a generic server.

## Consequences

- The console holds no secret, so an XSS-free build and the Origin check are
  what stand between another site and a person's session; the CSP and
  `frame-ancestors 'none'` are the second layer.
- A release needs Node and pnpm where it is built, as it needs Go.
- Local development runs the apiserver in dev auth (`make dev`) and
  `vite`/`vite preview` with a proxy that adds the dev user's header
  (`web/vite.config.ts`, `HANGAR_DEV_USER`). Dev auth has no sign-in pages,
  so the sign-in itself is tried on the testbed (`TestTheDirectPath`), not
  locally.
