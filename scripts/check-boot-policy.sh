#!/bin/sh
set -eu

fail() {
	echo "boot policy check failed: $1" >&2
	exit 1
}

if grep -Eq '(^|[[:space:]])menuentry([[:space:]]|$)|set[[:space:]]+timeout' build/grub.cfg; then
	fail "GRUB must boot directly without a menu or timeout"
fi
grep -qx 'boot' build/grub.cfg || fail "GRUB configuration has no direct boot command"
if grep -q 'console=tty' build/grub.cfg; then
	fail "production kernel command line must not enable a slow serial/virtual console"
fi

# The GRBL controller hangs off a USB serial adapter, and an adapter the kernel
# suspends mid-job can swallow the start of the next line or come back as a
# different device node. Userspace applies the configured policy later, but only
# once /data is readable; both slots have to boot without it in the meantime.
[ "$(grep -c 'linux .*usbcore\.autosuspend=-1' build/grub.cfg)" = "2" ] ||
	fail "both A/B kernel command lines must switch USB autosuspend off"
# And again where an update can reach it: the command line above is written to
# the ESP at install time and never replaced, so an appliance updated in place
# would otherwise keep the policy it was installed with forever.
grep -qx 'options usbcore autosuspend=-1' rootfs/etc/modprobe.d/laserbridge-usb.conf ||
	fail "USB autosuspend is not switched off anywhere an update can change it"
grep -qx '/etc/modprobe.d/laserbridge-usb.conf' rootfs/etc/mkinitfs/features.d/laserbridge.files ||
	fail "the USB module options are not carried into the initramfs"

grep -qx 'rc_parallel="YES"' rootfs/etc/rc.conf || fail "OpenRC parallel startup is disabled"
grep -q -- '-C zstd' build/prepare-rootfs.sh || fail "initramfs does not use zstd"
if grep -Eq '^[^#].*getty' rootfs/etc/inittab; then
	fail "headless image starts a virtual-console getty"
fi
if grep -Eq ':/sbin/openrc ([^-]|$)' rootfs/etc/inittab; then
	fail "OpenRC is not started in quiet mode"
fi

# The watchdog is the only thing that acts when the kernel stops scheduling
# userspace at all, and the panic settings are what get most lockups as far as
# a reset. Nobody notices any of them going missing until the day it matters,
# and by then a laser has been on for the duration (ADR 0017).
[ -f rootfs/etc/init.d/laserbridge-watchdog ] ||
	fail "rootfs/etc/init.d/laserbridge-watchdog is missing"
grep -qE 'require_service .*laserbridge-watchdog|for service in .*laserbridge-watchdog' build/prepare-rootfs.sh ||
	fail "the hardware watchdog is not started in any runlevel"
grep -qE 'for service in .*sysctl.*require_service' build/prepare-rootfs.sh ||
	fail "the sysctl service is optional; the panic settings would silently not apply"
for setting in 'kernel.panic = 10' 'kernel.panic_on_oops = 1'; do
	grep -qx "$setting" rootfs/etc/sysctl.d/90-laserbridge.conf ||
		fail "sysctl '$setting' is not set; a panic would leave the appliance stopped"
done

# A headless appliance has no console to recover from a slot that never boots
# or from Wi-Fi credentials that never associate. Both safety nets below are
# the only way back in, so guard them against a silent removal.
grep -q 'save_env' build/grub.cfg || fail "GRUB does not persist a boot-attempt counter"
grep -q 'load_env' build/grub.cfg || fail "GRUB does not read the boot-attempt counter"
grep -q 'laserbridge-boot-confirm' build/prepare-rootfs.sh ||
	fail "the boot confirmation service is not enabled in any runlevel"
grep -q 'grub-editenv' build/build-image.sh || fail "the image ships no GRUB environment block"
# The failures worth diagnosing on a headless appliance - an oops, an OOM kill,
# a USB reset - are readable only after the reboot that follows them, and a log
# in a tmpfs ring does not get there.
grep -q -- '-O /data/log/' rootfs/etc/conf.d/syslog ||
	fail "system logs are not written anywhere that survives a reboot"
grep -qE 'for service in .*klogd.*require_service' build/prepare-rootfs.sh ||
	fail "kernel messages are not recorded, or klogd is optional enough to vanish quietly"

grep -q 'wait_for_association' rootfs/etc/init.d/laserbridge-network ||
	fail "Wi-Fi client mode does not wait for association before DHCP"
grep -q 'start_access_point' rootfs/etc/init.d/laserbridge-network ||
	fail "Wi-Fi client mode has no setup-AP recovery path"

# ser2net is the permanent fallback for the day laserbridged does something
# unexpected mid-job (ADR 0011). "We keep the fallback" does not survive a year
# of refactoring unless something fails when it stops being true.
grep -q 'ser2net' build/prepare-rootfs.sh ||
	fail "ser2net is no longer installed; it is the permanent GRBL fallback (ADR 0011)"
for backend in ser2net laserbridged; do
	grep -q "add_service default.*$backend\|for service in .*$backend" build/prepare-rootfs.sh ||
		fail "$backend is not in the default runlevel; both GRBL backends must be present"
	[ -f "rootfs/etc/init.d/$backend" ] || fail "rootfs/etc/init.d/$backend is missing"
	grep -q "grbl-backend $backend" "rootfs/etc/init.d/$backend" ||
		fail "$backend does not check whether the configuration selects it"
done

# The size of a root slot is written down twice, in two languages: the
# partition table in the build script, and the limit the updater enforces
# before it writes a slot. If they drift apart an update either overflows its
# partition or is rejected for no reason.
slot_sectors=$(sed -n 's/^ROOT_SECTORS=\([0-9]*\)$/\1/p' build/build-image.sh)
slot_mib=$((slot_sectors / 2048))
go_mib=$(sed -n 's/^[[:space:]]*rootSlotSize[[:space:]]*=[[:space:]]*\([0-9]*\) << 20.*/\1/p' \
	backend/internal/update/update.go)
[ -n "$slot_sectors" ] && [ -n "$go_mib" ] ||
	fail "could not read the root slot size from both places"
[ "$slot_mib" = "$go_mib" ] ||
	fail "root slot is ${slot_mib} MiB in build-image.sh but ${go_mib} MiB in update.go"

echo "Boot policy: direct, headless, parallel, recoverable"
echo "Root slot: ${slot_mib} MiB, consistent between build and updater"
