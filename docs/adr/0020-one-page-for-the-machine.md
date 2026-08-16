# ADR 0020: One page for the machine, and a console on it

- Status: accepted
- Date: 2026-08-16
- Amends: [ADR 0019](0019-steering-from-the-web-interface.md), which split the
  same material across three pages

## Context

[ADR 0019](0019-steering-from-the-web-interface.md) split the GRBL section into
three pages - *Machine*, *GRBL settings*, *What happened* - on the argument that
they are consulted at different moments by someone in a different frame of mind.
That argument was about the settings and the record, and for those it holds. It
was wrong about everything else.

Standing at a laser, an operator holds four questions at once: where is the
head, what is it doing, what does the camera see, and what did it just say. The
answers were on three pages and a fourth (Camera), and moving between them meant
losing sight of the machine. The operator said so plainly: it should all be
under Machine.

Two of the four had no answer at all.

**Where the head is.** The page showed one line of coordinates in whichever
system GRBL happened to be reporting. But an operator sets a work zero and works
to it - that is what `G54` is for, it is what every sender in this field puts in
front of the user, and it is what a go-to target would be typed in. The parser
threw `WCO:` away, so the appliance could not show work coordinates at all, and
had no way to set the zero it was not showing.

**What it just said.** The transcript existed - the monitor port has carried it
since ADR 0012 - but only to whoever knows to run `nc laserbridge 23000` from a
terminal. "Did it get my command, and what did it answer?" is the question asked
most often at a machine and the one the interface answered least.

## Decision

**Everything about operating the machine is on the Machine page.** The readout,
the steering, the camera image and the console, in two columns: where it is and
what it is doing on the left, what it looks like and what it is saying on the
right.

**The record and the bridge settings are on the same page, folded away.** They
belong to the machine and they are not needed while standing at it, so they are
expandable sections rather than pages of their own - with the last eight entries
of the record visible when opened. The record keeps a page of its own as well,
reachable at `#journal`, and a button opens it in a second window to leave open
beside the machine. That is what "its own window for details" means here: not a
navigation entry, a window.

**Work coordinates lead; machine coordinates are underneath in small type.** The
parser now keeps `WCO:` and the observer carries both systems, deriving the one
the controller did not send. Both are shown because they answer different
questions - the operator works in work coordinates, and the limits, the record
and an alarm's position are in machine coordinates.

**Zeroing an axis is offered, and it is the one command that outlives the
session.** `G10 L20 P1 X0` writes the offset to the controller's EEPROM. A
readout in a coordinate system nobody can set is a readout of the wrong thing,
and the alternative is an operator typing it from memory.

**A go-to is a jog, not a `G0`.** `$J=G90 G21 X… Y… F…` rather than
`G90 G0 X… Y…`, for three reasons that all matter: a jog never changes the modal
state, so it cannot leave the machine in `G91` for whatever runs next; it is
answered when it is accepted rather than when the move ends, so the pad stays
responsive; and it is cancelled by the same `0x85` as any other jog, which a
`G0` would ignore. An axis left blank is left where it is - a go-to that
silently added `Z0` because a box was empty would drive the head into the bed.

**The console is the monitor port for people who do not have a terminal.** Same
marked transcript, same three marks: `>` from the client, `<` from the
controller, `*` the appliance's own commands. Filterable by mark and by text,
because during a job the interesting line is one in three hundred.

**It is recorded only while somebody is reading it.** The transcript is
assembled in the goroutines that carry the bytes between the client and the
machine, and doing that work during every job for nobody's benefit is not
acceptable on that path. So a reader holds a lease - the same shape as the one
that holds the aiming beam - which polling renews and a closed tab drops within
fifteen seconds. The ring holds four hundred lines and says how many fell out of
it rather than presenting a transcript with a hole in it.

The console is not the record. The journal is the record, it is on disk, and it
survives the reboot it caused; the console is a window onto a stream.

**An operator may type a command, bounded by what can be expressed rather than
by what it means.** One line, printable ASCII, at most eighty characters, sent
through the same path as every other command: only with `laserbridged` in
charge, only with no client connected, counted out and back against the
controller's buffer, and only from root over the status socket. Whether `G1 X500`
is sensible needs to know the machine, the material and the intent; the
appliance knows none of the three and the operator knows all of them.

The real-time keys `?`, `!` and `~` are refused with a sentence pointing at the
buttons that account for them properly. A soft reset typed into that box would
leave the appliance's own line bookkeeping believing in commands the controller
had already forgotten.

## Consequences

The navigation is shorter by two entries and the Machine page is long. That is
the right trade for a page used while standing at a machine: scrolling past
something is cheaper than losing sight of the reading.

Polling the console costs a request a second while the page is open on it, and
keeps the daemon assembling lines for that whole time. Both stop when the panel
is not showing, which is why the panel - not the page - is what starts it.

Typing commands is the widest thing this interface can do, wider than the jog
pad, and it is deliberately not made narrower by guessing at intent. The guards
that matter are the ones that were already there: nothing while somebody else is
steering, and nothing at all through ser2net.
