# A VM's serial console is interactive

Status: accepted, 2026-10.

## Context

A VirtualMachine's serial console, ttyS0, was its log and nothing more
(`docs/decisions/vm-boot.md`). A guest without a network, without an sshd,
or not yet booted, such as an operating system that keeps its keys in a TPM
and runs no cloud-init, or one stuck at its boot loader, could only be read,
never typed at. Clouds give their VMs a serial console for that: GCE and
EC2 behind a switch that is off by default, Azure with boot diagnostics.

Cloud Hypervisor (v53, as pinned) takes the serial port as a file, a pty,
its own tty, or a socket it listens on (`--serial socket=`). The socket
serves one client at a time: a second one's connection ends the first's.
Two of its behaviours, seen on the testbed (2026-10), shape what follows. A
client that closes its connection while output is unread ends Cloud
Hypervisor's serial thread for good, silently, until it is started again.
After a client has left, the guest's writing can stall until it is next
typed at. And with no client it keeps up to 1 MiB of the guest's output,
the oldest dropped first. Its UART takes about 3 KB/s on the testbed, so a
full screen of a TUI takes about a second to draw.

## Decision

- **A firmware VM's serial console is a socket of Cloud Hypervisor's,
  whose one client is the VM's console helper, from its start to its end.**
  `--serial socket=ch/serial`, in the one directory Cloud Hypervisor writes
  in. The helper, `hangar-node vmm-console`, is started by the launcher with
  Cloud Hypervisor and lives as long; it never closes the connection with
  output unread: it ends its writing and reads to the end. When the guest
  reboots, Cloud Hypervisor makes the socket afresh, and the helper dials
  it again, every 200 ms until it is there.
- **The helper runs as the VM's user, with no capability, in the VM's
  unit**, as Cloud Hypervisor does: through `vmm-exec`, with no group but
  the VM's (`docs/decisions/node-agent.md`). It parses what the guest
  writes, and that parsing is far larger than the launcher's framing, so
  it holds nothing the guest could not reach through Cloud Hypervisor,
  which is the same user. Not the launcher, which is root with
  `CAP_DAC_OVERRIDE`. Not the node agent, which restarts on its own, and
  whose connection ending would end the serial thread.
- **It is started once, before the launcher gives up its capabilities**,
  which running it as the VM's user takes. If it ends while the VM runs,
  the console, and the log, end with it for the rest of the boot, and the
  launcher says so in its journal; the next boot has them again.
- **The log is as it was.** The helper hands every byte the guest writes
  to the launcher on a pipe, and the launcher frames it into `console.log`
  as it framed the FIFO's. What a client types is not logged; the guest's
  echo of it is output, and is.
- **Clients attach to a socket the launcher makes**, `console.sock`, in the
  VM's directory, outside `ch/`, root's: the launcher listens on it and
  gives the helper the listening fd, so Cloud Hypervisor can neither
  replace it nor take its clients. The node agent dials it, never through
  a link (`driver.Hypervisor.AttachConsole`). A client speaks
  `hangar.node.v1`'s `ConsoleRequest` and `ConsoleResponse`, each delimited
  by its length: a start first, then what it types and its resizes; it is
  sent its state, then the screen drawn whole, then what comes, and its
  state again when it changes.
- **The host keeps the screen.** The helper feeds a terminal emulator
  (`github.com/charmbracelet/x/vt`, pinned) what the guest writes, and
  draws a client that attaches the screen as it is, the cursor and the
  modes that change what its keys send included, under the same lock as
  what is sent after: nothing is missed or sent twice. Not a replay of the
  log, which would draw a TUI wrong.
- **The emulator is guarded**, as the guest controls what it parses: counts
  past 1000 in the sequences that repeat, move, erase or scroll are dropped
  (one of a billion held it for seconds); the size is bounded to 500x200;
  its answers to the guest's queries are read and dropped; it keeps no
  scrollback; a write that panics, or takes past a second, starts it
  afresh, losing the screen but none of the output. Its version moves only
  with its fuzz target (`FuzzScreen`) run again.
- **Many watch, one types.** The first client holds the input; one asking
  for it while it is held watches; a read-only client never holds it; one
  that forces takes it, and the one that held it is told. When the holder
  leaves no one holds it. A client that falls 1024 messages behind is told
  so, if it can still be, and let
  go: the guest's output is never held up for one.
- **80x24 until the holder says otherwise.** A serial line carries no
  window size: the guest's tty is 0x0, which programs take as terminfo's
  80x24. The host's screen starts there; the client holding the input may
  resize it, within the bounds, which every client is told and drawn again,
  and the guest is told only by its user, with `stty`. A reboot brings
  80x24 back, as the guest's tty forgets its size.
- **Clients reach it through the apiserver**, as an exec: a WebSocket,
  `:console` with the subprotocol `hangar.console.v1alpha1`, of
  `MachineConsoleRequest` and `MachineConsoleResponse`
  (`proto/hangar/compute/v1alpha1/machine_capability.proto`), relayed to
  the node's `NodeService.Console` on its tunnel
  (`docs/decisions/node-tunnel.md`), which needs `compute.machines.exec`.
  `hangar console vm NAME` is a terminal on it: raw, Ctrl-] to leave, as
  telnet's and `virsh console`'s, every other key the guest's; it says on
  stderr how it is attached, and the `stty` that tells the guest the
  screen's size.
- **The web console has it too, out of the way.** It is slower than SSH and
  is for what SSH cannot reach, so a VM's page offers it only in its head's
  "..." menu, never as a button. It has a page of its own,
  `/vms/<name>/console` (`web/src/pages/machine-console.tsx`), which
  attaches on opening as `hangar console` does, on the same WebSocket
  (`web/src/lib/console.ts`), drawing with the container Terminal's xterm
  (`web/src/components/terminal.tsx`). The terminal is drawn at the
  console's size, not the window's, as that size is everyone's: a smaller
  window scrolls it. The client holding the input sets it from a few sizes
  or the window's; one watching is offered Take input, which attaches
  again with force.
- **No switch.** Unlike GCE's and EC2's, a VM's console is never turned
  off: whoever may reach it, with `compute.machines.exec`, which every
  built-in role granting it grants with `compute.machines.ssh` and the
  writing of the VM, reaches its guest more directly already: its disks,
  its user-data, its security groups. A switch would guard nothing they
  could not undo.
- **A VM started before this keeps its FIFO**, until it is started again:
  the launcher reads its arguments when it starts. Its console takes no
  client (`driver.ErrNoConsole`). A container's serial is the guest
  kernel's log, a FIFO, and takes none either.

## Consequences

- The serial console bypasses the VM's security groups, and reaches its
  boot loader: GRUB's menu, where a kernel's command line can be changed,
  and any login an image leaves on ttyS0.
- The serial console is slow, about 3 KB/s: a debugging tool, not a way to
  work in a VM; `hangar ssh` is that.
- The VM's unit runs one process more, of a few MiB and a few threads, on
  one CPU at a time, within its `MemoryMax`; its `TasksMax` has 16 more
  for it, as a Go runtime that cannot make a thread dies (the testbed saw
  a guest's boot, Cloud Hypervisor's I/O threads at their most, reach the
  limit that had sufficed without the helper).
- A bug in the emulator is a way from the guest to the VM's own user,
  which Cloud Hypervisor already is.
- A helper that dies takes the rest of its boot's log with it, and, as it
  leaves Cloud Hypervisor's connection unread, can end Cloud Hypervisor's
  serial thread, or stall the guest's writing on its serial port, for the
  rest of the boot.
