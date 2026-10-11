# Adding a controller

Controllers run inside the apiserver as the System principal, on
`internal/controller`. Go through this when adding one; the existing ones are
in `internal/apiserver/controllers.go`.

## Shape

- [ ] It reconciles one kind, by key. `Reconcile` reads the current state from
      the store and never relies on the event that queued it: a missed,
      repeated or reordered event must not change the outcome.
- [ ] `Watches` lists every kind whose changes affect the result, each with a
      `Map` from the changed object to the keys to reconcile (a child maps to
      its owner, a node change maps to the machines it could take).
- [ ] Anything that changes with time rather than with a write (a deadline, a
      heartbeat) is caught by the resync or by its own loop, not by hoping for
      an event.

## Writes

- [ ] Every write goes through a backend with `s.system(ctx)`, so admission
      still validates what the controller produces.
- [ ] Writes are conditional on what was read (pass the resource version, or
      apply the object as read). A conflict returns an error and the key is
      retried; nothing is overwritten with stale data.
- [ ] A reconcile that finds nothing to change writes nothing; compare before
      writing, so a quiet system stays quiet.
- [ ] Status the controller shares with another writer (the node, another
      controller) is changed field by field on the latest copy, and the
      condition types each writer owns are written down next to the
      constants.
- [ ] Objects it creates carry an owner reference with `controller: true`, so
      garbage collection removes them with their owner, and any finalizer it
      adds names who removes it and when.

## Tests

- [ ] Tests run the real apiserver with controllers on (`runningWith`, with
      `DisableControllers` false) and wait for the outcome; they do not call
      `Reconcile` directly unless they test a race.
- [ ] The paths that matter are covered: creation, a spec change, a change to
      a watched kind, deletion, and a start over a store written while no
      controller ran.
- [ ] Mutations of the core rules (drop the compare, skip the conditional
      write, ignore a watched kind) make a test fail.
