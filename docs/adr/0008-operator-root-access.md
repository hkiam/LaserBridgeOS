# ADR 0008: Root access for the operator

- Status: accepted
- Date: 2026-08-14
- Supersedes part of ADR 0002 and ADR 0005

## Context

ADR 0002 locked root: no password, no root login, no console. ADR 0005 added
`doas` but granted exactly one program, the deployment helper. The intent was
that the appliance should be operated through its interfaces and never poked
at by hand.

That works until something goes wrong in a way the interfaces do not describe.
Over one session this appliance produced: a `dnsmasq` that exited before
serving a single DHCP lease, a `ser2net` that logged an accepter error at
every boot, and a recovery system that could not be moved onto a different
network. Each was diagnosed from the outside by inference and rebuilt images,
and one of those inferences was wrong for a while. All three would have been
obvious in a minute with `dmesg` and a root shell.

The appliance also has no console — no getty, by design — so there is no
other way in. And `kernel.dmesg_restrict` kept the kernel log from the only
account that can log in at all.

## Decision

**The `laserbridge` account may become root through `doas`, without a
password.** The narrow rule for the deployment helper stays, because
`deploy.sh` needs it non-interactively; the general rule is added beside it.

Without a password, deliberately. An appliance set up with an SSH key has no
account password its owner knows, so requiring one would put root out of
reach of exactly the person the rule exists for. A password would also add
little: there is one account, and it is the owner's.

**`kernel.dmesg_restrict` is turned off.** Hiding the kernel log from a user
who can become root protects nothing and costs the first thing anyone reads
when a headless machine misbehaves.

Root login over SSH stays disabled, empty passwords stay rejected, and
`AllowUsers laserbridge` stays. Those keep the surface to one account rather
than defending that account from itself.

## Consequences

Whoever can log in as `laserbridge` is root. That is a real reduction in
depth compared to ADR 0002, and it is accepted: on this appliance the web
interface already has no login at all, so an attacker on the local network
does not need SSH to do damage. The remaining protection is the SSH
credential itself — which is now worth more, and worth choosing well.

An appliance exposed to a network its owner does not control should have SSH
password authentication switched off on the System page and use a key. That
was already the advice; it matters more now.

The recovery session (ADR 0005) already granted root for the same reason. The
installed system now matches it, which also removes a confusing asymmetry
where the same command worked in one mode and not the other.
