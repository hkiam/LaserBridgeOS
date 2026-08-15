# ADR 0014: No login on the web interface

- Status: accepted
- Date: 2026-08-15

## Context

The web interface has no authentication. It has CSRF tokens and a same-origin
check, which stop a page in another tab from acting on the appliance, and
nothing that stops a person on the same network. Anyone who can reach port 80
can change the configuration, restart services and reboot the machine.

That has been true since the beginning and was never written down, which makes
it a state rather than a decision. This is the decision.

## Decision

**No login, on this network.** The appliance is a workshop device on a network
the operator already controls. On that same network, port 23 accepts G-code
from anybody without so much as a handshake - that is what LightBurn connects
to, and it is what a bridge for LightBurn has to be. Somebody who can reach the
web interface can already drive the laser directly, so a login on the web
interface protects the settings of a machine whose controls are open.

Adding one would also cost something real: this is the interface an operator
reaches for when something is wrong, sometimes from a phone, sometimes through
a captive-portal window during first-boot setup, and a forgotten password on a
headless appliance is a reinstall.

**Two things are protected anyway, because they are not symmetrical with
driving the laser:**

- Installing an update needs the appliance password (ADR 0007). An update is
  arbitrary code on the root slot, which is a different order of access from
  moving an axis.
- Changing the appliance password needs the current one.

**The network is the boundary, so the appliance says so.** The setup flow
already pushes towards a configured Wi-Fi rather than the open setup hotspot,
and the documentation states plainly that anyone who can reach the appliance
can operate the machine.

## Consequences

This is only defensible while the premise holds. If the appliance is ever put
on a network the operator does not control - a shared workshop, a guest VLAN,
anything routed to the internet - it is wrong, and no part of the current
design would notice. That is the condition to re-open this decision, not the
arrival of a better login form.

It also sets the boundary for what may be added. Anything that reaches beyond
the machine - outbound notifications with credentials in them, remote access,
an API key for a cloud service - is not covered by "the network is trusted" and
needs its own answer.

The status socket and the read-only monitor port sit inside the same boundary:
world-readable on the appliance and open on the LAN respectively, both carrying
G-code and machine state, neither more sensitive than the port that accepts the
G-code in the first place. The monitor port stays off by default regardless.
