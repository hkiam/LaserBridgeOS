# ADR 0015: USB autosuspend is off

- Status: accepted
- Date: 2026-08-15

## Context

The GRBL controller reaches this appliance through a USB serial adapter, and
Linux is entitled to suspend a USB device that has gone idle and wake it on the
next access. The wake-up is not free. Cheap CH340 and CP210x bridges are
routinely reported to lose the first characters written after a resume, and
some re-enumerate instead of resuming, which moves `/dev/ttyUSB0` to a
different number under a bridge that is holding the old one open.

GRBL parses its input line by line and its sender counts `ok` replies against a
128-byte receive buffer. A few characters lost in the middle of that stream is
not a job that fails visibly; it is a line the controller reads as something
other than what was sent, on a machine with a laser on it. That is the same
class of failure ADR 0013 spent its length avoiding, arriving through a
different door.

Against that stands what autosuspend buys here: a fraction of a watt on a
fanless appliance that is bolted to a laser cutter and plugged into the wall.
There is no battery in this picture.

The idle window is also not hypothetical. Between two jobs, or while LightBurn
sits connected but silent, the adapter sees no traffic for as long as the
operator takes — and the first write after that pause is the start of a cut.

## Decision

**Autosuspend is off by default, and the default is stated in two places.**

`usbcore.autosuspend=-1` on the kernel command line covers devices probed
during boot. It has to be there because the alternative — userspace — cannot
run that early: `/data` is not mounted, the configuration is not readable, and
the adapter is probed long before anything can have an opinion about it.

`system.usb_autosuspend` covers everything after that, applied by
`laserbridge apply` at boot and on every configuration change. It is a setting
rather than a constant for two reasons: a device whose driver depends on
runtime power management is a real thing, and measuring what this appliance
draws is a legitimate exercise that should not require rebuilding an image.

**Applying it means writing two kinds of knob, not one.** The usbcore module
parameter is a default handed out at probe time; writing it changes nothing
about the adapter that has been plugged in since boot — which is precisely the
device this exists for. So the policy is also walked across
`/sys/bus/usb/devices/*/power/`, where each already-probed device keeps its
own `control` and `autosuspend_delay_ms`. Setting only the first one would have
looked correct in every test and done nothing on the appliance.

**A failure to apply it does not fail the configuration change.** A backend
started without root cannot write `sysfs`, a container mounts it read-only, and
a kernel without `CONFIG_PM` does not have the file at all. None of those is a
broken configuration, and rolling a hostname change back over a knob that was
never reachable would be the worse outcome.

**But it is not silent.** `/api/status` reports what usbcore answers next to
what was asked of it, and the System page shows both. A setting that did not
take is visible on the page that offers it, rather than only in the behaviour
of a laser three weeks later.

**Turning it back on while a job runs asks first.** Enabling it reaches the
adapter carrying the job, so it joins the small set of operations that refuse
during motion and take `force=true` for an answer. Turning it off never needs
to ask; that direction cannot cost anything.

## Consequences

The appliance keeps every USB port awake, including the root hubs, which means
the USB controller never runtime-suspends either. That is the intended trade
and it is the whole cost.

The kernel command line is not reachable by an update. Update bundles carry the
root filesystem, the kernel and the initramfs; the command line lives in the
GRUB image on the ESP, written at install time. An appliance updated in place
therefore keeps the command line it was installed with.

So the default is stated a third time, in the one place updates do replace:
`/etc/modprobe.d/laserbridge-usb.conf` carries `options usbcore
autosuspend=-1`, and that file is also listed in the mkinitfs feature so it is
present in the initramfs. Which of the copies does the work depends on when
`usbcore` is loaded - the initramfs loads it when the root device hangs off
USB, the coldplug in the sysinit runlevel otherwise - and the point of writing
it in both is not having to know. The driver that matters here, the USB serial
adapter's, is loaded by the coldplug and reads the root filesystem's copy.

A module option only applies at load time, which is why none of this replaces
the userspace half: `laserbridge apply` is the only mechanism that can express
a *configured* value, and the only one that reaches a device already probed.

The general shape is worth stating beyond this setting: **boot policy that has
to be changeable belongs where updates can reach it.** As a sysctl where it can
be one - that is where the panic settings in
[ADR 0017](0017-a-last-resort-for-a-frozen-kernel.md) went - as module options
or in the initramfs otherwise, and on the kernel command line only as a
belt-and-braces default for freshly installed images. Putting the ESP payload
into update bundles would remove the restriction, and would mean a bootloader
that must never be half-written on a headless appliance with no second copy of
it. That is a separate decision and not this one.

A build check asserts that both A/B command lines still carry the parameter.
Like the ser2net check in ADR 0011, this is a default that nothing would
notice the loss of until a specific adapter, on a specific job, dropped a
specific byte.
