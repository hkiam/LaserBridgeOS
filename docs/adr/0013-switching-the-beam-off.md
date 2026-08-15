# ADR 0013: Switching the beam off

- Status: accepted, supersedes part of ADR 0010
- Date: 2026-08-15

## Context

[ADR 0010](0010-what-the-bridge-may-do-on-its-own.md) made `hold` the default
for a client that disappears mid-job, on the reasoning that a feed hold is the
reversible option: motion pauses, the beam goes off, the position is kept, the
job can be resumed. Two parts of that sentence do not survive scrutiny.

**"The beam goes off" depends on a setting we do not control.** GRBL's `$32`
decides whether the output is a laser or a spindle. With laser mode on, a feed
hold switches the beam off. With laser mode off, the output is a spindle - and
a feed hold deliberately leaves a spindle running, because a router bit
stopping in the cut is its own kind of damage. `M3` constant power plus `$32=0`
means the axes stop and the beam keeps burning a hole where it stands. A
controller shipped in router mode and repurposed for a diode laser is exactly
how someone ends up there.

Worse than the specific risk is its shape: the safety of an unattended machine
resting on our reading of firmware behaviour that varies by version and fork,
and that cannot be verified from here. Our own controller simulator is no
evidence at all - it encodes what we believe.

**"The job can be resumed" is close to fictional.** LightBurn streams a job
over the connection. When the connection dies, so does the job on its side;
there is nothing left to resume into. A hold leaves the machine standing in
HOLD indefinitely, waiting for a client that has no way to continue.

So the reversibility that justified `hold` mostly is not there, and the risk it
carries is real.

## Decision

**`reset` becomes the default.** Feed hold, wait for the machine to come to
rest, then a soft reset. `mc_reset` kills the spindle and coolant
unconditionally - that is what soft reset is - so the output goes off whatever
`$32` says, whatever `M3`/`M4` is modal, and whatever firmware is running. It
is the one command whose beam-off behaviour is not a matter of configuration.

The cost is that the planner buffer is cleared and the job definitely cannot be
resumed. It could not be anyway. Waiting for rest first is what keeps this
cheap: a soft reset during motion loses the position and lands in an alarm; one
after a completed hold does not.

**`hold` stays, and now checks its own work.** GRBL fills the accessory field
of its status report from the actual output rather than from the modal state,
and it emits that field beside `Ov:` whenever anything is on. So `A:S` is the
controller saying the beam is on, and an `Ov:` field arriving without an
accessory beside it is the controller saying nothing is on. That is evidence,
not inference.

When the beam is still on after the hold, the bridge escalates in order of what
it costs:

1. `0x9E`, the spindle-stop override. Stops the output, and the job stays
   resumable. It is a **toggle** and only acts in HOLD, so it is sent only on
   positive evidence that the beam is on - sent to a controller whose output is
   already stopped, it would switch the laser back on.
2. `0x18`, soft reset. Guaranteed off, job over.

When the controller never says - no `Ov:` field at all - the bridge does not
guess, with one exception: if it has seen `$32=0`, the question is not open.
The output is a spindle and the hold did not touch it, so it resets. Otherwise
it records that it could not confirm and leaves it there. Ending a job on an
absence of evidence is its own kind of unreliable, and `reset` exists for
anyone who would rather.

**And the hazard is watched directly, not only reacted to.** The disconnect
handler acts on a symptom - the client went away - and the hazard it stands in
for has causes that symptom cannot see. LightBurn can hang with its connection
intact and its job half sent. Our own escalation can fail. A controller can sit
in Hold with the output still live. So a standing check runs alongside it, on
the invariant itself: the laser is on and the machine is not moving.

Two conditions, because they carry different risks of being wrong.

- **The laser is on with nobody attached.** There is no legitimate version of
  this, so the grace is one second.
- **The laser is on and the machine has not moved.** Piercing thick material is
  a stationary burn on purpose, and so is aiming the beam by hand with the
  client connected. The grace has to be longer than any of those, which makes
  it the one number worth putting in the configuration:
  `grbl.stationary_beam_seconds`, twenty by default, zero to switch it off.

Both end in the same ladder, and both are silent when
`grbl.on_disconnect` is `none` - that setting is a promise that the bridge
never commands the machine, and a watchdog that acted anyway would make it a
lie.

The check needs a status report that is not stale, and GRBL only speaks when
spoken to. So the appliance asks for one when nobody else has for a second.
During a job the client polls several times a second and the appliance stays
silent; when the conversation stops, it takes over the asking. That is the one
thing it injects into a client's stream, it is read-only, and without it the
watchdog would be reading a number that stopped updating exactly when it
started mattering.

**The appliance asks `$32` before it matters.** Two seconds after the port
opens, with no client attached, it sends `$$` and reads the answer. Read-only,
like the status request, and invisible to any client because there is none. A
controller in spindle mode is then named on the status page and in the record,
before the first job rather than after the first hole.

## Consequences

The default now ends jobs that the previous default would have paused. That is
the intended trade and it is worth stating plainly: `hold` was pausing jobs
that could not be resumed anyway, at the price of a beam whose state nobody had
checked.

`hold` is no longer a simple action - it can end in a soft reset. That is
deliberate: nobody chooses `hold` in order to leave the beam burning, so
verifying the intent was achieved is part of doing it rather than a separate
option. Which branch was taken is in `last_intervention` and the journal.

The escalation depends on facts about GRBL that this project cannot test:
that soft reset kills the spindle, that `0x9E` stops it during a hold, that the
accessory field reflects the real output. The design is arranged so that being
wrong about the first two costs nothing extra - the ladder ends in the
strongest command available either way - and so that the third is checked
rather than assumed. What remains untestable here is testable on a real
machine, which is where it belongs; `tests/hardware/grbl-smoke-test.sh` covers
it.

The stationary check can be wrong in one direction that costs something: a
pierce longer than the configured grace gets a feed hold. The setting exists
precisely so that it can be raised, and the record says what happened and why.
It cannot be wrong in the other direction cheaply - which is why the default is
twenty seconds rather than five.

None of this changes what the appliance is. It is still not an emergency stop,
and it still depends on this daemon running, the link working and the
controller answering.
