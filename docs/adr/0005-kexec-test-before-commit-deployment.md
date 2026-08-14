# ADR 0005: Test-before-commit deployment over kexec

- Status: accepted, partially blocked by the stock kernel
- Date: 2026-08-14

## Context

Installing or replacing an appliance image meant attaching a USB stick, a
monitor and a keyboard to a device that is otherwise headless and often
mounted next to a laser. The A/B update path (ADR 0003) covers updating an
already-installed appliance, but it cannot answer the question that matters
before committing: does this image actually work on *this* hardware?

What was wanted is a test-before-commit loop that runs entirely over SSH: boot
a candidate image from RAM, judge it on the real device, and then either
reboot back into the untouched installed system or write that exact image to
the disk.

Writing the system disk cannot be done from a system running off it. Something
has to run from RAM first.

## Decision

**The recovery system is the appliance image itself, booted from RAM.** No
second build, no separate recovery artefact. `deploy.sh` prepends
`root.squashfs` to `initramfs-lts` as its own uncompressed cpio segment - the
arrangement the kernel already uses for early microcode - and boots that.

Two kernel command line flags select the behaviour, parsed from
`/proc/cmdline` by a hook sourced from Alpine's `initramfs-init`, because
Alpine only turns command line options from a fixed allowlist into variables:

- `laserbridge.ramboot=1` mounts the SquashFS from the initramfs as the lower
  layer of an overlay with a tmpfs on top.
- `laserbridge.mode=recovery` additionally drops the appliance services.

**The system disk is made unreachable rather than merely left alone.** While
still in the initramfs, the hook rewrites `/etc/fstab` in the writable overlay
so the data partition becomes a tmpfs, and removes `laserbridge-boot-confirm`
from the default runlevel - that service would otherwise mount the boot
partition and rewrite the active slot, which is right for an installed system
and wrong for every RAM boot. A test mode that promises not to touch the disk
should not depend on every later code path being careful.

**The artefact set is shared between testing and installing.** `--test` boots
the SquashFS from the `.lbu` bundle; `--install` writes the disk image whose
slot A contains that same SquashFS. Both are checked against the manifest
digests before anything is transferred.

**`--install` erases the whole disk**, `/data` included, and streams the image
from the operator's machine straight onto the block device without staging it
on the target. The write is read back and compared before the reboot; any
failure stops short of rebooting.

**Privileges are narrow.** ADR 0002 keeps root login disabled. Rather than
opening it, the `laserbridge` account may run one program through `doas`:
`laserbridge-deploy`, which implements the privileged operations as a fixed
set of subcommands and refuses to write a device that has mounted partitions.
On a foreign Linux the same helper is uploaded and run through `sudo`, so
there is exactly one implementation of the privileged half.

## Consequences

The appliance image grows by roughly 13 MiB uncompressed for `kexec-tools`,
`doas`, `lsblk` and keeping `zstd`, which the build previously removed after
generating the initramfs. The 256 MiB root slot has ample room.

A RAM boot needs about twice the combined initramfs plus headroom - roughly
1 GiB for the current 178 MiB image - because the kernel holds the initramfs
while unpacking it. `deploy.sh` checks this before transferring.

Allowing kexec at all means a holder of the appliance's SSH key can boot an
arbitrary kernel on the device. That is a real widening of what the key can
do. It is accepted because the key is the owner's credential and the web API
already exposes unauthenticated reboot and signed-update installation on the
local network; the deployment helper is the narrowest gate that still permits
the workflow.

Writing the image faithfully leaves the disk with the image's layout: on a
larger disk the space beyond the image stays unallocated and the GPT backup
header remains where the image put it. Growing the data partition after
installation is not addressed here.

### The stock kernel blocks the trigger

Alpine's `linux-lts` starts with `kernel.kexec_load_disabled = 1` - not from
any sysctl file, and with lockdown inactive - and that sysctl only accepts
being written `1`, so it cannot be re-enabled. `CONFIG_KEXEC_FILE` is unset,
so `kexec -s` is unavailable as well. `kexec -l` therefore fails with
`Operation not permitted` on a LaserBridgeOS host, and no change to the
appliance's configuration can alter it.

The full path has since been exercised against real hardware from a foreign
Linux: a Z83F running Ubuntu 24.04 kexec'd into the image and served its web
interface from RAM while its eMMC stayed unmounted. Two things that only show
up outside a VM came out of that run. The RAM system sends `laserbridge` as
its DHCP hostname and therefore usually receives a different lease than the
installed system, so `deploy.sh` also looks for it over mDNS. And completing
the first-boot wizard inside a RAM session used to replace `authorized_keys`
outright, discarding the deployment key that started the session; the wizard
now keeps it.

The RAM boot itself was verified by booting the combined initramfs directly:
the overlay mounts, `/data` is a tmpfs, no partition of the system disk is
mounted, the disk is byte-identical afterwards, and the recovery mode drops
the appliance services. Only the kexec transition is blocked, and only when
the launch pad is LaserBridgeOS itself - a foreign Linux that permits
`kexec_load` can drive the same workflow today.

Resolving this needs a kernel with kexec enabled, or a GRUB one-shot RAM boot
in place of kexec; `docs/deploy.md` weighs both. The decision is deferred
rather than taken here, because it trades a maintained kernel against a disk
write during `--test`.
