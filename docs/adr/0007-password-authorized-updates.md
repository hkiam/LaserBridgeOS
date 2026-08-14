# ADR 0007: Password-authorized system updates

- Status: accepted
- Date: 2026-08-14
- Amends ADR 0003

## Context

ADR 0003 made every system update prove its authenticity with a detached
OpenSSH signature, verified against `/data/ssh/authorized_keys`. The owner
signs with the private half of a key the appliance already trusts. No vendor
secret, no cloud.

ADR 0006 then made a key file optional: the appliance ships with a password
and the setup wizard no longer forces the operator to obtain a key. Those two
decisions collided, and the collision was silent.

On an appliance set up with a password only, `authorized_keys` still holds the
public key generated on first boot — but that key's private half is deleted
when setup completes, and the operator never downloaded it. The verifier
therefore finds a key, does not report one missing, and rejects every update
with `update signature rejected: Could not verify signature`. Nobody can
produce a signature that would satisfy it. Worse, the initializer regenerates
a fresh key pair on the next boot, so the pair in `/data/setup` no longer even
matches the authorized entry, and `/api/setup/ssh-key` refuses to hand
anything out once setup is complete. There is no API to add a key afterwards.

The result was an appliance that could never be updated again, failing with a
message that pointed at the signature rather than at the missing credential.

## Decision

**An update is authorized by a signature or by the appliance password.**
Either is sufficient; the bundle's own integrity checks — manifest digests,
sizes, architecture, SquashFS magic — are never skipped, whichever was used.

**The shipped default password cannot authorize an update.** It is printed in
the README, so accepting it would mean anyone who can reach port 80 could
install firmware. The appliance recognises its own default hash and refuses
it, telling the operator to choose a password first.

**Changing the password requires the current one.** This is the part that
makes the rest worth anything. The web interface has no login, so if a new
password could be set without knowing the old one, an attacker would simply
set one and then use it. `PUT /api/system/password` verifies before it
changes, and both that endpoint and a failed update authorisation pause for a
second to make guessing over the network tedious.

**The password is checked as it arrives.** The web interface sends it as the
first multipart field, so a wrong password fails before a few hundred
megabytes of bundle have crossed the network.

## Consequences

The appliance password is now a real credential: it guards SSH, system
updates, and its own replacement. That is a change in kind from ADR 0006,
where it was only a convenience for logging in. It deserves to be chosen
deliberately, which is what the setup wizard asks for.

Signature-based updates are unchanged and remain the stronger option. They
prove the bundle came from the key holder; a password only proves the
uploader knew the password. An appliance that installs updates over a network
the operator does not control should keep using signatures, and can disable
password authentication for SSH entirely.

The password crosses the network in clear text, because the appliance serves
plain HTTP. So does the Wi-Fi passphrase today, and so would any other
credential this interface handled. It is acceptable only under the assumption
this project has made throughout: a trusted local network, never exposed to
the Internet. Terminating TLS would change that calculus and is not in scope
here.

The rest of the interface stays unauthenticated. Configuration, service
control and reboot remain open to anyone on the network. The password gates
the two operations that can replace the running system, not the appliance as
a whole; treating it as a full login is future work, and would want TLS first.
