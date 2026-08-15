# ADR 0011: ser2net stays

- Status: accepted
- Date: 2026-08-15

## Context

[ADR 0009](0009-laserbridged.md) shipped both GRBL backends and said the
default would stay `ser2net` "until the new one has earned the job". That
phrasing implies an ending: laserbridged proves itself, ser2net is removed,
the appliance carries one bridge again. That ending is now explicitly not the
plan.

The two programs are not the same kind of thing. `ser2net` moves bytes and has
no opinion about them, and it has been doing that in the field for two
decades. `laserbridged` reads the stream, tracks machine state, and can send a
feed hold to a machine that is cutting. The second one is more useful and has
strictly more ways to be wrong - and it is our code, maintained by one person,
exercised on one workshop's hardware.

The failure that matters is not "laserbridged has a bug". It is "laserbridged
has a bug and the workshop cannot cut anything until somebody fixes it".

## Decision

**Both backends ship in every image, permanently.** Removing ser2net is not a
future cleanup task. The image cost is a few megabytes on a 192 MiB root slot,
which is not a constraint worth trading a fallback for.

**The fallback must work without the web interface.** Switching backends is a
line in `/data/config.yaml` and two `rc-service` calls, both reachable over
SSH. If laserbridged is broken badly enough to take the web interface with it,
the way back does not go through the web interface. The README carries the
literal commands.

**A build check enforces it.** Nothing about "we keep the fallback" survives
contact with a year of refactoring unless something fails when it stops being
true, so the boot-policy check verifies that both services are installed and
in the default runlevel.

**laserbridged may become the default; it may not become the only option.**
Earning the job means changing which one new images select, not deleting the
other.

## Consequences

Every feature laserbridged gains is a feature the appliance can lose by
falling back, and that is the correct trade. An operator who switches to
ser2net gives up machine state, the disconnect hold and the status page, and
keeps a laser cutter that works.

It also sets a standard for what may be built on top of the bridge: anything
that becomes load-bearing for cutting at all - as opposed to load-bearing for
convenience - is in the wrong place. Persistent job state, for instance,
cannot live only in laserbridged if falling back to ser2net is meant to leave
a working machine.

Two backends means two paths to keep working, and the status page has to be
honest about which one is running rather than assuming. That is already the
case: `/api/status` reports the selected backend and both services' state.
