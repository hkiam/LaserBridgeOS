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
# An account prefixed with ! is rejected by OpenSSH before public-key auth.
# Use an invalid (but administratively unlocked) password field instead.
sed -i 's/^laserbridge:[^:]*:/laserbridge:x:/' "$ROOT/etc/shadow"
chroot "$ROOT" /usr/bin/passwd -l root >/dev/null 2>&1 || true

add_service() {
	runlevel=$1
	service=$2
	if [ -e "$ROOT/etc/init.d/$service" ]; then
		ln -snf "/etc/init.d/$service" "$ROOT/etc/runlevels/$runlevel/$service"
	fi
}

for service in devfs dmesg mdev hwdrivers; do add_service sysinit "$service"; done
for service in modules sysctl hostname bootmisc syslog localmount laserbridge-init; do add_service boot "$service"; done
for service in networking laserbridge-network avahi-daemon sshd ser2net ustreamer laserbridge-web laserbridge-boot-confirm; do add_service default "$service"; done
for service in mount-ro killprocs savecache; do add_service shutdown "$service"; done

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
