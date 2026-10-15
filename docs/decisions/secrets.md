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
- **The cache key is not authorization.** What one machine pulled with its
  login, another uses only once the registry has resolved the reference with
  that other machine's own credentials (Kubernetes' AlwaysPullImages lesson,
  by default). The Images driver answers a cached digest whatever the
  credentials (its contract, in its conformance suite); the node's puller is
  where access is proven:
  - A machine with pull secrets that has not yet proven its image gets a
    pull job of its own, which resolves the reference (a manifest HEAD, and
    the digest compared when the reference names one) with its credentials,
    whatever the host has cached.
  - Machines without credentials share one job a reference, which resolves
    anonymously: a public image is shared as before, and a private one is
    refused to all of them.
  - A machine whose status records the digest it was pulled for (written by
    its node only, after such a resolve) has proven it: after an agent
    restart it is built from the cache without the registry, so what runs
    keeps running.
- **Security-sensitive.** The redaction, the node RPC's checks and the
  puller's rule have tests aimed at each (`internal/apiserver`,
  `internal/node`, `test/e2e`), checked by mutating them, and take a
  third-party review.

## Consequences

- Losing the KEK loses every Secret; it is kept in the vault, and the host
  makes one only where none is.
- A machine pulls a private image even when another on the host has it, at
  the cost of one registry round trip; the content itself is fetched and
  unpacked once.
- Deleting or changing a secret does not stop what runs with an image
  pulled with it: it counts at the next image change, or the next machine.
- The node cannot learn which secrets exist: it gets a login for a machine,
  and nothing else.
