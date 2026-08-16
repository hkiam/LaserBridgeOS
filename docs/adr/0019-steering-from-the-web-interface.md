# ADR 0019: Steering from the web interface

- Status: accepted
- Date: 2026-08-16

## Context

The GRBL section of the web interface had grown into one page holding three
unrelated things: what the machine is doing, how the bridge is configured, and
the record of what went wrong. They are consulted at different moments by
someone in a different frame of mind - standing at the machine, sitting down to
set it up, working out what happened last night - and none of them wants to
scroll past the others.

The operator also asked for something the appliance has never done: a jog pad
with homing, a low-power beam for aiming, and a stop button.

That last part is a change of kind, not of degree. Everything this appliance
does to a machine today is something it decided to do itself, narrowly, with a
written justification - [ADR 0010](0010-what-the-bridge-may-do-on-its-own.md)
and [ADR 0013](0013-switching-the-beam-off.md) are entirely about how narrow.
Steering is the machine doing what somebody at a web page asked, and that page
has no login at all ([ADR 0014](0014-no-login-on-the-web-interface.md)).

No login was defensible while the worst the interface could do was restart a
service: anyone who can reach port 80 is on the workshop network and can pull
the plug anyway. A page that can move an axis and switch a laser on is not in
that class.

## Decision

**Three pages.** *Machine* is the reading and the steering, *GRBL settings* is
the configuration, *What happened* is the record. The nav says what each is for.

**Steering is bounded by what it will not do, not by who is asking.** Adding a
login would be a larger change than this one and would not obviously help: the
credential would be the same appliance password that is already root over SSH,
typed into a page on an unencrypted connection. The guards are these instead:

- **Nothing at all unless laserbridged owns the port.** ser2net cannot say what
  the machine is doing and has nowhere to put a command.
- **Nothing while a client is connected**, with one exception below. Two
  applications steering one laser is the failure this appliance exists to
  prevent, and a jog injected into LightBurn's stream would also hand it an
  `ok` it never earned - the same corruption ADR 0013 forbids for `$$`.
- **The exception is Stop**, because it ends the conflict rather than joining
  it. Whoever presses it means the machine to be stopped, and a controller that
  has been reset will not finish what it was sent - so Stop takes the machine
  over exactly as the watchdog does (ADR 0018): the client is told and let go.

**The aiming beam is held on a lease, not switched on.** A beam that is on with
nobody connected is precisely what `TriggerBeamUnattended` exists to stop, and
it would stop it within a second. Rather than carve an exception out of the
watchdog - which would mean the watchdog no longer watching the one case it was
built for - the operator holds a three-second lease that the page renews every
second. While it is held, the beam counts as attended. When it lapses, the beam
is switched off and the watchdog is watching again.

The lease is a dead man's handle, and it is the right shape for the hazard: a
closed laptop, a dropped Wi-Fi link, a browser tab that crashed, a page left on
a phone in a pocket - all of them end with the beam out, without anyone having
to remember. A switch would leave a laser on behind a browser that is no longer
there.

**The stop button is not called an emergency stop.** It is called "Stop (soft
reset)" and it says what it is under it. This is not pedantry. ADR 0010 states
plainly that this appliance is not an emergency stop and will not become one -
the chain is this appliance running, the network, the cable and the controller
answering, and no part of that belongs in the path between a person and a fire.
A button labelled *Notstopp* invites somebody to reach for a browser instead of
the red mushroom on the machine, and the two seconds that costs are the two
seconds that matter. The label is the safety feature.

**Commands are refused to anyone but root over the status socket.** That socket
is world-writable because "the status is not a secret" and because
`laserbridge grbl-status` should not need doas. The argument does not extend to
steering, so the daemon asks the kernel who is at the other end. On this
appliance the distinction is currently theoretical - one account, and it may
become root without a password - but the permissions were justified by an
argument, and the argument has a boundary.

## Consequences

The appliance can now start a hazard, where before it could only fail to stop
one. That is a real widening and it is why the guards above are structural
rather than advisory: they are in the daemon, not in the page, and the page only
reflects them.

Jogging and homing move a machine with no interlock in the path. So does the
machine's own control panel, and an operator standing at a laser with a browser
open is in the same position as one standing at it with a pendant - but the
browser can be in another room, and nothing here can tell the difference. The
honest limit is that this is a convenience for someone who can see the machine,
and the interface does not pretend otherwise.

`$30` is now learned along with `$32`, so a percentage of laser power is a
percentage of what this controller actually calls full power rather than of
GRBL's default. Where the controller never said, the default is assumed and the
figure is what it always was.
