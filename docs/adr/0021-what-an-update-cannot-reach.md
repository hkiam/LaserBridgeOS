# ADR 0021: What an update cannot reach

- Status: accepted
- Date: 2026-08-16

## Context

Installing 0.1.5 on the workshop appliance and rebooting it left it off the
network for forty-five minutes. It came back only when the power was cycled.
The record says exactly what happened, and it is not what it looked like:

```
13:58:58  laserbridged: shutting down / state STOPPED
13:58:58  laserbridge-watchdog: watchdog disarmed
13:58:58  syslogd exiting
14:43:40  Linux version 6.12.103-0-lts …          (the next boot)
```

The shutdown was orderly to the last line. `grubenv` afterwards held
`laserbridge_try=0` and an empty override, so GRUB did not run in between
either - two unconfirmed attempts would have left `try=2` and a slot flip. The
board accepted the reset request, switched off, and stayed off. The hardware is
a Z83-F; small x86 boards of that generation are known for it.

Investigating that turned up a second thing. This appliance has never had
`usbcore.autosuspend=-1` on its kernel command line, although
[ADR 0015](0015-usb-autosuspend-is-off.md) put it there months ago:

```
Command line: BOOT_IMAGE=(hd0,gpt1)/boot/slot-b/vmlinuz-lts root=LASERBRIDGE_ROOT_B
              laserbridge.slot=b rootfstype=squashfs ro rootwait quiet loglevel=3
```

The updater writes the inactive slot's kernel and initramfs, `active-slot.cfg`
and `grubenv`. `grub.cfg` is embedded in the GRUB EFI binary on the ESP, and no
bundle has ever touched it. So the boot policy on a running appliance is
whichever one was flashed onto it, and every boot-level decision this project
has made since - the kernel parameter, the boot-attempt counter, the slot
layout - reaches a device only by reflashing.

## Decision

**Both are documented rather than fixed.**

The reboot could be made to recover itself by leaving the hardware watchdog
armed while the reset is requested, so that a board which declines to restart
is reset thirty seconds later. That is a real fix and it is not a small one: it
has to tell a reboot from a poweroff, or a deliberate shutdown becomes a reboot
loop, and it would be the first thing in this appliance that deliberately
leaves a watchdog running with nobody to feed it. The frequency does not
justify it - one occurrence, on hardware that is powered on the bench next to
the person installing the update.

Making the ESP updatable would let `reboot=pci` and any future boot parameter
be delivered. It is also the one write on this appliance with no A/B behind it:
a bundle that damages the ESP leaves a device that does not boot at all, which
is precisely the failure the A/B design exists to make impossible. That trade
needs a better reason than it currently has.

So the README says both things plainly, in the update section, and the System
page says the first one where the bundle is uploaded: install while you can
reach the appliance.

## Consequences

An update is a job for someone who can put a hand on the machine. That is worth
saying out loud rather than discovering once, which is the whole reason this
ADR exists.

Nothing at the boot level ships to an existing appliance. Anything decided
there - and there will be more, because that is where kernel parameters and the
recovery counter live - applies to newly imaged devices and to nobody else
until this decision is revisited. The wording in the README is deliberately the
kind somebody will find while wondering why a documented kernel parameter is
not on their kernel command line.
