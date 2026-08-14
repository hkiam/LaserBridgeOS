#!/bin/sh
# Remote kexec deployment for LaserBridgeOS.
#
#   ./deploy.sh --test     <image-dir>   boot the image from RAM, disk untouched
#   ./deploy.sh --install  <image-dir>   write the image to the system disk
#   ./deploy.sh --recovery [image-dir]   boot the RAM recovery system only
#   ./deploy.sh --status                 report what the target is running
#   ./deploy.sh --check    <image-dir>   verify prerequisites, change nothing
#
#   --resume  write to an appliance that is already running from RAM, instead
#             of kexecing into it first. Needs LASERBRIDGE_RECOVERY_SSH.
#
# See docs/deploy.md and ADR 0005.
set -eu

SELF_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
HELPER_SOURCE="$SELF_DIR/rootfs/usr/sbin/laserbridge-deploy"
REMOTE_HELPER=/usr/sbin/laserbridge-deploy
WORK=""

MODE=""
IMAGE_DIR=""
ASSUME_YES=no
DRY_RUN=no
VERIFY=yes
RESUME=no

die() { echo "deploy: $*" >&2; exit 1; }
info() { echo "==> $*"; }
warn() { echo "    warning: $*" >&2; }

cleanup() { [ -n "$WORK" ] && [ -d "$WORK" ] && rm -rf "$WORK"; }
trap cleanup EXIT INT TERM

usage() {
	sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'
	exit 2
}

# ---------------------------------------------------------------- configuration

load_env() {
	# A variable exported for this run wins over .env. Overriding one setting
	# once should not mean editing the file and remembering to change it back.
	exported_ssh=${LASERBRIDGE_SSH:-}
	exported_key=${LASERBRIDGE_SSH_KEY:-}
	exported_port=${LASERBRIDGE_SSH_PORT:-}
	exported_password=${LASERBRIDGE_PASSWORD:-}
	exported_recovery=${LASERBRIDGE_RECOVERY_SSH:-}
	exported_disk=${LASERBRIDGE_DISK:-}
	exported_pubkey=${LASERBRIDGE_PUBKEY:-}
	if [ -f "$SELF_DIR/.env" ]; then
		# shellcheck disable=SC1091
		. "$SELF_DIR/.env"
	fi
	[ -n "$exported_ssh" ] && LASERBRIDGE_SSH=$exported_ssh
	[ -n "$exported_key" ] && LASERBRIDGE_SSH_KEY=$exported_key
	[ -n "$exported_port" ] && LASERBRIDGE_SSH_PORT=$exported_port
	[ -n "$exported_password" ] && LASERBRIDGE_PASSWORD=$exported_password
	[ -n "$exported_recovery" ] && LASERBRIDGE_RECOVERY_SSH=$exported_recovery
	[ -n "$exported_disk" ] && LASERBRIDGE_DISK=$exported_disk
	[ -n "$exported_pubkey" ] && LASERBRIDGE_PUBKEY=$exported_pubkey
	# The LIGHTBURN_* names come from the operator's existing setup and are
	# accepted as aliases so one .env can serve both.
	SSH_TARGET=${LASERBRIDGE_SSH:-${LIGHTBURN_SSH:-}}
	SSH_KEY=${LASERBRIDGE_SSH_KEY:-${LIGHTBURN_SSH_KEY:-}}
	SSH_PORT=${LASERBRIDGE_SSH_PORT:-${LIGHTBURN_SSH_PORT:-}}
	SSH_PASSWORD=${LASERBRIDGE_PASSWORD:-${LIGHTBURN_PASSWORD:-}}
	# After the kexec the target is LaserBridgeOS regardless of what ran
	# before, so the account is laserbridge - not whatever user the installed
	# system uses - and only the injected key can log in.
	RECOVERY_TARGET=${LASERBRIDGE_RECOVERY_SSH:-laserbridge@${SSH_TARGET#*@}}
	TARGET_DISK=${LASERBRIDGE_DISK:-${LIGHTBURN_DISK:-}}
	PUBLIC_KEY=${LASERBRIDGE_PUBKEY:-}
	[ -n "$SSH_TARGET" ] ||
		die "no target configured; set LASERBRIDGE_SSH in .env (see .env.example)"
}

SSH_CMD="ssh"
SSH_KEEPALIVE="-o ServerAliveInterval=5 -o ServerAliveCountMax=3"

set_ssh_options() {
	# A RAM boot generates a fresh host key every time, so recovery sessions
	# would otherwise trip the known-hosts check on every single deployment.
	SSH_OPTS="-o ConnectTimeout=10 -o StrictHostKeyChecking=no"
	SSH_OPTS="$SSH_OPTS -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"
	# kexec tears the connection down without closing it, so the client would
	# otherwise sit on a dead socket until the kernel's TCP timeout - tens of
	# minutes. Keepalives make it notice in seconds. ConnectTimeout does not
	# help here: it only covers establishing the connection.
	SSH_OPTS="$SSH_OPTS $SSH_KEEPALIVE"
	[ -n "$SSH_KEY" ] && SSH_OPTS="$SSH_OPTS -i $SSH_KEY"
	[ -n "$SSH_PORT" ] && SSH_OPTS="$SSH_OPTS -p $SSH_PORT"

	# A foreign Linux may only offer password authentication. The password
	# goes through the environment rather than the command line so it does not
	# show up in the process list.
	if [ -n "$SSH_PASSWORD" ] && ! key_auth_works; then
		command -v sshpass >/dev/null ||
			die "$SSH_TARGET wants a password but sshpass is not installed (brew install sshpass)"
		SSHPASS=$SSH_PASSWORD
		export SSHPASS
		SSH_CMD="sshpass -e ssh"
		SSH_OPTS="$SSH_OPTS -o PreferredAuthentications=password -o PubkeyAuthentication=no"
		info "Using password authentication for $SSH_TARGET"
	fi
	return 0
}

key_auth_works() {
	# shellcheck disable=SC2086
	ssh -n -o BatchMode=yes $SSH_OPTS "$SSH_TARGET" true 2>/dev/null
}

CURRENT_TARGET=""

# target_ssh runs a command and keeps its own stdin closed, so a call made
# inside a command substitution cannot swallow the operator's terminal input.
# target_ssh_stdin is the variant that deliberately forwards stdin, used to
# stream artefacts onto the target.
target_ssh() {
	# shellcheck disable=SC2086 # SSH_CMD and SSH_OPTS are deliberate word lists
	$SSH_CMD -n $SSH_OPTS "$CURRENT_TARGET" "$@"
}

target_ssh_stdin() {
	# shellcheck disable=SC2086,SC2029 # deliberate word lists; remote expansion intended
	$SSH_CMD $SSH_OPTS "$CURRENT_TARGET" "$@"
}

# ---------------------------------------------------------------- remote helper

PRIVILEGE=""
HELPER=""

# probe_privilege works out how to reach root on whatever is currently running
# on the target. LaserBridgeOS ships the helper and a doas rule for exactly
# this script; a foreign Linux gets the same helper uploaded so that both
# cases share one implementation of the privileged operations.
probe_privilege() {
	remote_id=$(target_ssh 'id -u' 2>/dev/null) ||
		die "cannot reach $CURRENT_TARGET over SSH"

	if target_ssh "test -x $REMOTE_HELPER" 2>/dev/null; then
		HELPER=$REMOTE_HELPER
	else
		info "target has no deployment helper; uploading it"
		[ -f "$HELPER_SOURCE" ] || die "helper not found at $HELPER_SOURCE"
		HELPER=/tmp/laserbridge-deploy
		target_ssh_stdin "cat > $HELPER && chmod 0755 $HELPER" < "$HELPER_SOURCE" ||
			die "could not upload the deployment helper"
	fi

	if [ "$remote_id" = "0" ]; then
		PRIVILEGE=""
	elif target_ssh "command -v doas >/dev/null && doas -n $HELPER detect >/dev/null 2>&1"; then
		PRIVILEGE="doas"
	elif target_ssh 'command -v sudo >/dev/null && sudo -n true 2>/dev/null'; then
		PRIVILEGE="sudo -n"
	elif [ -n "$SSH_PASSWORD" ] && sudo_password_works; then
		PRIVILEGE="sudo-password"
	else
		die "cannot become root on $CURRENT_TARGET (no doas rule, no passwordless sudo, no LASERBRIDGE_PASSWORD)"
	fi
}

# sudo_password_works checks that the account can reach root with the
# configured password. sudo's timestamp is not reused afterwards: it does not
# survive between SSH sessions, so every privileged call carries the password
# itself.
sudo_password_works() {
	printf '%s\n' "$SSH_PASSWORD" |
		target_ssh_stdin 'command -v sudo >/dev/null && sudo -S -p "" true' >/dev/null 2>&1
}

# remote and remote_stdin run one privileged helper command. In password mode
# the password is fed to sudo -S as the first line of stdin; sudo consumes
# exactly that line and the command it starts sees the rest, which is what
# lets a payload be streamed through the same pipe. Nothing is written to the
# target's disk and nothing appears in its process list - unlike an askpass
# file, which would survive on a system that a kexec is about to replace.
# shellcheck disable=SC2029 # the helper path and arguments expand there on purpose
remote() {
	if [ "$PRIVILEGE" = "sudo-password" ]; then
		printf '%s\n' "$SSH_PASSWORD" |
			target_ssh_stdin "sudo -S -p '' $HELPER $*"
	else
		target_ssh "$PRIVILEGE $HELPER $*"
	fi
}

# shellcheck disable=SC2029 # as above; this variant streams a payload
remote_stdin() {
	if [ "$PRIVILEGE" = "sudo-password" ]; then
		{ printf '%s\n' "$SSH_PASSWORD"; cat; } |
			target_ssh_stdin "sudo -S -p '' $HELPER $*"
	else
		target_ssh_stdin "$PRIVILEGE $HELPER $*"
	fi
}

# ---------------------------------------------------------------- local artifacts

first_match() {
	for candidate in $1; do
		if [ -f "$candidate" ]; then
			printf '%s' "$candidate"
			return 0
		fi
	done
	return 1
}

find_artifacts() {
	[ -d "$IMAGE_DIR" ] || die "image directory not found: $IMAGE_DIR"
	BUNDLE=$(first_match "$IMAGE_DIR/*.lbu") ||
		die "no .lbu bundle in $IMAGE_DIR; run 'make image' first"
	DISK_IMAGE=$(first_match "$IMAGE_DIR/*.img.gz") || DISK_IMAGE=""
	SUMS="$IMAGE_DIR/SHA256SUMS"
}

verify_checksums() {
	[ -f "$SUMS" ] || { warn "no SHA256SUMS in $IMAGE_DIR; skipping checksum verification"; return 0; }
	info "Verifying artifact checksums"
	( cd "$IMAGE_DIR" && shasum -a 256 -c SHA256SUMS 2>/dev/null ) >/dev/null ||
		die "checksum verification failed in $IMAGE_DIR"
}

extract_bundle() {
	WORK=$(mktemp -d "${TMPDIR:-/tmp}/laserbridge-deploy.XXXXXX")
	info "Unpacking $(basename "$BUNDLE")"
	unzip -q -o "$BUNDLE" -d "$WORK" ||
		die "could not unpack $BUNDLE"
	for name in manifest.json root.squashfs vmlinuz-lts initramfs-lts; do
		[ -f "$WORK/$name" ] || die "bundle is missing $name"
	done

	# The manifest is the authority on what this image is; check the payload
	# against it rather than trusting the file names.
	MANIFEST_VERSION=$(manifest_field version)
	MANIFEST_ARCH=$(manifest_field architecture)
	for name in root.squashfs vmlinuz-lts initramfs-lts; do
		expected=$(manifest_digest "$name")
		actual=$(shasum -a 256 "$WORK/$name" | cut -d' ' -f1)
		[ "$expected" = "$actual" ] ||
			die "$name does not match the manifest digest"
	done
	info "Image $MANIFEST_VERSION ($MANIFEST_ARCH) verified against its manifest"
}

manifest_field() {
	sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p" "$WORK/manifest.json"
}

manifest_digest() {
	sed -n "s/.*\"$1\":{\"sha256\":\"\([0-9a-f]*\)\".*/\1/p" "$WORK/manifest.json"
}

operator_public_key() {
	if [ -n "$PUBLIC_KEY" ] && [ -f "$PUBLIC_KEY" ]; then
		cat "$PUBLIC_KEY"
		return
	fi
	if [ -n "$SSH_KEY" ] && [ -f "$SSH_KEY.pub" ]; then
		cat "$SSH_KEY.pub"
		return
	fi
	for candidate in "$HOME/.ssh/id_ed25519.pub" "$HOME/.ssh/id_rsa.pub"; do
		[ -f "$candidate" ] && { cat "$candidate"; return; }
	done
	die "no public key found; set LASERBRIDGE_PUBKEY so the RAM system can authorize you"
}

# build_kexec_initramfs prepends an uncompressed cpio archive carrying the
# root filesystem to the stock initramfs. The kernel concatenates initramfs
# segments, and an uncompressed archive first is the same arrangement the
# kernel already uses for early microcode, so it is the well-trodden path.
build_kexec_initramfs() {
	info "Building the RAM boot initramfs"
	mkdir -p "$WORK/extra"
	cp "$WORK/root.squashfs" "$WORK/extra/root.squashfs"
	operator_public_key > "$WORK/extra/laserbridge-authorized-keys"
	( cd "$WORK/extra" && printf '%s\n' root.squashfs laserbridge-authorized-keys |
		cpio -o -H newc --quiet ) > "$WORK/extra.cpio" 2>/dev/null ||
		( cd "$WORK/extra" && printf '%s\n' root.squashfs laserbridge-authorized-keys |
			cpio -o -H newc ) > "$WORK/extra.cpio" ||
		die "could not build the cpio segment"

	# Segments must start on a 4-byte boundary.
	size=$(wc -c < "$WORK/extra.cpio" | tr -d ' ')
	padding=$(( (4 - (size % 4)) % 4 ))
	[ "$padding" -gt 0 ] && dd if=/dev/zero bs=1 count="$padding" >> "$WORK/extra.cpio" 2>/dev/null
	cat "$WORK/extra.cpio" "$WORK/initramfs-lts" > "$WORK/kexec-initramfs"
	KEXEC_INITRAMFS_BYTES=$(wc -c < "$WORK/kexec-initramfs" | tr -d ' ')
	info "RAM boot initramfs: $(human "$KEXEC_INITRAMFS_BYTES")"
}

human() {
	awk -v b="$1" 'BEGIN {
		split("B KiB MiB GiB TiB", u, " "); i = 1
		while (b >= 1024 && i < 5) { b /= 1024; i++ }
		printf (i == 1 ? "%d %s" : "%.1f %s"), b, u[i]
	}'
}

# ---------------------------------------------------------------- target facts

probe_target() {
	info "Inspecting $CURRENT_TARGET"
	TARGET_FACTS=$(remote detect) || die "could not inspect the target"
	TARGET_ARCH=$(fact arch)
	TARGET_KERNEL=$(fact kernel)
	TARGET_MEM_KB=$(fact memtotal_kb)
	TARGET_MEM_FREE_KB=$(fact memavailable_kb)
	TARGET_KEXEC=$(fact kexec)
	TARGET_KEXEC_PERMITTED=$(fact kexec_permitted)
	TARGET_RAMBOOT=$(fact ramboot)
	TARGET_HOSTNAME=$(target_ssh 'hostname' 2>/dev/null || echo unknown)
}

fact() {
	printf '%s\n' "$TARGET_FACTS" | sed -n "s/^$1=//p" | head -1
}

target_disks() {
	printf '%s\n' "$TARGET_FACTS" | sed -n 's/^disk=//p'
}

choose_disk() {
	if [ -n "$TARGET_DISK" ]; then
		target_disks | grep -q "^$TARGET_DISK " ||
			die "configured disk $TARGET_DISK is not present on the target"
		CHOSEN_DISK=$TARGET_DISK
	else
		count=$(target_disks | wc -l | tr -d ' ')
		[ "$count" -ge 1 ] || die "no fixed disk found on the target"
		[ "$count" = "1" ] ||
			die "the target has $count fixed disks; set LASERBRIDGE_DISK to choose one:
$(target_disks | sed 's/^/      /')"
		CHOSEN_DISK=$(target_disks | head -1 | cut -d' ' -f1)
	fi
	CHOSEN_DISK_BYTES=$(target_disks | grep "^$CHOSEN_DISK " | sed -n 's/.*bytes=\([0-9]*\).*/\1/p')
}

run_checks() {
	failures=0
	check() {
		if [ "$1" = "ok" ]; then
			printf '    [ ok ] %s\n' "$2"
		else
			printf '    [FAIL] %s\n' "$2"
			failures=$((failures + 1))
		fi
	}
	echo "Preflight:"

	if [ -n "$IMAGE_DIR" ]; then
		if [ "$MANIFEST_ARCH" = "$TARGET_ARCH" ]; then
			check ok "architecture: image $MANIFEST_ARCH matches target $TARGET_ARCH"
		else
			check fail "architecture: image is $MANIFEST_ARCH, target is $TARGET_ARCH"
		fi
	fi

	if [ "$RESUME" = "yes" ]; then
		if [ "$TARGET_RAMBOOT" = "yes" ]; then
			check ok "the target is already running from RAM"
		else
			check fail "the target is not running from RAM; --resume has nothing to resume"
		fi
	elif [ "$TARGET_KEXEC" = "yes" ]; then
		check ok "kexec-tools is installed on the target"
	else
		check fail "kexec-tools is not installed on the target"
	fi

	if [ "$RESUME" = "yes" ]; then
		: # the kexec already happened; its prerequisites no longer matter
	elif [ "$TARGET_KEXEC_PERMITTED" = "yes" ]; then
		check ok "the kernel permits kexec_load"
	else
		check fail "the running kernel forbids kexec_load (kexec_load_disabled=1).
           This is one-way: it cannot be re-enabled at runtime. Alpine's stock
           linux-lts ships this way and has no kexec_file_load either, so the
           target needs a kernel built with kexec enabled. See docs/deploy.md."
	fi

	# The RAM system holds the initramfs while the kernel unpacks it, so the
	# image is briefly resident twice.
	if [ -n "$IMAGE_DIR" ] && [ "$RESUME" = "no" ]; then
		needed_kb=$(( (KEXEC_INITRAMFS_BYTES / 1024) * 2 + 262144 ))
		if [ "$TARGET_MEM_KB" -ge "$needed_kb" ]; then
			check ok "memory: $(human $((TARGET_MEM_KB * 1024))) total, needs about $(human $((needed_kb * 1024)))"
		else
			check fail "memory: $(human $((TARGET_MEM_KB * 1024))) total, needs about $(human $((needed_kb * 1024)))"
		fi
	fi

	if [ "$MODE" = "install" ]; then
		if [ -n "$DISK_IMAGE" ]; then
			check ok "disk image: $(basename "$DISK_IMAGE")"
		else
			check fail "no *.img.gz in $IMAGE_DIR; --install needs the full disk image"
		fi
		if [ -n "${CHOSEN_DISK:-}" ]; then
			check ok "target disk: $CHOSEN_DISK ($(human "$CHOSEN_DISK_BYTES"))"
		else
			check fail "no target disk selected"
		fi
	fi

	[ "$failures" = "0" ] || die "$failures preflight check(s) failed"
}

# ---------------------------------------------------------------- phases

kexec_into_ram() {
	extra_cmdline=$1
	stage_bytes=$(( KEXEC_INITRAMFS_BYTES + $(wc -c < "$WORK/vmlinuz-lts" | tr -d ' ') + 33554432 ))

	info "Staging the RAM image on the target ($(human "$stage_bytes") of tmpfs)"
	remote stage "$stage_bytes" >/dev/null || die "could not create the staging area"
	remote_stdin receive vmlinuz-lts < "$WORK/vmlinuz-lts" >/dev/null ||
		die "could not transfer the kernel"
	info "Transferring the RAM image ($(human "$KEXEC_INITRAMFS_BYTES"))"
	remote_stdin receive kexec-initramfs < "$WORK/kexec-initramfs" >/dev/null ||
		die "could not transfer the initramfs"

	cmdline="laserbridge.ramboot=1 $extra_cmdline quiet loglevel=3"
	info "Loading the kexec image"
	remote kexec-load vmlinuz-lts kexec-initramfs "'$cmdline'" ||
		die "kexec could not load the image"

	info "Rebooting into RAM via kexec (the SSH connection will drop)"
	# kexec bypasses firmware, so the connection dies rather than closing.
	remote kexec-exec >/dev/null 2>&1 || true
}

# operator_private_key names the counterpart of the public key injected into
# the RAM image, which is the only credential that system will accept.
operator_private_key() {
	if [ -n "$PUBLIC_KEY" ]; then
		printf '%s' "${PUBLIC_KEY%.pub}"
	elif [ -n "$SSH_KEY" ]; then
		printf '%s' "$SSH_KEY"
	else
		printf '%s' "$HOME/.ssh/id_ed25519"
	fi
}

# The RAM system has password authentication disabled and a freshly generated
# host key, so the session that follows a kexec needs different settings than
# the one that started it.
switch_to_recovery_ssh() {
	CURRENT_TARGET=$RECOVERY_TARGET
	SSH_CMD="ssh"
	unset SSHPASS
	SSH_OPTS="-o ConnectTimeout=10 -o StrictHostKeyChecking=no"
	SSH_OPTS="$SSH_OPTS -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"
	SSH_OPTS="$SSH_OPTS -o PreferredAuthentications=publickey -o BatchMode=yes"
	SSH_OPTS="$SSH_OPTS $SSH_KEEPALIVE -i $(operator_private_key)"
	[ -n "$SSH_PORT" ] && SSH_OPTS="$SSH_OPTS -p $SSH_PORT"
	return 0
}

# recovery_candidates lists where the RAM system may answer. It sends a
# different DHCP hostname than the installed system - always "laserbridge" -
# so the lease it gets is often a different address than the one configured
# here. It announces itself over mDNS, which is the reliable way to find it
# again.
recovery_candidates() {
	printf '%s\n' "$RECOVERY_TARGET"
	case "$RECOVERY_TARGET" in
		*@laserbridge.local) ;;
		*) printf 'laserbridge@laserbridge.local\n' ;;
	esac
}

wait_for_recovery() {
	switch_to_recovery_ssh
	info "Waiting for the RAM system ($(recovery_candidates | tr '\n' ' '))"
	waited=0
	while [ "$waited" -lt 240 ]; do
		sleep 5
		waited=$((waited + 5))
		for candidate in $(recovery_candidates); do
			CURRENT_TARGET=$candidate
			if target_ssh 'grep -q laserbridge.ramboot=1 /proc/cmdline' 2>/dev/null; then
				info "RAM system is up after ${waited}s on $CURRENT_TARGET"
				RECOVERY_TARGET=$CURRENT_TARGET
				probe_privilege
				return 0
			fi
		done
	done
	die "the RAM system did not answer within ${waited}s at $(recovery_candidates | tr '\n' ' ').
      It may hold a different DHCP lease, because it identifies itself as
      \"laserbridge\" rather than as the installed system. Look for it on the
      network and set LASERBRIDGE_RECOVERY_SSH, or power-cycle the device to
      return to the installed system."
}

confirm_install() {
	uncompressed_bytes=$(gzip -l "$DISK_IMAGE" 2>/dev/null | awk 'NR==2 {print $2}')
	cat <<EOF

  Target host:     $TARGET_HOSTNAME ($SSH_TARGET)
  Target disk:     $CHOSEN_DISK
  Disk size:       $(human "$CHOSEN_DISK_BYTES")
  Image:           $MANIFEST_VERSION
  Image size:      $(human "${uncompressed_bytes:-0}") written
  Mode:            PERMANENT INSTALL

  Everything on $CHOSEN_DISK is destroyed, including the data partition:
  SSH keys, Wi-Fi credentials and all appliance settings. After the reboot
  the device starts its first-boot setup access point. If it has no network
  cable, it will only be reachable over that access point.

EOF
	[ "$ASSUME_YES" = "yes" ] && { info "Proceeding (--yes)"; return 0; }

	# Ask on the terminal rather than on stdin, which may be carrying piped
	# input. Not every context has a controlling terminal though - a command
	# run from an editor or an agent typically has none - so fall back to
	# stdin when that is a terminal, and refuse to guess when neither is.
	if { exec 3<>/dev/tty; } 2>/dev/null; then
		printf '  Type the target disk (%s) to continue: ' "$CHOSEN_DISK" >&3
		read -r answer <&3
		exec 3>&-
	elif [ -t 0 ]; then
		printf '  Type the target disk (%s) to continue: ' "$CHOSEN_DISK"
		read -r answer
	else
		die "no terminal available to confirm on.
      Re-run this from an interactive shell, or pass --yes if the summary
      above is what you intend. --yes is the only confirmation there is."
	fi
	[ "$answer" = "$CHOSEN_DISK" ] || die "aborted"
}

write_disk_image() {
	info "Streaming $(basename "$DISK_IMAGE") to $CHOSEN_DISK"
	# Decompressed on this machine and piped straight onto the disk, so the
	# target never has to store the image anywhere.
	gzip -dc "$DISK_IMAGE" | remote_stdin write-disk "$CHOSEN_DISK" ||
		die "writing the disk image failed; NOT rebooting"
	info "Write finished and synced"

	if [ "$VERIFY" = "yes" ]; then
		info "Verifying what was written (reading the disk back)"
		raw_name=$(basename "$DISK_IMAGE" .gz)
		expected=$(sed -n "s/^\([0-9a-f]*\)  $raw_name$/\1/p" "$SUMS")
		bytes=$(gzip -l "$DISK_IMAGE" 2>/dev/null | awk 'NR==2 {print $2}')
		if [ -z "$expected" ] || [ -z "$bytes" ]; then
			warn "no checksum for $raw_name in SHA256SUMS; skipping read-back"
		else
			actual=$(remote verify-disk "$CHOSEN_DISK" "$bytes") ||
				die "could not read the disk back; NOT rebooting"
			[ "$actual" = "$expected" ] ||
				die "read-back mismatch on $CHOSEN_DISK; NOT rebooting
      expected $expected
      actual   $actual"
			info "Read-back matches the image checksum"
		fi
	fi
}

show_status() {
	echo
	echo "  Host:      $TARGET_HOSTNAME ($CURRENT_TARGET)"
	echo "  Arch:      $TARGET_ARCH"
	echo "  Kernel:    $TARGET_KERNEL"
	echo "  Memory:    $(human $((TARGET_MEM_KB * 1024))) total, $(human $((TARGET_MEM_FREE_KB * 1024))) available"
	echo "  kexec:     $TARGET_KEXEC"
	if [ "$TARGET_RAMBOOT" = "yes" ]; then
		echo "  Running:   RAM image (the system disk is not mounted)"
	else
		echo "  Running:   installed system from disk"
	fi
	echo "  Disks:"
	target_disks | sed 's/^/    /'
	echo
}

# ---------------------------------------------------------------- entry point

while [ $# -gt 0 ]; do
	case "$1" in
		--test|--install|--recovery|--status|--check)
			[ -z "$MODE" ] || die "choose one mode"
			MODE=${1#--}
			;;
		--yes|-y) ASSUME_YES=yes ;;
		--resume) RESUME=yes ;;
		--dry-run) DRY_RUN=yes ;;
		--no-verify) VERIFY=no ;;
		-h|--help) usage ;;
		-*) die "unknown option $1" ;;
		*) [ -z "$IMAGE_DIR" ] || die "unexpected argument $1"; IMAGE_DIR=$1 ;;
	esac
	shift
done
[ -n "$MODE" ] || usage

load_env
set_ssh_options
CURRENT_TARGET=$SSH_TARGET

case "$MODE" in
	test|install|check) [ -n "$IMAGE_DIR" ] || die "--$MODE needs an image directory" ;;
	recovery) [ -n "$IMAGE_DIR" ] || IMAGE_DIR="$SELF_DIR/dist" ;;
esac

if [ -n "$IMAGE_DIR" ]; then
	find_artifacts
	verify_checksums
	extract_bundle
	# Resuming talks to a system that is already running from RAM, so no
	# initramfs has to be built or transferred.
	[ "$RESUME" = "yes" ] || build_kexec_initramfs
fi

# --resume picks up an appliance that is already in recovery mode. On a
# Wi-Fi-only device that is the normal case rather than an exception: a RAM
# boot starts with an empty /data, has no credentials for the operator's
# network, and therefore raises its own setup access point. Point
# LASERBRIDGE_RECOVERY_SSH at it - usually laserbridge@10.42.0.1 - and carry
# on from there instead of kexecing a second time.
if [ "$RESUME" = "yes" ]; then
	[ "$MODE" = "install" ] || die "--resume only applies to --install"
	switch_to_recovery_ssh
	info "Resuming against the running RAM system on $CURRENT_TARGET"
fi

probe_privilege
probe_target

if [ "$MODE" = "status" ]; then
	show_status
	exit 0
fi

[ "$MODE" = "install" ] && choose_disk
run_checks

if [ "$MODE" = "check" ]; then
	info "All preflight checks passed; nothing was changed"
	exit 0
fi

if [ "$DRY_RUN" = "yes" ]; then
	echo
	echo "  Dry run - the following would happen:"
	step=1
	if [ "$RESUME" = "no" ]; then
		echo "    $step. stage the RAM image on $CURRENT_TARGET"; step=$((step + 1))
		echo "    $step. kexec into it (mode: $MODE)"; step=$((step + 1))
	else
		echo "    (the target already runs from RAM; no kexec)"
	fi
	[ "$MODE" = "install" ] && {
		echo "    $step. stream $(basename "$DISK_IMAGE") onto $CHOSEN_DISK (DESTROYS ALL DATA)"
		step=$((step + 1))
		echo "    $step. verify the write, then reboot"
	}
	echo
	exit 0
fi

case "$MODE" in
	test)
		kexec_into_ram ""
		wait_for_recovery
		probe_target
		[ "$TARGET_RAMBOOT" = "yes" ] ||
			die "the target came back but is not running from RAM"
		cat <<EOF

  The image is running from RAM. The system disk was not touched.

    Not good?  ssh $RECOVERY_TARGET 'doas reboot'   - the installed system returns
    Good?      ./deploy.sh --install $IMAGE_DIR

EOF
		;;
	recovery)
		kexec_into_ram "laserbridge.mode=recovery"
		wait_for_recovery
		info "Recovery system is up: ssh $RECOVERY_TARGET"
		info "Available: dd, zstd, lsblk, blkid, mount, sha256sum, kexec"
		;;
	install)
		confirm_install
		if [ "$RESUME" = "no" ]; then
			kexec_into_ram "laserbridge.mode=recovery"
			wait_for_recovery
			probe_target
			choose_disk
		fi
		# Checked again on the system that is about to be written, not only on
		# the one that was asked to hand over. Writing a disk the running
		# system lives on is the one mistake this workflow exists to avoid.
		[ "$TARGET_RAMBOOT" = "yes" ] ||
			die "refusing to write: the target is not running from RAM"
		write_disk_image
		info "Rebooting into the installed system"
		remote power reboot >/dev/null 2>&1 || true
		;;
esac
