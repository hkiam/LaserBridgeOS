#!/bin/sh
# shellcheck disable=SC1091
set -eu

. /workspace/build/versions.env

ROOT=/image-rootfs
REPOSITORIES=/tmp/laserbridge-repositories

mkdir -p "$ROOT/etc/apk"
printf '%s\n%s\n' \
	"https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION}/main" \
	"https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION}/community" > "$REPOSITORIES"

apk --root "$ROOT" --arch x86_64 --initdb --no-cache \
	--keys-dir /etc/apk/keys --repositories-file "$REPOSITORIES" add \
	alpine-base avahi ca-certificates doas e2fsprogs ifupdown-ng intel-ucode \
	dnsmasq hostapd iw kexec-tools libbsd libevent libjpeg-turbo \
	linux-firmware-brcm linux-firmware-intel linux-firmware-rtlwifi \
	linux-firmware-rtw88 linux-firmware-rtw89 linux-lts lsblk mdev-conf \
	mkinitfs openssh ser2net v4l-utils wireless-regdb wpa_supplicant zstd

rsync -a /workspace/rootfs/ "$ROOT/"
install -D -m 0755 /workspace/laserbridge "$ROOT/usr/sbin/laserbridge"
install -D -m 0755 /workspace/laserbridged "$ROOT/usr/sbin/laserbridged"
install -D -m 0755 /workspace/ustreamer "$ROOT/usr/bin/ustreamer"
mkdir -p "$ROOT/usr/share/laserbridge/web" "$ROOT/data" "$ROOT/run/laserbridge" \
	"$ROOT/var/log" "$ROOT/var/tmp" "$ROOT/etc/runlevels/sysinit" \
	"$ROOT/etc/runlevels/boot" "$ROOT/etc/runlevels/default" "$ROOT/etc/runlevels/shutdown"
rsync -a /workspace/web/ "$ROOT/usr/share/laserbridge/web/"
printf '%s\n' "${VERSION:-0.1.0}" > "$ROOT/etc/laserbridge/version"

ln -snf /data/config.yaml "$ROOT/etc/laserbridge/config.yaml"
ln -snf /proc/mounts "$ROOT/etc/mtab"
rm -f /image-rootfs/etc/resolv.conf
ln -snf /run/resolv.conf "$ROOT/etc/resolv.conf"
rm -rf /image-rootfs/var/run
ln -snf /run "$ROOT/var/run"

# doas refuses to run if its configuration is group- or world-writable.
install -m 0755 /workspace/rootfs/usr/sbin/laserbridge-deploy "$ROOT/usr/sbin/laserbridge-deploy"
chmod 0600 "$ROOT/etc/doas.conf"

chroot "$ROOT" /usr/sbin/addgroup -S laserbridge
chroot "$ROOT" /usr/sbin/adduser -S -D -H -h /data/home/laserbridge -s /bin/ash -G laserbridge laserbridge
# busybox syslogd creates /data/log/messages as root:wheel with mode 0640, and
# recreates it that way after every rotation, so a one-off chmod would not last.
# The operator has to be able to read the log of the appliance they are the only
# account on - the same reasoning that leaves dmesg unrestricted (ADR 0008).
# Membership grants nothing by itself here: doas permits this account by name,
# and su is irrelevant with root locked.
chroot "$ROOT" /usr/sbin/addgroup laserbridge wheel
# Ship a known default password so the appliance is usable over SSH straight
# away, without a key file. It is the same on every image and therefore only
# a convenience, never a secret: the setup wizard asks for a new one, and the
# device is meant for an isolated workshop network. See ADR 0006.
#
# The hash is precomputed with a fixed salt rather than generated here,
# because a random salt would make the image non-reproducible. The salt adds
# nothing anyway for a password that is printed in the README.
#   openssl passwd -6 -salt LaserBridgeOS laserbridge
# shellcheck disable=SC2016 # the $6$ prefix is crypt syntax, not a variable
DEFAULT_PASSWORD_HASH='$6$LaserBridgeOS$5w7ZyIhzoG.NAVmvorgxpRKC.XGg6qvMpguJYnLjUmnnR6l/4K22zqUDEJzxDLz70DhXx89G6igxNYJLXeczQ0'
# A | delimiter, because the hash contains slashes. sed -i keeps the file's
# ownership and mode, which matters for /etc/shadow.
sed -i "s|^laserbridge:[^:]*:|laserbridge:${DEFAULT_PASSWORD_HASH}:|" "$ROOT/etc/shadow"
grep -q "^laserbridge:\\\$6\\\$" "$ROOT/etc/shadow" ||
	{ echo "failed to set the default password" >&2; exit 1; }
chroot "$ROOT" /usr/bin/passwd -l root >/dev/null 2>&1 || true

# The root filesystem is an immutable SquashFS, so a password could never be
# changed while the appliance runs. Keep the account database where the rest
# of the persistent state lives and expose it at the conventional path, the
# same trick already used for config.yaml. The image ships the initial file
# as a template; the first boot copies it into /data.
install -D -m 0640 "$ROOT/etc/shadow" "$ROOT/etc/laserbridge/shadow.default"
ln -snf /data/shadow "$ROOT/etc/shadow"

add_service() {
	runlevel=$1
	service=$2
	if [ -e "$ROOT/etc/init.d/$service" ]; then
		ln -snf "/etc/init.d/$service" "$ROOT/etc/runlevels/$runlevel/$service"
	fi
}

# The services whose absence nobody would notice in time.
#
# add_service skips whatever it cannot find, which is right for the ones that
# only make the appliance nicer - no ntpd means a wrong clock, and the journal
# already carries uptimes for that reason. It is wrong for these. An Alpine
# release that renames or drops klogd would build a perfectly good image with no
# kernel messages in the log; one that drops the sysctl service would build one
# that does not reboot after an oops; a renamed file of our own would silently
# take the GRBL bridge, the watchdog or the web interface out of the image. All
# of those are found on the appliance, weeks later, by someone looking for
# something else.
require_service() {
	runlevel=$1
	service=$2
	if [ ! -e "$ROOT/etc/init.d/$service" ]; then
		echo "required service $service has no init script; refusing to build an image without it" >&2
		exit 1
	fi
	add_service "$runlevel" "$service"
}

for service in devfs dmesg mdev hwdrivers; do add_service sysinit "$service"; done
for service in modules hostname bootmisc localmount hwclock; do add_service boot "$service"; done
# sysctl carries the panic settings, so an appliance without it does not reboot
# after an oops. klogd puts the kernel's own messages into the same log as
# everything else; without it an oops, an out-of-memory kill or a USB reset
# lives only in a ring buffer that the reboot empties, and those are exactly
# the failures on a headless appliance that can only be read afterwards. The
# watchdog is armed here, in the boot runlevel, before the services it outlives
# and sharing no dependency with any of them.
for service in sysctl syslog klogd laserbridge-init laserbridge-watchdog; do require_service boot "$service"; done
for service in networking avahi-daemon ntpd; do add_service default "$service"; done
# Both GRBL backends are enabled; each refuses to start unless the
# configuration names it, so exactly one ends up owning the serial port.
for service in laserbridge-network sshd ser2net laserbridged ustreamer laserbridge-web laserbridge-boot-confirm; do require_service default "$service"; done
for service in mount-ro killprocs savecache; do add_service shutdown "$service"; done

# Alpine ships kernel modules individually gzipped. Inside an xz SquashFS that
# is the worse of two compressions applied to the same bytes: unpacking them
# first and letting mksquashfs compress the lot roughly halves what they cost.
# depmod runs afterwards so modules.dep names the files that now exist.
find "$ROOT/lib/modules" -name '*.ko.gz' -exec gunzip -f {} +

kernel_version=$(basename "$(find "$ROOT/lib/modules" -mindepth 1 -maxdepth 1 -type d | sort | tail -n 1)")
if [ -z "$kernel_version" ]; then
	echo "No installed kernel modules found" >&2
	exit 1
fi
chroot "$ROOT" /sbin/depmod -a "$kernel_version"
sed -f /workspace/build/initramfs-root.sed \
	"$ROOT/usr/share/mkinitfs/initramfs-init" > "$ROOT/etc/laserbridge/initramfs-init"
chmod 0755 "$ROOT/etc/laserbridge/initramfs-init"
chroot "$ROOT" /sbin/mkinitfs -C zstd -c /etc/mkinitfs/mkinitfs.conf \
	-i /etc/laserbridge/initramfs-init "$kernel_version"
# zstd stays installed: the RAM recovery system needs it to unpack images
# streamed from the operator's machine (ADR 0005).

find "$ROOT" -exec touch -h -d "@${SOURCE_DATE_EPOCH:-1786579200}" {} +
