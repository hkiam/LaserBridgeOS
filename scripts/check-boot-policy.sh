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
grep -qx 'rc_parallel="YES"' rootfs/etc/rc.conf || fail "OpenRC parallel startup is disabled"
grep -q -- '-C zstd' build/prepare-rootfs.sh || fail "initramfs does not use zstd"
if grep -Eq '^[^#].*getty' rootfs/etc/inittab; then
	fail "headless image starts a virtual-console getty"
fi
if grep -Eq ':/sbin/openrc ([^-]|$)' rootfs/etc/inittab; then
	fail "OpenRC is not started in quiet mode"
fi

# A headless appliance has no console to recover from a slot that never boots
# or from Wi-Fi credentials that never associate. Both safety nets below are
# the only way back in, so guard them against a silent removal.
grep -q 'save_env' build/grub.cfg || fail "GRUB does not persist a boot-attempt counter"
grep -q 'load_env' build/grub.cfg || fail "GRUB does not read the boot-attempt counter"
grep -q 'laserbridge-boot-confirm' build/prepare-rootfs.sh ||
	fail "the boot confirmation service is not enabled in any runlevel"
grep -q 'grub-editenv' build/build-image.sh || fail "the image ships no GRUB environment block"
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
