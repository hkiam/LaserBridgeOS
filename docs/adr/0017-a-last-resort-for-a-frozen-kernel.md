# ADR 0017: A last resort for a frozen kernel

- Status: accepted
- Date: 2026-08-15

## Context

The appliance recovers from a lot, and each mechanism recovers from something
specific. `supervise-daemon` restarts a process that exits. `BridgeWatch`
notices a GRBL bridge that is running but no longer answering and restarts it
without a reboot. The GRUB boot-attempt counter picks the other slot after three
boots that never reach userspace. [ADR 0013](0013-switching-the-beam-off.md)
watches the machine itself and switches off a beam that is on while nothing
moves.

Every one of those needs the kernel to still be scheduling userspace.

Nothing covered the case where it is not. A lockup, a driver spinning with
interrupts disabled, a livelock under memory pressure: the appliance stops, and
whatever the laser was doing it keeps doing. ADR 0013 spends its entire length
arguing that a beam must not be on while nothing moves, and in this one state it
has no mechanism behind it at all - not a degraded one, none.

It is also the state a headless appliance cannot be talked out of. There is no
console, no login, and nobody in the workshop at three in the morning.

## Decision

**A hardware watchdog, fed by its own service.** `laserbridge watchdog` holds
`/dev/watchdog` open, asks the driver for a thirty-second timeout, and writes to
it every ten seconds. If the writes stop, the board resets. On the way back
through the reboot the USB bus is re-enumerated, which toggles DTR, which resets
an Arduino-based controller - and a soft reset is the one command whose
beam-off behaviour is not a matter of configuration.

**It does not judge.** It pets the watchdog for as long as it is scheduled, and
requires nothing else of the system. An earlier draft tied the keepalive to a
liveness check - the web backend answering, the bridge responding - and that is
a worse trade than it sounds. The web service can be stopped from its own
interface; the bridge can be stopped on purpose to switch backends. A watchdog
whose false positive costs a reboot in the middle of a cut must have no
plausible false positives, and hung *processes* are already handled one layer
up, where restarting one costs a connection instead of a job.

For the same reason the feeder asks the out-of-memory killer to leave it alone.
An OOM the kernel survives is exactly the situation this must not escalate into
a reset.

**It is its own service, not a goroutine in the web backend.** Closing
`/dev/watchdog` is how a watchdog is disarmed, so hosting it inside a process
that is restarted for ordinary reasons - a configuration change, an update -
would quietly remove the last resort every time. Stopping the service on purpose
writes the magic close character first, so a deliberate stop does not reboot the
appliance thirty seconds later. A kernel built with `CONFIG_WATCHDOG_NOWAYOUT`
ignores that and reboots anyway; that is that kernel's stated policy and not
something to work around.

**A machine without a watchdog device says so.** Most virtual machines have
none. The service refuses to start with a message naming what is missing, rather
than looking armed while it is not.

**Panic behaviour is set as sysctls.** `kernel.panic_on_oops=1` and
`kernel.panic=10`: treat an oops as fatal, reboot ten seconds later. A kernel
limping on after an oops is one whose next surprise arrives while a laser is
running, and there is nobody here to read the trace off a screen.

These are sysctls and not kernel command line arguments on purpose. The command
line lives in the GRUB image on the ESP, which is written at install time and
never by an update, so anything put there reaches new installations only.
`/etc/sysctl.d` is in the root filesystem, which every update replaces. **Boot
policy that has to be changeable belongs where updates can reach it** - and
where it cannot be a sysctl, it belongs in the initramfs, which ships per slot
in the bundle, rather than on the command line.

## Consequences

The reboot ladder now has three rungs with different costs, and they are ordered
by what they take away: restart the bridge (a client's connection), reboot after
a panic (the job), reset the board (the job, and any filesystem writes in
flight). `/data` is ext4 with `errors=remount-ro` and everything of consequence
is written through `atomicfile`, so the last of those costs settings written in
the same second, not the configuration.

The watchdog cannot help with hardware that has stopped answering: if the USB
controller is wedged in a way a re-enumeration does not clear, or the laser's
own power supply keeps the tube driven, the reset changes nothing. This closes
the gap where *software* is dead. The permanent caveat from ADR 0010 stands
unchanged - this appliance is not an emergency stop, and there will be no
hardware interlock.

A board with no watchdog now boots with one service in a failed state, visibly.
That is the honest reading of the situation and it is preferable to the previous
one, where the same appliance looked exactly like a protected one.

The boot-policy check asserts the service is installed, that it is in a
runlevel, and that both panic sysctls are set. As with the ser2net rule in
ADR 0011, none of this survives a year of refactoring unless something fails
when it stops being true.
