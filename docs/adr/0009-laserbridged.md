# ADR 0009: laserbridged, the appliance's own GRBL bridge

- Status: accepted, first stage
- Date: 2026-08-15

## Context

`ser2net` has carried the GRBL connection since the beginning and does it
well. What it cannot do is know anything: it moves bytes and has no opinion
about what they mean. Everything the appliance might want to add — noticing
that a job is running, holding the machine when the operator's laptop
disappears mid-cut, reporting the controller's state to the web interface —
needs a component that understands both ends of the wire.

Replacing a working component is a risk in itself, so this is staged. The
first stage is a transparent proxy that does exactly what ser2net does, and
nothing more.

## Decision

**`laserbridged` is a second Go binary in the existing module.** Not a
separate repository or module: it reads the same `/data/config.yaml` through
the same package, which is the only way "configuration exclusively through
the central LaserBridgeOS configuration" stays true rather than aspirational.

**No new dependencies.** A serial port is three ioctls, and the standard
library's `syscall` package has all of them. `termios2` is used rather than
the older interface because the appliance permits 250000 baud, which has no
`B...` constant; `termios2` takes the number itself, so every rate the
configuration accepts can actually be set.

**The serial port is opened once and held**, not opened per client. Opening a
USB serial adapter toggles DTR, which resets an Arduino-based GRBL
controller. A bridge that reopened the port on each connection would reset
the machine every time LightBurn reconnected — during a job, that is a ruined
workpiece at best. Clients attach to a port that is already open, and the
daemon keeps reading it even with nobody attached so the kernel buffer does
not fill with unread output and greet the next client with stale messages.

**One client at a time, enforced twice.** The daemon serves one connection,
and `TIOCEXCL` asks the kernel to refuse a second opener of the device.
`kick_old_user` decides whether a newcomer displaces the incumbent or is
turned away.

**Both backends ship, and the configuration picks one.**
`grbl.backend` is `ser2net` or `laserbridged`; both services sit in the
default runlevel and each refuses to start unless the configuration names it.
Exactly one owns the serial port, and switching is a configuration change
rather than an image change.

`ser2net` is the default, and it is not going away once laserbridged has
earned the job - see ADR 0011. Shipping both permanently costs a few megabytes
and buys a fallback that does not depend on any of our own code being right.

**Status over a Unix socket** at `/run/laserbridge/laserbridged.sock`, one
line in and one JSON object out. The web backend asks there instead of
opening the device itself, which is what keeps the promise that nothing else
touches the serial port. The protocol has room for the commands that a later
stage will add.

## Consequences

The appliance carries two GRBL backends permanently, which is the point: the
old one remains one configuration change away, indefinitely.

Byte transparency is verified against a pseudo-terminal, which is a real tty
with the same termios semantics as a serial adapter — the tests exercise the
code that will run against hardware. What they cannot prove is timing against
a real controller, so the hardware smoke test remains a manual step.

The daemon has no persistence and no state of its own. Restarting it loses
the byte counters and nothing else.

What this stage deliberately does not do: parse GRBL replies, track machine
state, act on disconnect, or expose commands. Those are the following stages,
and each of them changes what the bridge is allowed to do to a running
machine — they deserve their own decisions rather than arriving as a side
effect of replacing ser2net.
