# Adding a controller

Controllers run on `internal/controller`: inside the apiserver as the System
principal, following the store (`controller.StoreSource`), and in the node
agent as its Node, following the API (`controller.APISource`). Go through
this when adding one; the apiserver's are in
`internal/apiserver/controllers.go`. A node agent writes only status, never
through a backend, so of the rules on writes below only those on status
apply to it.

## Shape

- [ ] It reconciles one kind, by key. `Reconcile` reads the current state
      (the store, or the API from a node) and never relies on the event that
      queued it: a missed, repeated or reordered event must not change the
      outcome.
- [ ] `Watches` lists every kind whose changes affect the result, each with a
      `Map` from the changed object to the keys to reconcile (a child maps to
      its owner, a node change maps to the machines it could take). A kind
      whose `Map` is costly and whose keys another watch already queues on
      start and resync sets `SkipResync`.
- [ ] Nothing slow (a download, a large copy) runs in `Reconcile`: it holds up
      every other key. Start it elsewhere and return `ErrWaiting` until it is
      done, as the node agent's puller does.
- [ ] Anything that changes with time rather than with a write (a deadline, a
      heartbeat) is caught by the resync or by its own loop, not by hoping for
      an event. A change outside the API, such as a VM exiting on the host,
      feeds `Trigger`; a reconcile waiting on one (a VM booting) returns
      `ErrWaiting`, not a failure.

## Writes

- [ ] Every write goes through a backend with `s.system(ctx)`, so admission
      still validates what the controller produces.
- [ ] Writes are conditional on what was read (pass the resource version, or
      apply the object as read). A conflict returns an error and the key is
      retried; nothing is overwritten with stale data.
- [ ] An object the controller found absent is created with
      `backend.Create`, never `Apply`: `Apply` would overwrite one written
      since the read, dropping its finalizers and owner references.
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
      `DisableControllers` false; outside the apiserver package, such as for
      the node agent's, `apiservertest`) and wait for the outcome; they do
      not call `Reconcile` directly unless they test a race. "Nothing more
      happens" is checked after `WaitSettled`, not over a window of time
      (see `docs/decisions/testing-strategy.md`).
- [ ] Its process's `ControllerStatus` reports it, as it does every
      controller of `newControllers` in the apiserver; in the node agent,
      add it there by hand. Otherwise `WaitSettled` cannot see it.
- [ ] The paths that matter are covered: creation, a spec change, a change to
      a watched kind, deletion, and a start over a store written while no
      controller ran.
- [ ] Mutations of the core rules (drop the compare, skip the conditional
      write, ignore a watched kind) make a test fail.
