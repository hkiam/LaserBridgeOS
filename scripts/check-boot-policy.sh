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

echo "Boot policy: direct, headless, parallel"
