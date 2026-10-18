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

### Tenants and projects

- **A tenant has pages of its own, apart from any project's**, at
  `/tenants/<tenant>/…`, with a sidebar of their own: an Overview (its plan,
  what all its projects hold against the plan's limits, and each project
  with what it holds) and its Members. Tenant settings stay out of a
  project's pages, so a project's Members lists the project's people
  alone. Someone whose roles do not include reading the tenant sees that
  stated, and the tenant's projects they hold a role in.
- **Where the console is, is chosen in a picker**: a dialog listing every
  tenant the member sees (those it may list and those of its projects, so
  a tenant with no project too) with its projects under it, filtered by name or
  ID, opened from the switcher at the top of the sidebar. A tenant's row
  opens its pages, a project's row the project. The top bar's tenant crumb
  opens the tenant's pages too.
- **A new project** is made from the picker or a tenant's Overview. Its
  name is global and never reused, so it is suggested from the display
  name as `hangar create` suggests it (the same slug and six random
  digits), checked with `NameService.CheckName`, and can be changed before
  creating. The project is made with a create-only apply, so a name taken
  after the check is refused and the other project left as it was. Only
  the tenants the caller may create a project in are offered, and New
  project is drawn only when there is one. When the tenant
  already holds as many projects as its plan allows, that is stated above
  the button, which is disabled.

### Members

- A project's or a tenant's Members page lists the people bound at exactly
  that scope, through `MemberService`, one row a person. A person who is
  not a platform member yet is shown with that alone ("the role takes
  effect once they are"), never with whether they were invited
  (`tenancy-and-iam.md`).
- Roles are changed in the row, added in a dialog with what each role
  holds, and removed after the dialog says what ends and what stays (a
  project role ends; a tenant role stays). The API's refusal, such as a
  tenant left without an owner, is shown where it was asked for.
- A project's page states, under its list, how many people hold a role
  there through the tenant, linking to the tenant's Members; when the
  caller may not list the tenant's members, it states only that the
  tenant's roles hold there too.

### What a control is shown for

A page asks `AccountService.TestPermissions` which of the permissions its
controls need the caller holds at the scope, in one call
(`web/src/lib/permissions.ts`), and shows a control only when the answer
holds its permission: adding a member and changing a role need
`iam.rolebindings.apply`, removing one `iam.rolebindings.delete`, and a new
project `tenancy.projects.apply` in its tenant. While the answer loads, or
when it failed, nothing is held, so a control appears once it is known to
work; a refusal is stated only when the answer came and said so. On a
page, a failed answer hides the controls and states nothing; a form that
cannot go on without the answer (New project) shows the failure as the
error it is. The answer only decides what is drawn: the API still checks
every call, and a refusal is shown like any error. Pages whose controls
do not ask yet show them to everyone and show the API's refusal in place.
New project asks once per tenant the member sees, which is every tenant
for a platform administrator; at today's size that is a handful of calls.

### Machines

- **Start, stop and delete are the API's calls**, so the API's rules
  decide and its error is shown where the action was asked for. Start and
  stop work as `hangar start` and `hangar stop` do: read the object, apply
  it back with only the run state changed at the version read, and read
  again on a conflict. Delete asks for the name to be typed and says what
  goes with the object.
- **Creating never replaces a machine.** The form sends nothing until the
  project's machines of the kind have loaded and the name is not among
  them; while they load, or when they failed to, it is disabled and says
  so (`web/src/lib/create.ts`). The write is a create-only apply, so an
  object of the name made by someone else since the list loaded is
  refused by the API (`docs/decisions/apply-semantics.md`). By the submit
  button the form says that refusal as it says a name in the list, that a
  machine of the name exists in the project; any other error shows as the
  API gave it.
- **The quota warning reads the project's status**: its limits, the smaller
  of its own and its tenant's, against its own usage. A creation that goes
  past the tenant's limit through other projects' usage is refused by the
  API, and the form shows that refusal.
- **Logs** are `MachineCapabilityService.Logs`, the server stream `hangar
  logs -f` reads, followed while the page is open.
- **The Runs on panel says where the machine stands** on its way to a
  host, in its header where the host's name goes once it is placed
  (`web/src/components/runs-on-panel.tsx`, the state from
  `web/src/lib/runs-on.ts`; settled with the owner):

  | State | Read from | Header | Body |
  |---|---|---|---|
  | Scheduling | asked to run, no placement, not refused | "Scheduling…" | empty, a faint light sweeping across it |
  | Not schedulable | the Machine's `Scheduled` condition False; the Machine not known, the container's or VM's `Ready` reason `Unschedulable` | "Not schedulable", in the warning colour | the scheduler's message, as a sentence |
  | Stopped, never placed | stopped, no placement | nothing | empty |
  | Placed | `status.placement` | the host's name | the host's drawing and rows |

  A placement wins over every other state. Before placement nothing of the
  hardware is known, so nothing of it is drawn: the empty body is as tall
  as a placed one, so the page does not move when the host comes, and the
  sweep covers it to the card's rounded edges.
- **A host arrives only when watched.** When the placement comes while the
  page is open, the host's lines trace themselves, its LEDs light one after
  another and its rows fade in in order, about a second in all. A machine
  placed when the page opens shows its host with nothing played. Under
  `prefers-reduced-motion` nothing sweeps and the host replaces the empty
  body at once.

### Disks and images

Settled with the owner on the screens, before they read the API.

- **Disks have a list and a page; so do images.** They sit in the sidebar's
  Storage group. A disk shows its phase (Pending, Available, In use), its
  size on its node with the size it is growing to when its spec asks for
  more, what it holds (blank, or a copy of an image), its node ("Not placed
  yet" until a machine using it is placed), and the machine it is attached
  to. When the disk's `Ready` condition, which its node writes, is False,
  its reason shows under the phase, and on the disk's page as a banner.
  When a placed disk's `Allocated` condition, which the apiserver writes, is
  False (a growth not granted, such as `ResizeDoesNotFit`), its reason shows
  next to the size it is growing to, and on the disk's page as a banner
  with the API's message. A disk not yet placed is `Allocated` False too
  (`WaitingForFirstMachine`); that blocks nothing and is not shown.
- **The attached machine is shown as its Container or VM**, linked to that
  page. A disk's `status.machine` names the Machine, which is found by name
  and then followed to its owner, as everywhere else
  (`web/src/lib/storage.ts`); while the machines load, the Machine's own
  name is shown, unlinked. A VM's root disk made from an image says so,
  from its `virtual-machine` annotation and owner reference.
- **Three actions on disks: new, grow and delete.** Each is the API's own
  call, and its error is shown in the dialog that asked for it.
  - New disk never replaces a disk, as machine creation does not
    (`web/src/lib/create.ts`): nothing is sent until the project's disks are
    known and the name is not among them, and the one apply it sends is
    create-only, so a name taken since is refused with `AlreadyExists`,
    shown as "A disk named X exists in this project." The dialog's CLI is
    `hangar apply --create-only`.
  - Grow sets `spec.size`: it reads the disk and applies it back with only
    `spec.size` changed at the version read, as start and stop do. The
    dialog states the current `spec.size` and what the node holds. Its
    smallest size is what the node holds (and, for a disk of an image, the
    image's smallest disk), so a growth not yet granted can be withdrawn,
    as the API allows. When the disk read already has the size asked for,
    or has changed so that the request no longer goes the way it did when
    asked, nothing is applied and the dialog says so. When the disk is
    attached, the dialog says that growing restarts that machine, which is
    what happens (`machine-model.md`, `storage-zfs.md`); for a disk
    attached to none it says nothing of restarts.
  - Delete asks for the name to be typed and says what goes with the disk.
    On a disk attached to a live machine it says the API refuses, and
    shows the refusal when it comes; when that machine is being deleted,
    the API does not refuse, and the dialog says the data goes once the
    machine is gone.
- **Images are read-only in the console.** Only platform administrators
  write them (`vm-images.md`). An image's smallest disk is the larger of
  its `min_disk_size` and its own size, as admission holds disks to. An
  image's page offers "New disk from it", the same dialog with the image
  chosen; it is disabled for an image at fault (`Verified` False), which
  the page names in a banner.
- **Controls are shown as "What a control is shown for" says**, asked at
  the project in one call a page: New disk and "New disk from it" need
  `storage.disks.apply`, Grow `storage.disks.apply`, Delete
  `storage.disks.delete`. While the answer loads, or when it failed, the
  control is hidden and these pages say nothing about it, neither a
  refusal nor the failed answer.
- Sizes are whole GiB, read and written with `web/src/lib/quantity.ts`.
  The detail pages show their CLI and YAML, and each dialog the manifest
  it amounts to, copyable; the list pages are data alone.

### Networks, security groups, addresses and public endpoints

Settled with the owner on screenshots before the pages read the API.

- **The "Network" group** in the sidebar lists the four kinds, each with a
  list page and an object's page (`web/src/pages/networks.tsx`,
  `security-groups.tsx`, `addresses.tsx`, `public-endpoints.tsx`). The
  pages read the lists the API serves; which interfaces a network or group
  holds is read from the project's Machines, whose interfaces name the
  groups they are in (`web/src/lib/network.ts`).
- **The actions** are: networks are created and deleted; security groups
  are created, their rules edited, and deleted; addresses are reserved,
  bound, moved, unbound and deleted; public endpoints are created, made
  public or private, and deleted. Each is the API's call
  (`web/src/lib/network-actions.ts`), and its error is shown in the dialog
  it was asked from. Creating never replaces, as for machines
  (`web/src/lib/create.ts`): the form refuses a name in the loaded list,
  and sends one create-only apply, whose AlreadyExists for a name taken
  since is shown as "A network named X exists in this project." Its
  command is `hangar apply --create-only`. Editing a group's rules, and
  binding or moving an address, apply at the resource version of the
  object the dialog opened from, with only that field changed: what the
  person chose depends on what they saw, so a newer object is never
  overwritten. The API then answers Aborted, and the dialog says the
  object changed after it opened and offers to load it again. Making an
  endpoint public or private, and unbinding an address, set one value
  whatever the rest is, so they read the object and apply it back at the
  version read, as start and stop do. Each form shows the manifest it
  applies and the `hangar apply` command; delete shows `hangar delete`.
- **What comes from machines shows their state.** Machine counts, members
  and interfaces show a skeleton while the project's machines load and the
  failure when they did not; Delete stays off until they have loaded, and
  says when they failed.
- **A group naming a network the project lacks** is listed under that
  network, stated as "network NAME does not exist".
- **Delete asks for the name to be typed.** When the console already
  knows the API will refuse (machines on a network, interfaces in a
  group), the fact stands above the Delete button, such as "3 machines
  are on this network.", and the button is off.
- **A public endpoint a container's public port made** offers no action:
  the API sets its spec back and makes it again. Its page says which
  container made it.
- **Every network's built-in group `default`** is a row of the security
  groups list, marked "built in", with what it does: it lets in its own
  members and lets everything out. It is no object and has no page.
- **No hostname is guessed.** The endpoint domain is the apiserver's, so
  the create form shows no hostname; an endpoint's page shows the URL its
  status gives.
- **Controls show only when the caller may use them**, as "What a control
  is shown for" above says. Each page asks once, at its project, for what
  its controls need: `network.<plural>.apply` for create, edit rules,
  bind, move, unbind and make public or private,
  `network.<plural>.delete` for delete.

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
  `web/src/lib/placement.ts`. A machine without one shows one of the Runs
  on panel's other states (above); an empty field shows nothing; a chassis
  the console has no drawing of, or none, is drawn as a generic server.

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
