# Review checklist

For anyone reviewing a change, human or agent, and for the author before asking
for a review. Each item links to the rule it checks; this file does not restate
those rules.

## Every change

- [ ] `make check` passes, and CI is green.
- [ ] The change does what its issue or PR description says, and nothing else.
- [ ] Generated code (`gen/`, sqlc output) changed only through its source and
      `make gen`.
- [ ] Every new or changed comment follows `docs/conventions/comments.md`. In
      particular: no option labels or discussion history, no section numbers or
      milestone names, no essays, no ownerless TODOs.
- [ ] Repo text is in English.
- [ ] Tests exercise the behaviour at the right layer (see
      `docs/decisions/testing-strategy.md`). A test that cannot fail when the
      behaviour breaks does not count.
- [ ] Tests do not synchronise on sleep: they wait for what they need, and
      every sleep left carries its reason (see "Waiting in tests" in
      `docs/decisions/testing-strategy.md`).
- [ ] A "did not happen" check looks at the object itself, such as its
      resourceVersion, never at the store-wide revision (same section).
- [ ] Docs follow the change: if it alters how something is done, the matching
      checklist or runbook is updated in the same PR, and new docs are listed in
      `AGENTS.md`.

## Changes to `proto/`

- [ ] `buf lint` and `buf breaking` (WIRE_JSON against `main`) pass. Removed
      fields are `reserved`, by number and by name.
- [ ] A new resource satisfies the envelope check (`go test` runs it).

## Changes touching authentication or authorization

- [ ] Every new RPC or action has a permission, and a test proves a principal
      without it is denied.
- [ ] Nothing trusts a header that a client can set directly.
- [ ] A secret Hangar issues or holds stays a `token.Secret` until the one
      place that sends or stores it, so no log line, error or CLI output
      shows it; tests check the output for it.
- [ ] Anything a browser's session cookie authenticates holds to the
      same-origin rule (`session.SameOrigin`), and no GET a cookie
      authenticates changes anything, with two named exceptions:
      - a WebSocket handshake, which carries `Origin` and is held to the
        rule;
      - `/auth/endpoint-login`, a cross-site GET by design, which writes a
        one-time code. The code goes only to the endpoint's own callback, and
        counts only from the browser holding the edge's state cookie for that
        handoff, so the site that sent the browser gets nothing from it
        (`docs/decisions/public-endpoints.md`).

      Any other is a finding.

## Changes touching a host driver (`internal/node/...`)

- [ ] The fake implements the new behaviour, and the conformance suite covers
      it, so the fake and the real driver are held to the same contract.
