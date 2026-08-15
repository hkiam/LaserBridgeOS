# ADR 0018: Taking the machine means taking it completely

- Status: accepted, amends ADR 0013
- Date: 2026-08-15

## Context

Found on a real machine, not in a test: the stationary-beam watchdog stopped a
laser while LightBurn was connected, and LightBurn carried on sending.

That is worse than not intervening. A soft reset empties GRBL's planner and its
128-byte receive buffer and, after motion, leaves the controller in alarm. A
sender that knows none of this keeps counting `ok`s against a buffer that no
longer holds what it believes. What follows is either a stream of `error:9`, or
- if the controller came back idle - G-code executed while the two ends
disagree about where the job is. [ADR 0013](0013-switching-the-beam-off.md) has
a phrase for that: not a failed job, a wrong cut.

The hole is in the reasoning, not in one function. ADR 0013 justified injecting
real-time commands for the case where the client had **already gone**: nothing
is streaming, so nothing can be desynchronised. The stationary-beam watchdog
was then built on the same mechanism for the case where the client is **still
there** - that is its entire purpose, since the hazard it names is LightBurn
hanging with its connection intact. The justification did not carry across, and
nobody noticed because the test that covers this path asserts that the beam goes
out and never asked what the client was told.

Two smaller things came out of the same incident.

The record did not survive. The journal is written to `/data` so that it
outlives a reboot, and it did - on disk. `NewJournal` started with an empty
ring, and the web interface reads the ring, so an incident became unreadable in
the moment it mattered: a beam is stopped, the operator changes a setting, the
bridge restarts, and the account of what just happened is gone from the page
they were looking at.

And the beam reading was being presented as a state. GRBL mentions its outputs
only alongside `Ov:`, which is every tenth report or so, and it fills the
accessory field from the output as it is at that instant. In laser mode with
`M4` the output is genuinely off during rapids and between moves. So "Laser
output: off" appeared next to a machine cutting at S450 - the report was true
and the display was not.

## Decision

**Whoever takes the machine takes it completely.** When the bridge is about to
command a machine that a client is still streaming to, it first writes a
`[MSG:LaserBridgeOS took over: ...]` and then closes the connection. The message
is GRBL's own way of speaking to a sender, appears in LightBurn's console, and
cannot disturb the `ok` accounting because it is not an `ok`. The disconnect is
what actually stops the streaming; the message is what explains it afterwards.

Not for a disconnect the bridge is reacting to - there is no client there - and
not for a newcomer who arrives mid-intervention, who is handed the machine as
before.

The connection the bridge closed is remembered, so the disconnect it causes is
not mistaken for a client walking away and answered with a second ladder into a
controller that was reset a moment ago. It is remembered as the connection
rather than as a flag, because a client leaving of its own accord in the same
moment still has to be handled.

**The record is read back at startup.** `NewJournal` fills the ring from the
file and its rotated generation, oldest first, skipping a half-written last line
- which is what a power cut leaves behind. The `opened /dev/ttyUSB0` entry that
every run begins with is what separates one session from the previous one on
screen, so nothing has to be invented to mark the boundary.

Dropping a client because the bridge took over is recorded as an intervention
rather than as a client event. Routine connects and disconnects stay in memory
on purpose - a workshop laptop reconnecting a dozen times an afternoon would
push the entries worth having out of a bounded file - but this one has to
survive the restart that so often follows it.

**A reading about the outputs carries the time it was taken.** `BeamUnix` is
when the controller last said anything about them, and two things follow.

The web interface stops asserting "off" about a machine that is cutting: it
shows what was said and how long ago, and where "off" contradicts a `Run` state
with a non-zero commanded power it says the reading is not conclusive. The age
is computed from the appliance's own two timestamps rather than from the
browser's clock, which may be years away if the RTC battery is flat.

And the stationary check no longer acts on a memory. By the time a
twenty-second grace has run out, the oldest of its evidence is twenty seconds
old and may be a single observation. That was tolerable while an intervention
cost a feed hold; it is not now that the client is dropped with it, because a
false positive ends the job outright. If the controller has said nothing about
its outputs within the window, the bridge declines - and records that it
declined, rather than being quietly silent about a check it did not make.

## Consequences

An intervention now always ends the job, visibly. That is the honest reading of
what was already happening: a controller that has been reset is not going to
finish what it was doing, and leaving a sender streaming into it only made the
outcome harder to understand.

The stationary check is narrower than it was. A controller that stops mentioning
its outputs entirely will not be acted on, and that is a deliberate narrowing
with a cost: if such a controller really did leave a beam on, this no longer
stops it. The `controllerSilent` reading and the journal both say when that
state has been entered, and `on_disconnect` still ends any job whose client goes
away. Widening it again would mean acting on evidence whose age nobody bounded,
which is what produced the incident this document exists for.

The test that covers the stationary path now reads the client's socket to the
end. That is the general lesson and it is worth stating plainly: it asserted the
laser went out, which was true, and the mechanism was still wrong. A safety test
that checks only the machine has checked half the system.
