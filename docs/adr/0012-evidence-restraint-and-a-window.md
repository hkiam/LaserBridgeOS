# ADR 0012: Evidence, restraint, and a window

- Status: accepted
- Date: 2026-08-15

## Context

[ADR 0009](0009-laserbridged.md) built a bridge that understands GRBL, and
[ADR 0011](0011-ser2net-stays.md) settled that ser2net remains the fallback
forever. That combination sets the bar for everything built on top: a feature
is worth having if it makes the appliance better at its job, and it is only
allowed if falling back to ser2net still leaves a working laser cutter.

Three things clear that bar, and they are variations on one theme - the
appliance now knows things it used to be blind to, so it should say what it
knows and stop doing things that contradict it.

## Decision

**A record of what went wrong, kept across a reboot.** Alarms with their code,
errors with the G-code line that caused them, controller resets, the adapter
disappearing, clients arriving and leaving, and every intervention the bridge
made. Two hundred events in memory, the notable ones appended to
`/data/laserbridge/bridge-journal.log`, rotated at 256 KB.

Recording the offending line is the part that needed the outgoing direction to
be watched as well. GRBL answers `error:9` and names no line number, so unless
the bridge kept what it had just sent, the code is a riddle. Four lines are
enough and cost nothing.

Status reports are not recorded. There are several a second during a job and
they would bury the three lines that matter.

**Nothing to do with the record may endanger the bridge.** Write failures are
silent: a full or read-only `/data` costs the history, not the machine. Client
comings and goings stay in memory rather than causing a disk write every time
a laptop's Wi-Fi wobbles.

**Three actions ask before interrupting a job.** Saving GRBL settings restarts
the bridge; installing an update rewrites a root slot; rebooting is obvious.
All three now refuse with 409 while the machine is moving, and all three take
`force=true` from anyone who means it.

The default when the appliance cannot tell - ser2net in charge, daemon not
answering - is to allow. An appliance that refused to reboot because it was
unsure would be worse than one that never asked. And only the GRBL section of
the configuration triggers the check: telling someone they may not adjust the
camera during a job teaches them to reach for `force=true` out of habit.

**A read-only monitor port, off by default.** Diagnosing a GRBL problem means
watching what LightBurn and the controller actually say to each other, and
until now the only way to do that was to take the port away from LightBurn -
which changes the situation being investigated. `grbl.monitor_port` serves a
marked transcript of both directions. Whatever a watcher sends is read and
discarded; a watcher that stops reading is dropped rather than waited for.

**The wrong baud rate is named.** It is the most common setup mistake, and
from the bridge's side it is unmistakable: bytes arrive and none of them parse
as GRBL. Reported once there is enough to judge, so that one stray byte on a
good link cannot raise it.

## Consequences

The journal is the clearest single answer to "why not ser2net". It costs
nothing while things work and is the only thing in the appliance that can
answer "what happened at 03:12" afterwards.

None of it is load-bearing for cutting. Switching to ser2net loses the record,
the refusals and the monitor port, and leaves a machine that runs - which is
the test ADR 0011 sets.

Two of these hand out information over interfaces that are not authenticated:
the status socket is world-readable on the appliance and the monitor port is
open to the workshop network. What they expose is G-code and machine state on
a network the operator already trusts with an unauthenticated laser cutter on
port 23. The monitor port stays off unless asked for, which keeps the default
attack surface where it was.

`force=true` is a guard against a slip, not a permission system. Anyone
determined to reboot mid-job can, and should be able to.
