# The console: how it looks, how it reaches the API, how it is served

Status: accepted, 2026-10. The visual direction was settled on a proof of
concept with mock data; the rest with the first version on real data.

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
  counts, clocks, memory size or how loaded the whole host is. A chassis the console has no drawing of is drawn as a
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
- **Copy states facts.** No "next step", no imperative sentences: "Q7 needs
  3 vCPU; 1 vCPU of 5 vCPU is left." Buttons are named for what they do.
- **Copy describes what people can see and do, not how Hangar is built.**
  It says what a container or a virtual machine is to its user (an image
  from a registry, a full operating system, exec and logs, SSH) and never
  names the mechanism underneath: microVMs, firmware, the hypervisor.
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
joining it when its page does.

### The command palette

- **⌘K or Ctrl+K, or the search control in the top bar, opens one field
  that finds things and starts things** (`web/src/components/command-palette.tsx`).
  On a phone the control is an icon and the palette fills the screen.
- **It finds, by name or ID**: the current project's objects of every kind
  the console has a page for (images included, each opening this project's
  page of it), the people bound at the current project or tenant (by
  username or email, opening Members), the projects and tenants the picker
  lists, and the console's pages. On a tenant's pages it finds the
  tenant's members, the projects, tenants and pages. It searches nothing
  on the server and no other project's objects. An exact name ranks above
  a prefix, a prefix above a match inside; the section holding the best
  match comes first. Choosing a result opens it.
- **With nothing typed it offers actions and pages.** The actions are the
  "New …" flows (New container and New virtual machine open the new-machine
  page; the others open their list page with `?new=1`, which opens the
  page's create dialog), Add member, New project, and Switch project, which
  opens the picker. Each "New …" and Add member shows only when
  TestPermissions holds its permission at the project, and New project
  where the caller may create one in some tenant, as on the pages: hidden
  while unknown or failed, with nothing said. Closing a dialog opened by
  `?new=1` takes the parameter away, so going back or reloading does not
  open it again. No action in the palette destroys anything.
- **It reads the lists the pages read**, through the same hooks and query
  keys, without fetching again on opening what the cache holds; a list not
  yet fetched is fetched then. A list still loading or failed is stated
  under the results, one line each, with the API's error; nothing is said
  to match nothing until every list has answered.
- Arrows move through the results, Enter opens, Escape closes, and Tab
  stays inside; focus returns to where it was. The field is a combobox over
  a listbox, the active result its active descendant.

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
- **The sizes are the platform's**, read from the API
  (`MachineSizeService.ListMachineSizes`, `web/src/lib/machine-sizes.ts`,
  `machine-sizes.md`); the console holds none of its own and offers no
  size outside them. The create form shows them by their `order` and fills
  the machine's cpu and memory from the chosen one; `?size=` takes a
  size's name, and a name the platform does not list chooses the first.
  A size's card states its cpu and memory, not its disk.
  With no size defined the form says "No machine sizes are defined on this
  platform." and sends nothing. A list's size badge is the listed size
  whose cpu and memory the machine has, and none when no size matches.
- **The disk is chosen apart from the size**, as GCP and AWS choose a boot
  disk apart from the machine type. The create form's Disk field takes
  whole GiB, at least the minimum its heading states: the chosen size's
  disk, or for a VM its image's smallest disk (`imageFloorGiB`) where that
  is larger. It holds that minimum, following the size and the image, until
  the person edits it. Below the minimum, or not a whole number, the field
  states the fact and the form sends nothing. The manifest and the summary
  show the value typed, refused or not. It goes to a container's
  `resources.disk` and a VM's `rootDisk.fromImage.size`. By the field: "A
  disk can grow later; growing restarts the machine. It never shrinks."
- **A size's card is over quota by its own smallest disk**, with its cpu
  and memory: the larger of its disk and the image's floor, never the disk
  typed. A raised disk past the project's disk limit is stated under the
  Disk field and above the submit button only.
- **The quota warning reads the project's status**: its limits, the smaller
  of its own and its tenant's, against its own usage. A creation that goes
  past the tenant's limit through other projects' usage is refused by the
  API, and the form shows that refusal.
- **Logs** are `MachineCapabilityService.Logs`, the server stream `hangar
  logs -f` reads, followed while the page is open.
- **The Runs on panel says where the machine stands** on its way to a
  host, in its header where the host's name goes once it is placed
  (`web/src/components/runs-on-panel.tsx`, the state from
  `web/src/lib/runs-on.ts`):

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
  another and its rows fade in in order, about a second in all, after a
  moment's pause (0.3 s) that lets the eye land on the page first. Scheduling
  is usually done before the page the create form opens has loaded, so that
  page plays the arrival once for the machine just created, on a host
  already there (`web/src/lib/just-created.ts`, in memory only). Any other
  page that opens on a placed machine, a reload included, shows its host
  with nothing played. Under
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

### Secrets and service tokens

Settled with the owner on screenshots before the pages read the API.

- **They sit in the sidebar's Access group**, with Members: Secrets,
  Members, Service tokens. Each kind has a list and an object's page
  (`web/src/pages/secrets.tsx`, `service-tokens.tsx`,
  `web/src/lib/credentials.ts`).
- **A secret's values are never shown and never read back**
  (`secrets.md`). Pages show its name, type, keys, registries, state and
  times, and of its spec only the type; a value field an answer held
  anyway is dropped in the list's query function, before the query cache
  keeps the answer. Its content HMAC is not shown. A secret's page shows
  which containers name it, from the project's containers'
  `spec.imagePullSecrets`.
- **New secret takes a registry login or a pasted Docker config**: a
  registry, username and password, which the console writes as the
  Docker config `docker login` writes, or the config as given. The
  password field is masked, with `autocomplete="off"`: it is not a
  password for this site, so the browser is not invited to generate or
  save one. The values live in the form's state, and in a ref from its
  submit until the one create-only apply reads it; the call and closing
  the form both empty the ref. The mutation holds the name alone and
  leaves the query client once the form is gone (`gcTime` 0), and a
  replay of it sends nothing. Creating never replaces, as for disks
  (`web/src/lib/create.ts`). Its command is
  `hangar create secret registry NAME --create-only --from-docker-config FILE`,
  which names a file and never a value. Replacing a secret's values is
  not offered in the console.
- **A service token's secret is shown once**, in the dialog of the create
  or the rotate that returned it, with a copy button, and the dialog says
  it is not shown again. The issuer's answer goes straight to that
  dialog's state: the mutation's result holds none, the mutation leaves
  the query client with its dialog, and nothing is kept in storage. The
  dialog is locked: only its Close button closes it, and closing it drops
  the secret. Escape and a click outside do not; when the browser closes
  the element anyway (Chromium does after a few Escapes, with a cancel
  that cannot be prevented), the dialog opens it again with its state.
  The dialog keeps its place in the page whatever the lists do: a list
  that fails to load again, or no longer holds the token, never unmounts
  a shown secret. A list that fails to load again leaves its page showing
  the last answer under a banner that says so. Pages show a token's state
  (Active, Expiring within 14 days, Expired, No secret), its secret's
  first characters as the status gives them, when it expires and was
  last used, and its roles in the project, from the RoleBindings whose
  subject it is.
- **New token** sends one `CreateServiceToken`, with a role to bind in the
  project (none, owner, editor or viewer) and how long the secret works
  (30 days, 90 days, a year, three years); the issuer refuses a name that
  exists. **Rotate** says which secret, by its first characters, stops
  working. **Delete** asks for the name typed and says the bindings
  naming the token go with it, as do secrets' deletes, which say which
  containers name the secret. Each dialog shows its `hangar token` or
  `hangar create secret` command, holding names and options alone; a
  form's command reads NAME until the name is one the form would send.
  A copy button shows its check only once the clipboard took the text,
  and says "Not copied: the clipboard is unavailable." when it did not.
- **Controls show only when the caller may use them**, as "What a control
  is shown for" above says: `secrets.secrets.apply` for New secret,
  `secrets.secrets.delete` for its Delete, `iam.servicetokens.apply` for
  New token and Rotate, `iam.servicetokens.delete` for its Delete. Editors
  read tokens and write none (`tenancy-and-iam.md`), so for them the
  token pages have no controls, and an object page's CLI lists only the
  commands the caller may run.

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
  the same checksum; it is in the host tar's `SHA256SUMS` with everything
  else. The build is told the release's tag (`VITE_HANGAR_RELEASE`), and
  the overview's "From the CLI" card shows that release's install line
  with this deployment's API, `api.` for the console's `console.`
  (`web/src/lib/cli-install.ts`, `docs/runbook/release.md`). A build of no
  tag, or a console on another host, points at the releases instead.
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
