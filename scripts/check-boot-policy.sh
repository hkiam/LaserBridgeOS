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

echo "Boot policy: direct, headless, parallel, recoverable"
