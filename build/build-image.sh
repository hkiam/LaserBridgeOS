#!/bin/sh
set -eu

VERSION=${VERSION:-0.1.0}
BUILD_EPOCH=${SOURCE_DATE_EPOCH:-1786579200}
unset SOURCE_DATE_EPOCH
OUTPUT=/out
IMAGE_NAME="LaserBridgeOS-x86_64.img"
IMAGE="$OUTPUT/$IMAGE_NAME"
UPDATE_NAME="LaserBridgeOS-x86_64-${VERSION}.lbu"
UPDATE="$OUTPUT/$UPDATE_NAME"

ESP_START=2048
ESP_SECTORS=262144
ROOT_A_START=264192
ROOT_SECTORS=524288
ROOT_B_START=788480
DATA_START=1312768
DATA_SECTORS=655360
DISK_SECTORS=1970176

case "$VERSION" in
	''|*[!A-Za-z0-9._-]*) echo "Invalid VERSION for update manifest: $VERSION" >&2; exit 1 ;;
esac

mkdir -p /work
rm -rf /work/rootfs /work/esp /work/update
rm -f /work/root.squashfs /work/esp.fat /work/data.ext4 /work/sort.txt "$UPDATE"
cp -a /image-rootfs /work/rootfs
mkdir -p /work/esp/EFI/BOOT /work/esp/boot/slot-a /work/esp/boot/slot-b /work/update "$OUTPUT"

for slot in slot-a slot-b; do
	install -m 0644 /work/rootfs/boot/vmlinuz-lts "/work/esp/boot/$slot/vmlinuz-lts"
	install -m 0644 /work/rootfs/boot/initramfs-lts "/work/esp/boot/$slot/initramfs-lts"
done
printf '%s\n' 'set laserbridge_slot=a' > /work/esp/boot/active-slot.cfg
rm -rf /work/rootfs/boot/*
find /work/rootfs -exec touch -h -d "@$BUILD_EPOCH" {} +

SOURCE_DATE_EPOCH="$BUILD_EPOCH" grub-mkstandalone -O x86_64-efi -o /work/esp/EFI/BOOT/BOOTX64.EFI \
	"boot/grub/grub.cfg=/workspace/build/grub.cfg"
find /work/esp -exec touch -h -d "@$BUILD_EPOCH" {} +
# mksquashfs's sort-file parser cannot represent whitespace in a path. Such
# firmware board files keep the default priority; all other entries are listed
# in bytewise order for reproducible inode assignment.
find /work/rootfs -mindepth 1 -printf '%P\n' | LC_ALL=C sort | \
	awk 'index($0, " ") == 0 && index($0, "\t") == 0 { print $0 " 0" }' > /work/sort.txt
mksquashfs /work/rootfs /work/root.squashfs -noappend -all-root -no-xattrs \
	-comp xz -b 1M -no-progress -mkfs-time "$BUILD_EPOCH" -sort /work/sort.txt

root_bytes=$(stat -c %s /work/root.squashfs)
root_capacity=$((ROOT_SECTORS * 512))
if [ "$root_bytes" -gt "$root_capacity" ]; then
	echo "SquashFS is too large for root partition ($root_bytes > $root_capacity)" >&2
	exit 1
fi

root_sha=$(sha256sum /work/root.squashfs | cut -d ' ' -f 1)
kernel_sha=$(sha256sum /image-rootfs/boot/vmlinuz-lts | cut -d ' ' -f 1)
initramfs_sha=$(sha256sum /image-rootfs/boot/initramfs-lts | cut -d ' ' -f 1)
root_size=$(stat -c %s /work/root.squashfs)
kernel_size=$(stat -c %s /image-rootfs/boot/vmlinuz-lts)
initramfs_size=$(stat -c %s /image-rootfs/boot/initramfs-lts)
install -m 0644 /work/root.squashfs /work/update/root.squashfs
install -m 0644 /image-rootfs/boot/vmlinuz-lts /work/update/vmlinuz-lts
install -m 0644 /image-rootfs/boot/initramfs-lts /work/update/initramfs-lts
printf '{"format_version":1,"architecture":"x86_64","version":"%s","files":{"root.squashfs":{"sha256":"%s","size":%s},"vmlinuz-lts":{"sha256":"%s","size":%s},"initramfs-lts":{"sha256":"%s","size":%s}}}\n' \
	"$VERSION" "$root_sha" "$root_size" "$kernel_sha" "$kernel_size" "$initramfs_sha" "$initramfs_size" \
	> /work/update/manifest.json
find /work/update -exec touch -h -d "@$BUILD_EPOCH" {} +
(cd /work/update && zip -X -0 -q "$UPDATE" manifest.json root.squashfs vmlinuz-lts initramfs-lts)

truncate -s $((ESP_SECTORS * 512)) /work/esp.fat
mkfs.vfat --invariant -F 32 -n LBBOOT -i 4c424f54 /work/esp.fat
mcopy -spm -i /work/esp.fat /work/esp/EFI ::/
mcopy -spm -i /work/esp.fat /work/esp/boot ::/

truncate -s $((DATA_SECTORS * 512)) /work/data.ext4
E2FSPROGS_FAKE_TIME="$BUILD_EPOCH" mkfs.ext4 -q -F -L LBDATA \
	-U 4c42524f-4f54-4000-8000-000000000003 \
	-E hash_seed=4c42524f-4f54-4000-8000-000000000003,lazy_itable_init=0,lazy_journal_init=0 \
	/work/data.ext4
sha256sum /work/root.squashfs /work/esp.fat /work/data.ext4

rm -f "$IMAGE"
truncate -s $((DISK_SECTORS * 512)) "$IMAGE"
sgdisk --clear \
	--disk-guid=4c42524f-4f54-4000-8000-000000000000 \
	--new=1:${ESP_START}:$((ESP_START + ESP_SECTORS - 1)) --typecode=1:ef00 --change-name=1:LBBOOT --partition-guid=1:4c42524f-4f54-4000-8000-000000000001 \
	--new=2:${ROOT_A_START}:$((ROOT_A_START + ROOT_SECTORS - 1)) --typecode=2:8300 --change-name=2:LBROOTA --partition-guid=2:4c42524f-4f54-4000-8000-000000000002 \
	--new=3:${ROOT_B_START}:$((ROOT_B_START + ROOT_SECTORS - 1)) --typecode=3:8300 --change-name=3:LBROOTB --partition-guid=3:4c42524f-4f54-4000-8000-000000000003 \
	--new=4:${DATA_START}:$((DATA_START + DATA_SECTORS - 1)) --typecode=4:8300 --change-name=4:LBDATA --partition-guid=4:4c42524f-4f54-4000-8000-000000000004 \
	"$IMAGE"

dd if=/work/esp.fat of="$IMAGE" bs=512 seek="$ESP_START" conv=notrunc status=none
dd if=/work/root.squashfs of="$IMAGE" bs=512 seek="$ROOT_A_START" conv=notrunc status=none
dd if=/work/root.squashfs of="$IMAGE" bs=512 seek="$ROOT_B_START" conv=notrunc status=none
dd if=/work/data.ext4 of="$IMAGE" bs=512 seek="$DATA_START" conv=notrunc status=none
sgdisk --verify "$IMAGE"

gzip -n -9 -c "$IMAGE" > "$IMAGE.gz"
cd "$OUTPUT"
sha256sum "$IMAGE_NAME" "$IMAGE_NAME.gz" "$UPDATE_NAME" > SHA256SUMS
if [ -n "${OUTPUT_UID:-}" ] && [ -n "${OUTPUT_GID:-}" ]; then
	chown "$OUTPUT_UID:$OUTPUT_GID" "$IMAGE_NAME" "$IMAGE_NAME.gz" "$UPDATE_NAME" SHA256SUMS
fi

echo "Built $IMAGE ($VERSION)"
