# ADR 0006: Password login by default, keys as an option

- Status: accepted
- Date: 2026-08-14
- Supersedes part of ADR 0002

## Context

ADR 0002 made the appliance key-only: a per-device Ed25519 key generated on
first boot, downloaded through the setup wizard, with no password anywhere.
That is the right default for something reachable from a hostile network.

First use on real hardware showed it is the wrong default for this one. The
setup hotspot triggers the captive-portal detection of macOS and iOS, which
open a stripped-down web view rather than a browser. That window cannot save
downloads: the wizard's key download silently did nothing while the button
changed to "Download again", so the only credential the appliance would
accept could not be obtained at all. The operator was locked out of a device
sitting on the bench in front of them.

The wider point is that the threat model did not match the deployment. This
appliance drives a laser in a workshop, on an isolated network, and its own
web interface has no authentication whatsoever — anyone who can reach port 80
can already reconfigure it, install a signed update, or reboot it. Demanding
key-only SSH while leaving that door open was security theatre with a real
usability cost.

## Decision

**Password authentication is on by default**, and the image ships with the
documented default password `laserbridge` for the `laserbridge` account. The
appliance is therefore reachable over SSH immediately, with no key file and
no wizard.

The hash is precomputed with a fixed salt and written into `/etc/shadow` at
build time. Generating it during the build would use a random salt and make
the image non-reproducible, which the build otherwise takes some care to
guarantee. A salt protects a password that is printed in the README from
nothing.

**The setup wizard offers three ways** and defaults to the first: set a new
password, download the generated key, or paste an existing public key. A
password-only setup no longer touches `authorized_keys`, so keys installed
earlier — including the deployment key of a RAM session, see ADR 0005 —
survive it.

**Key authentication is unchanged and always available.** Nothing about the
generated key, the A/B update signing, or `AllowUsers laserbridge` changes.
Root login stays disabled and empty passwords stay rejected; a test asserts
both, so they cannot be relaxed by accident along with the password default.

**The wizard no longer depends on downloads working.** The generated key is
always displayed as selectable text next to the download button, and a
captive-portal window is detected and named, with a pointer to open
`http://10.42.0.1` in a real browser.

## Consequences

Every image carries the same known password until someone changes it. That is
a fleet-wide secret, exactly what ADR 0002 set out to avoid, and it is
accepted here as a deliberate trade for a device on an isolated network: the
wizard asks for a new password on first boot, and the README states the
default plainly rather than hiding it.

An appliance exposed to an untrusted network should have password
authentication switched off on the System page after installing a key. The
configuration flag for that already existed and is unchanged; only its
default moved.

The captive-portal window remains a poor place to run a wizard. Showing the
key as text works around the download restriction, but the reliable path is
still a normal browser, and the wizard now says so.
