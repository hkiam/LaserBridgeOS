# ADR 0010: What the bridge may do on its own

- Status: accepted
- Date: 2026-08-15

## Context

[ADR 0009](0009-laserbridged.md) built a bridge that only carries bytes. The
reason for having our own bridge was to be able to do more than that, and this
decides how much more.

The case that prompted it: a laptop running LightBurn closes its lid mid-job.
The TCP connection dies. The controller works through whatever is left in its
planner buffer with nobody watching, and then stops wherever it happens to
stop. Nothing in the appliance noticed, because nothing in it understood what
was being carried.

Now something does. `laserbridged` reads along with the controller's replies
and knows whether the machine is idle, running, holding or in alarm. The
question is what it is entitled to do with that knowledge.

## Decision

**Reading along never touches the stream.** The observer is fed a copy of what
has already been forwarded to the client. It has its own lock, its own buffer
and no way back onto the wire. A parser that misreads a line produces a wrong
number on a status page; it cannot produce a wrong byte at the controller.

**The bridge acts in exactly one situation: the client disappeared and the
machine is moving.** `grbl.on_disconnect` chooses what happens:

- `none` — carry on as before. The machine finishes its buffer.
- `hold` (default) — send a feed hold. Motion pauses, the beam goes off, the
  position is kept, and the job can be resumed.
- `reset` — feed hold, wait for the machine to come to rest, then a soft
  reset. The job is over, but nothing is left moving.

`hold` is the default because it is the reversible one. `reset` waits for the
hold to take effect first: a soft reset during deceleration loses the position
and lands the controller in an alarm, so the bridge asks `?` until the machine
reports it has stopped, and gives up after three seconds rather than wait
forever.

**Nothing else is grounds for acting.** In particular a controller that has
gone quiet is not. GRBL speaks when spoken to, so silence usually means the
client has nothing to ask - only the client knows whether it is mid-job. A
bridge that held the machine on silence would interrupt good work on a guess.
Silence is reported (`controller_silent`) and logged, and the operator, who can
see the machine, decides.

**A daemon being stopped is not a client walking away.** A service restart
after a settings change does not trigger an intervention: somebody asked for
it, and the machine is not unattended.

**The status page says what the bridge did and why.** `last_intervention`
carries it in words, so an operator who finds a paused machine is not left
guessing.

## Consequences

The appliance can now interrupt a running machine. That is a real capability
and it deserves the plainest possible statement of its limits: **this is not an
emergency stop.** It depends on the appliance running, the USB cable being
seated, the controller answering, and a feed hold being enough to make the
situation safe. A hardware emergency stop depends on none of those. The web
interface says so next to the reading, and the documentation says so wherever
the setting appears.

The default changes behaviour for existing installations: a configuration
written before this setting existed loads with `hold`, not with the previous
`none`. That is a deliberate choice to make the safer behaviour the one nobody
has to know about; anyone who wants the old behaviour sets `none`.

No hardware interlock is planned. A relay in the laser-enable line would
remove the "it depends on this daemon" caveat, and it is the only thing that
would; it is also hardware this appliance will not get. Saying so plainly
matters more than leaving it on a list: the caveat above is permanent, not
provisional, and nothing in this project should be written as though a future
version will make the software stop trustworthy.
