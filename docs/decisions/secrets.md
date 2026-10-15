# Secrets: write-only, sealed at rest, a login per pull, and no shared private image without proof

Status: accepted, 2026-10.

## Context

Containers' images usually live in private registries (ghcr.io, a private
Docker Hub repository, ECR), and until now every pull was anonymous. Holding
registry logins means holding credentials for tenants: they must not leak
through the API, the store and its backups, the logs, or the node. And the
node shares what it pulls across tenants: content is cached by digest,
unpacked once into a base, and cloned for every machine of that digest.

## Decision

- **A project-scoped `Secret` kind** (`hangar.secrets.v1alpha1`), of type
  `registry-auth` first, holding one key, `.dockerconfigjson`, in the Docker
  config format, so what `docker login` writes can be given as it is. Other
  types (`opaque`, `tls`) can follow. Admission checks the format and bounds
  the data to 64 KiB; a login names a whole registry by its host, and Docker
  Hub's names are one, `index.docker.io`. Its errors never quote a value.
- **Values are write-only.** Apply sets `spec.data`; no answer holds it
  again: get, list, watch, apply's and delete's answers, and status updates
  all go through one function that clears it (`kindHooks.redact`). Status
  says what can be said without the values: the keys, the registries, and a
  content HMAC. **Every apply carries the data** (apply replaces the whole
  spec, and what get answers has none, so applying it back is refused,
  saying so). Data equal to what is stored, by its HMAC, is no change: no
  new generation and no write.
- **The HMAC is keyed** by a key derived from the KEK (HKDF), not a plain
  hash: a plain hash of a password is an offline guessing oracle for
  everyone who can read the secret's metadata.
- **Envelope encryption at rest.** Each secret's data is sealed with
  AES-256-GCM under a data key of its own, which is wrapped by the
  key-encryption key (KEK), AES-256-GCM too. The sealed form,
  `spec.sealed`, is what the store and its event log hold; it names the KEK
  by id, and both seals are bound to the secret's project, name and uid, so
  sealed data copied onto another secret does not open. The KEK is a file,
  0600, the apiserver's, outside the database
  (`/var/lib/hangar/state/secrets-kek`): a backup of the database holds no
  value. An apply that sets `spec.sealed` is refused.
- **Start-up checks the KEK.** Before it serves, the apiserver opens every
  stored secret: a wrong KEK, or none while secrets are stored, stops it,
  saying which secret did not open. Without a KEK and no secrets stored, it
  starts and refuses Secret writes. Given previous KEKs
  (`--secrets-kek-previous`), it seals again under the current one what they
  sealed: that is the rotation (`docs/runbook/host-bootstrap.md`). A KMS can
  later take the KEK's place behind the same keyring.
- **`Container.spec.imagePullSecrets`** names secrets in the container's
  project, mirrored onto its machine. Whoever may apply a container in a
  project may name any secret in it, as in Kubernetes: naming one gives the
  container the image, never the login. A missing secret makes a container
  that is not running say `Ready=False ImagePullSecretNotFound`; a login the
  registry refuses, or none where it asks for one, `ImagePullUnauthorized`.
  Changing the names restarts nothing: they count at the next pull.
- **Nodes get a login per pull, and no other way to a Secret.** Node agents,
  which read every other kind, may not get, list or watch Secrets
  (`kindHooks.hiddenFromNodes`). They ask `ImagePullCredentialsService`, a
  node-only RPC the apiserver serves on the node socket, for a machine: it
  answers only a node agent this apiserver runs, only for a machine placed on
  that node, and only with the login, from the first secret the machine
  names that holds one, for the registry of the machine's own image. The
  node asks each time the Images driver needs a login, keeps it in memory
  for that pull, and never writes or logs it. The apiserver logs each login
  given (node, machine, secret, registry), never the login.
- **The cache key is not authorization.** Content is cached by digest and
  shared across tenants, but a digest proves only what the content is, not
  who may have it, and a registry's word that it has a digest proves
  nothing: any registry, an attacker's included, can answer a manifest HEAD
  for any digest. So a machine reuses cached content only if this host
  fetched that digest from the repository its own reference names, and its
  own resolve against that repository succeeded (Kubernetes'
  AlwaysPullImages lesson, by default). It holds by these:
  - **The cache records where content came from.** Each cached digest's
    record lists its sources: the repositories (registry and repository,
    normalized in one place, `driver.Repository`, so `docker.io/library/x`,
    `index.docker.io/library/x` and `x` are one, with the host in lower
    case and without its default port) it was fetched from in full. Only a
    full fetch adds one, and the list is kept with the record across
    restarts. A record written before there were sources has none, and is
    not trusted: it was proven under a rule that let any repository claim
    a digest. The Images driver answers a digest from the cache only for a
    reference naming one of its sources. For any other, it first fetches
    the manifest, the config and every layer from that reference's
    repository with the machine's login, each checked against its digest,
    the layers the cache holds too (hashed as they stream, the cached copy
    left as it is). A repository that cannot serve them all, as one that
    only claims the digest cannot, fails the pull and is never added.
    Anonymously fetched content is therefore reused only by machines
    naming the same repository. This is the driver's contract, held by its
    conformance suite's repository-confusion part on the fake and the real
    driver.
  - **Each machine with credentials proves its login itself.** A machine
    with pull secrets that has not yet proven its image gets a pull job of
    its own, which resolves its reference with its own credentials;
    machines without credentials share one anonymous job per reference.
    A machine whose status records the digest it was pulled for (written
    by its node only, after such a pull) has proven it: after an agent
    restart it is built from the cache, its repository being a source,
    without the registry. Whatever the registry does, a machine running
    its current spec keeps running and its status stands: only starting
    one needs its image, after a host reboot, a stop and start, or a
    change that restarts it.
  - The shared base made of a digest is reached only through a job whose
    pull passed these rules.
- **Security-sensitive.** The redaction, the node RPC's checks and the
  puller's rule have tests aimed at each (`internal/apiserver`,
  `internal/node`, `test/e2e`), checked by mutating them, and take a
  third-party review.

## Consequences

- Losing the KEK loses every Secret; it is kept in the vault, and the host
  makes one only where none is.
- A machine pulls a private image even when another on the host has it, at
  the cost of one registry round trip. The first machine to name a digest
  from a repository the host has not fetched it from costs a full download
  of its layers again, hashed and dropped, so the cost is one extra download
  per new source repository of a digest; the content is still stored and
  unpacked once.
- After the upgrade to the release that records sources, nothing in a
  node's cache has any: the agent fetches each machine's image in full
  from its own registry once, in the background, and starting a machine
  again (after a host reboot, a stop and start, or a change that restarts
  it) waits for that fetch, so a registry that is down blocks the start
  until it is back. Running machines are not touched
  (`docs/runbook/host-bootstrap.md`, `TestAnUpgradedNodeFetchesEachImageAgain`).
- Deleting or changing a secret does not stop what runs with an image
  pulled with it: it counts at the next image change, or the next machine.
- The node cannot learn which secrets exist: it gets a login for a machine,
  and nothing else.
