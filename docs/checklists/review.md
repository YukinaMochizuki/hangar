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

## Changes touching a host driver (`internal/node/...`)

- [ ] The fake implements the new behaviour, and the conformance suite covers
      it, so the fake and the real driver are held to the same contract.
