# Remote deployment

`deploy.sh` boots an appliance image from RAM on real hardware, and installs
that same image permanently once it has proven itself. Everything runs over
SSH: no USB stick, no monitor, no live medium.

```
build the image
      |
./deploy.sh --test  ./dist        image runs from RAM, disk untouched
      |
      +-- not good?  reboot -> the installed system is back, unchanged
      |
      +-- good?      ./deploy.sh --install ./dist
                           |
                     kexec into the RAM recovery system
                           |
                     image streamed from this machine onto the disk
                           |
                     verified, then rebooted
```

## Status

`--test` has been run end to end against real hardware: a Z83F mini PC
running Ubuntu 24.04 (kernel 6.8) kexec'd into the LaserBridgeOS image and
came up serving its web interface from RAM, with the 58 GiB eMMC untouched -
`/api/status` reported 64 MiB of storage, which is the tmpfs `/data`.

Launching from LaserBridgeOS itself does not work yet; see "The kexec
blocker".

### Finding the appliance between the two phases

This is a step of the procedure, not an exception to it. After the kexec the
appliance is a different machine on the network than the one you handed over
to, and there are three places it can be:

| Where | When |
| --- | --- |
| a new DHCP lease | it has credentials for a network and joined it |
| `10.42.0.1` | it has none, so it raised `LaserBridge-XXXXXX` itself |
| the old address | only if the router happened to reuse the lease |

`deploy.sh` tries the configured target, `laserbridge.local` and `10.42.0.1`.
Name it directly whenever you know better — mDNS can answer late, or answer
with an address from a previous session:

```sh
./deploy.sh --resume --install --recovery-host laserbridge@10.42.0.1 ./dist
```

`--resume` exists for this: it skips the kexec and writes to the RAM system
already running, so a lost or changed address costs a command line rather
than the whole deployment.

### The RAM system takes a different address

It sends `laserbridge` as its DHCP hostname rather than the installed
system's, so the router usually hands it a **different lease**. In the run
above the device moved from `.83` to `.166`. `deploy.sh` therefore also looks
for `laserbridge.local` over mDNS, and `LASERBRIDGE_RECOVERY_SSH` pins the
address when neither applies. If a deployment seems to hang after the kexec,
the appliance is most likely up and answering somewhere else.

## Configuration

Copy `.env.example` to `.env` and set at least `LASERBRIDGE_SSH`. The file is
git-ignored because it names a host and may hold a sudo password.
`LIGHTBURN_*` names are accepted as aliases.

## The artefacts

`make image` produces everything the deployment needs in `dist/`:

| File | Used by | Contents |
| --- | --- | --- |
| `*.lbu` | `--test`, `--recovery` | `manifest.json`, `root.squashfs`, `vmlinuz-lts`, `initramfs-lts` |
| `*.img.gz` | `--install` | the full disk image: GPT, ESP, both root slots, data partition |
| `SHA256SUMS` | all modes | checksums for the above |

There is no separate test build. `--test` boots the `root.squashfs` from the
bundle; `--install` writes the disk image whose slot A holds that same
SquashFS. `deploy.sh` checks every payload against the manifest digests
before it transfers anything.

## Modes

### `--test <image-dir>`

Loads kernel and a combined initramfs into the target's RAM and kexecs into
them. The root filesystem is an overlay: the image's SquashFS read-only
underneath, a tmpfs on top. `/data` is a tmpfs as well.

The system disk is not merely left alone, it is unreachable: the initramfs
rewrites `/etc/fstab` in the overlay before handing over, so nothing mounts
it. This was verified by comparing the disk's SHA-256 before and after a RAM
boot - byte for byte identical.

`reboot` returns to the installed system. Nothing has to be undone.

Running the first-boot wizard inside a RAM session is fine: `/data` is a
tmpfs, so the settings live only for that session, and the deployment key is
kept alongside the key chosen in the wizard. Without that the wizard would
replace `authorized_keys` and lock out the machine that started the session.

### `--install <image-dir>`

Same RAM boot, but in recovery mode, and then the disk image is streamed from
this machine straight onto the block device - it is never stored on the
target. Afterwards the disk is read back and its checksum compared against
`SHA256SUMS`. Only then does the appliance reboot; any failure stops before
the reboot so the situation can be inspected.

**This erases the whole disk, including `/data`.** SSH keys, Wi-Fi
credentials and every setting are gone; the device comes back in first-boot
setup mode with its setup access point. On a remote device without a network
cable that means a visit. `deploy.sh` prints the target, the disk and its size
and requires the disk path to be typed back before it proceeds; `--yes` skips
that prompt.

### `--recovery`

Boots only the RAM recovery system: network, SSH, and `dd`, `zstd`, `lsblk`,
`blkid`, `mount`, `sha256sum`, `kexec`. The appliance services stay stopped.

In this mode the `laserbridge` account has **full root through `doas`**, not
just the deployment helper. Repairing a machine by hand means mounting
filesystems, writing block devices and reconfiguring interfaces, none of which
the appliance's narrow rule allows; without this the tools would be installed
but unusable. The rule is written into the RAM overlay while the session
starts and disappears with it, and the session is only reachable by whoever
holds the deployment key or the access point password.

On an installed appliance nothing changes: root login stays disabled and
`doas` still grants exactly one program (ADR 0005).

### `--status`, `--check`, `--dry-run`

`--status` reports what the target runs, how much memory it has and which
disks it sees. `--check` runs every preflight test and changes nothing.
`--dry-run` prints the steps a real run would take.

## What is verified before anything happens

- every artefact against `SHA256SUMS` and against the manifest digests
- image architecture against the target's
- kexec present *and* permitted by the running kernel
- enough RAM: the image is briefly resident twice, so roughly twice the
  initramfs plus 256 MiB
- exactly one fixed disk, or `LASERBRIDGE_DISK` naming which one

## Privileges on the target

kexec and raw disk writes need root, but the appliance keeps root login
disabled (ADR 0002). Instead the `laserbridge` account may run exactly one
program through `doas`: `/usr/sbin/laserbridge-deploy`, which implements the
fixed set of privileged operations and validates its own arguments.

On a foreign Linux there is no such rule; `deploy.sh` uploads the same helper
and uses `sudo`, falling back to `LASERBRIDGE_PASSWORD` to prime sudo's
timestamp when the account needs a password.

## The kexec blocker

Alpine's `linux-lts` boots with `kernel.kexec_load_disabled = 1`. Nothing in
userspace sets it - no sysctl file, and kernel lockdown is inactive - so the
kernel itself starts out this way. That sysctl is deliberately one-way: the
kernel accepts writing `1` and rejects writing `0`, so it cannot be undone at
runtime. The same kernel is built with `CONFIG_KEXEC_FILE` unset, so the
modern `kexec -s` path is missing too.

The practical consequence: `kexec -l` fails with `kexec_load failed:
Operation not permitted`, and no configuration change on the appliance can
alter that. `deploy.sh --check` reports this instead of failing halfway
through a transfer.

Everything else in the workflow is verified and works. Getting the trigger
working needs one of:

- **A kernel with kexec enabled.** Build `linux-lts` with `CONFIG_KEXEC_FILE=y`
  and without the disabled default, or use another kernel. Cleanest result,
  but the project then maintains a kernel.
- **A GRUB one-shot RAM boot instead of kexec.** Stage kernel and initramfs
  on the data partition, set a flag in `grubenv`, and reboot; GRUB boots the
  RAM image once and falls back to normal afterwards. No custom kernel and it
  reuses the boot-counter machinery from ADR 0004, but it is a full reboot
  rather than a kexec, and staging writes to the disk - which `--test` is
  otherwise careful not to do.
- **A foreign Linux as the launch pad.** Debian and most distributions permit
  `kexec_load`. For the bare-metal first install, where the target is not yet
  running LaserBridgeOS, the workflow already works today.
