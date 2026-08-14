# LaserBridgeOS

LaserBridgeOS is a small, headless Alpine Linux appliance for exposing a
GRBL controller and a USB camera on a local network. It boots from an
immutable SquashFS root, keeps runtime state in RAM, and stores the small
amount of durable state under `/data`.

## LightBurn and older GRBL lasers

LaserBridgeOS is an ideal companion for LightBurn's GRBL network mode. It
turns the USB serial connection of a GRBL laser into a raw TCP endpoint at
`laserbridge.local:23`. LightBurn can therefore reach the controller over the
local network while LaserBridgeOS remains a transparent bridge and does not
take over job processing.

This is especially useful for making older USB-only machines, such as a
SCULPFUN S9, network-capable without replacing their controller. The laser can
stay in the workshop while LightBurn runs on another computer connected by
Ethernet or Wi-Fi.

```text
LightBurn PC ── Ethernet / Wi-Fi ── LaserBridgeOS ── USB ── GRBL laser
                                          │
                                          └── USB camera
```

### Connect from LightBurn

1. Add a GRBL device in LightBurn.
2. Select its network connection instead of a local USB serial port. The exact
   option label depends on the LightBurn version.
3. Enter `laserbridge.local` as the host and `23` as the TCP port. If mDNS is
   unavailable, use the IP address shown in the LaserBridgeOS dashboard.
4. Make sure no terminal or other application is holding the bridge connection
   and connect the device.

The endpoint is raw TCP, not Telnet or RFC2217. It transparently carries the
GRBL serial protocol at the configured baud rate. LaserBridgeOS permits one
client by default, which prevents two applications from controlling the laser
at the same time.

## Supported hardware

The current image is built exclusively for **x86_64 UEFI systems**. It is
intended to give compact or retired Intel/AMD computers a new purpose.

| Platform | Compatibility status | Notes |
| --- | --- | --- |
| x86_64 UEFI QEMU | Verified | Automated boot and service smoke test |
| Z83F mini PC | Primary hardware target | Physical compatibility report pending |
| Intel N100 mini PC | Expected | Must boot in x86_64 UEFI mode |
| Intel NUC | Expected | Must boot in x86_64 UEFI mode |
| x86_64 thin client | Expected | UEFI and supported network hardware required |
| Older x86_64 office PC | Expected | UEFI required; Ethernet is the safest setup path |

Physical hardware is only marked verified after a successful installation has
been reported. Contributions to this compatibility table are welcome.

The current image does **not** support ARM, ARM64, Raspberry Pi, or similar
single-board computers. ARM64 support may be added later as a separate image.

### Target system requirements

- x86_64 CPU and UEFI firmware; Legacy BIOS boot is not supported;
- 512 MB RAM minimum, 1 GB or more recommended;
- target disk of at least 2 GB;
- one USB port for the GRBL controller;
- Ethernet, or a Linux-supported Wi-Fi adapter capable of AP mode;
- optional USB camera with UVC/V4L2 support.

The image includes firmware for common Intel, Broadcom, and Realtek Wi-Fi
devices. Firmware alone cannot guarantee setup-AP support: the adapter and its
Linux driver must also implement AP mode. Wired Ethernet remains available as
the recovery and provisioning path.

Realtek RTL8821CU support is included in the current x86_64 image through
Alpine's in-kernel `rtw88_8821cu` module and `rtw8821c_fw.bin` firmware. Most
common Realtek USB IDs are handled by that driver. A particular dongle, and
especially concurrent AP/scan operation, still depends on its exact USB ID,
board revision, and the upstream Linux driver.

The primary controller target is GRBL 1.1 over USB serial, including devices
that appear as `/dev/ttyUSB*` or `/dev/ttyACM*`. Ruida, Trocen, and other
proprietary DSP controllers are not supported. Cameras should expose a standard
V4L2 interface; MJPEG is preferred and YUYV is used as a software-encoding
fallback.

The MVP provides:

- raw GRBL serial-to-TCP bridging with `ser2net` on port 23;
- an MJPEG camera stream with `ustreamer` on port 8080;
- a responsive dashboard and configuration UI on port 80;
- SSH and Bonjour/mDNS (`laserbridge.local`);
- first-boot Wi-Fi setup AP and a browser-based onboarding wizard;
- Wi-Fi scanning without intentionally stopping the setup AP;
- a unique initial Ed25519 key for the `laserbridge` SSH account;
- owner-signed, offline A/B system updates with rollback to the previous slot;
- central validated configuration in `/data/config.yaml`, exposed at
  `/etc/laserbridge/config.yaml`;
- a directly flashable x86_64 UEFI disk image.

## Quick start

Requirements: Docker with BuildKit, GNU Make, and roughly 4 GB of free disk
space. On macOS, Docker Desktop is sufficient.

```sh
make test
make image
```

The image and checksum are written to `dist/`:

```text
dist/LaserBridgeOS-x86_64.img
dist/LaserBridgeOS-x86_64.img.gz
dist/LaserBridgeOS-x86_64-0.1.0.lbu
dist/SHA256SUMS
```

Flash the uncompressed image with Balena Etcher, Raspberry Pi Imager, or:

```sh
sudo dd if=dist/LaserBridgeOS-x86_64.img of=/dev/sdX bs=4M conv=fsync status=progress
```

### First boot

1. Flash the complete image and boot the target in x86_64 UEFI mode.
2. Join the unique `LaserBridge-XXXXXX` network using password
   `laserbridge-setup`. With Ethernet attached, skip this step.
3. Open `http://10.42.0.1`, or `http://laserbridge.local` over Ethernet.
4. Scan for a target network or enter its SSID manually, then enter the
   hostname and WPA2 Wi-Fi credentials.
5. Choose a new SSH password, or download the generated key, or paste an
   existing public key.
6. Finish setup, reconnect the computer to the target network, and open
   `http://laserbridge.local`.

After setup, connect with:

```text
ssh laserbridge@laserbridge.local
http://laserbridge.local
tcp://laserbridge.local:23
http://laserbridge.local:8080
```

> **Note on the captive-portal window.** macOS and iOS open a small
> stripped-down window when you join the setup hotspot. It works for the
> wizard but **cannot save downloads**, so the key download appears to do
> nothing there. The wizard therefore also shows the key as copyable text.
> For the full experience open `http://10.42.0.1` in a normal browser.

### SSH access

The `laserbridge` account ships with the password **`laserbridge`** so the
appliance is reachable straight away, without a key file. It is the same on
every image and is therefore a convenience, not a secret — the setup wizard
asks for a new one.

Key authentication works alongside it and stays available: the wizard can
install the device-generated key or your own public key, and there is no
shared default private key. On a network you do not fully trust, install a key
and switch **SSH password authentication** off on the System page. Root SSH
login is disabled and empty passwords are rejected regardless. See ADR 0006.

## A/B system updates

`make image` also creates an update bundle for the built version. Updates are
deliberately local and owner-authorized, in one of two ways.

**With the appliance password.** Upload the `.lbu` on the System page and
enter the password. Nothing else is needed — this is the path for an
appliance set up without a key file. The shipped default password is refused
for this on purpose; choose your own first, in the setup wizard or on the
System page.

**With a signature.** Sign the exact `.lbu` with an SSH key whose public half
is installed on the appliance, then upload bundle and signature together:

```sh
scripts/sign-update.sh \
  dist/LaserBridgeOS-x86_64-0.1.0.lbu \
  laserbridge_ed25519
```

A signature proves the bundle came from the key holder; a password only
proves the uploader knew it. Prefer signatures where the network is not
entirely yours. See ADR 0007.

The appliance verifies the OpenSSH signature and every payload checksum,
writes only the inactive root slot, and changes the next boot slot last. The
current configuration and SSH keys under `/data` are retained. Reboot after a
successful installation. The WebUI can stage the previous slot for the next
boot. There is deliberately no interactive boot menu or boot delay. Offline
recovery can select a slot by editing `boot/active-slot.cfg` on `LBBOOT`.

## Remote deployment and RAM testing

`deploy.sh` boots a candidate image from RAM on the real device over SSH, so
it can be judged on the hardware before anything is committed to the disk. A
reboot returns to the installed system, unchanged; if the image is good, the
same artefacts can be written to the disk permanently.

```sh
./deploy.sh --test ./dist        # run from RAM, system disk untouched
./deploy.sh --install ./dist     # write that same image to the disk
./deploy.sh --recovery           # RAM system with dd, zstd, lsblk, blkid
```

Run `./deploy.sh --check ./dist` first: it verifies every artefact, the
architecture, the available memory and the target disk without changing
anything.

### How a deployment runs

Both `--test` and `--install` start the same way. `deploy.sh` unpacks the
`.lbu`, checks each payload against the manifest digests, and prepends
`root.squashfs` to `initramfs-lts` as its own uncompressed cpio segment — the
arrangement the kernel already uses for early microcode. Your public key goes
into that segment too, because a RAM boot starts with an empty `/data` and
would otherwise authorize nobody.

That combined initramfs and the kernel are staged in a tmpfs on the target,
loaded with `kexec -l`, and started. From there:

- **`--test`** boots the full appliance from RAM. The root is an overlay with
  the image's SquashFS read-only underneath and a tmpfs on top, and `/data`
  is a tmpfs as well. The system disk is not merely left alone but
  unreachable: the initramfs rewrites `/etc/fstab` in the overlay before
  handing over, so nothing can mount it. `reboot` returns to the installed
  system with nothing to undo.
- **`--install`** boots the same image in recovery mode, then streams
  `LaserBridgeOS-x86_64.img.gz` from your machine straight onto the block
  device. The target never stores the image. Afterwards the disk is read back
  and its SHA-256 compared against `SHA256SUMS`; only then does the appliance
  reboot. Any failure stops before the reboot.

### What is verified, and how

| Step | Evidence |
| --- | --- |
| kexec into RAM | Z83F running Ubuntu 24.04, twice |
| Recovery mode, disk untouched | `--status` on the device reports `/dev/mmcblk0 mounted=no` |
| `--test` leaves no trace | disk byte-identical before and after (QEMU) |
| Refuses to write a mounted disk | attempt rejected with an explanation |
| Streaming write | 962 MiB at 13 MB/s onto a blank disk |
| Read-back verification | checksum matched `SHA256SUMS` exactly |
| The written disk boots | comes up in first-boot setup with its own AP SSID |
| Privileged half on the appliance | `doas` rule exercised on real hardware |

The orchestration of `--install` as a single command — confirm, kexec, wait,
select, write, verify, reboot — has not yet run end to end on hardware; each
of its steps has.

### Before you install permanently

`--install` erases the whole disk, `/data` included. Three consequences are
worth weighing first, and the first one decides it on most hardware.

**The appliance comes back with no network configuration.** Wi-Fi
credentials live in `/data`. After the wipe the device boots into first-boot
setup and raises its setup access point. With a network cable attached it is
reachable immediately over DHCP; **without one it is only reachable on site**,
by joining `LaserBridge-XXXXXX` from a phone or laptop. Check for an Ethernet
port before starting — a mini PC such as the Z83F may have Wi-Fi only.

**The launch pad disappears.** Whatever Linux was installed is what made
`kexec` possible. Once LaserBridgeOS occupies the disk, the RAM test workflow
is unavailable until the kexec blocker below is resolved.

**The disk keeps the image's layout.** A 962 MiB image on a 58 GiB disk
leaves the remainder unallocated, `/data` fixed at 289 MiB, and the GPT
backup header where the image put it rather than at the end of the disk. It
boots and runs; it does not use the space.

### Installing a Wi-Fi-only device

A RAM boot starts with an empty `/data`. It therefore has no credentials for
your network and, because setup is not complete, raises its own first-boot
access point instead. On a device with a network cable that is invisible; on a
Wi-Fi-only device it means the whole installation would run over that access
point — and `hostapd` serves it in 802.11g, which measured **196 KiB/s** on a
Z83F. Even compressed, the image needs half an hour that way, and the client
machine has to stay associated for all of it. macOS will not: it prefers a
known network with internet access and roams back to it mid-transfer, which
is what broke two attempts here.

Give the appliance a fast path before writing:

1. `./deploy.sh --test ./dist` — the full appliance runs from RAM, web
   interface included.
2. Join `LaserBridge-XXXXXX`, open `http://10.42.0.1`, and complete the setup
   wizard with your Wi-Fi credentials. The RAM session joins your network.
3. Reconnect your computer to that same network.
4. `./deploy.sh --resume --install ./dist` with
   `LASERBRIDGE_RECOVERY_SSH=laserbridge@laserbridge.local`.

`--resume` writes to an appliance that is already running from RAM instead of
kexecing into it again, which is what makes this two-step route possible.

If you do install over the access point anyway, keep the client associated:
turn off auto-join for your usual network first, and stay close to the device.

### Installing

```sh
./deploy.sh --dry-run --install ./dist   # prints the plan, changes nothing
./deploy.sh --install ./dist
```

The second command prints the target, the disk, its size and the image
version, and requires the disk path to be typed back before it proceeds.
`--yes` skips that prompt and should be reserved for unattended use — it
removes the last thing standing between a typo and a wiped disk.

Expect the run to take several minutes: roughly 180 MiB of initramfs cross
the network before the kexec, then the compressed image, then a full read-back
of the disk for verification. `deploy.sh` reports each stage. If it seems to
stall right after `Rebooting into RAM via kexec`, the appliance is probably up
under a different DHCP lease; it is searched for under `laserbridge.local`
as well.

After the reboot the appliance is in factory state: setup wizard pending,
default password, no Wi-Fi.

### Open point: the kexec trigger

The RAM boot itself is verified — the overlay mounts, `/data` is a tmpfs, no
partition of the system disk is mounted, and the disk is byte-identical
afterwards. What does not work yet is triggering it *from LaserBridgeOS
itself*: Alpine's stock `linux-lts` starts with `kernel.kexec_load_disabled=1`
(no sysctl file sets it, kernel lockdown is inactive), and that sysctl only
accepts being written `1`, so it cannot be re-enabled at runtime.
`CONFIG_KEXEC_FILE` is unset too, so `kexec -s` is missing as well.

Launching from a foreign Linux — Debian, Ubuntu, stock Alpine — works today
and is verified on real hardware: a Z83F running Ubuntu 24.04 kexec'd into
the image and served its web interface from RAM with its eMMC untouched. That
covers bare-metal first installation. Note that the RAM system asks DHCP for
the name `laserbridge` and so usually appears under a **different address**;
`deploy.sh` also looks for `laserbridge.local`.

Two ways to close the remaining gap, not yet decided:

| Option | Gains | Costs |
| --- | --- | --- |
| Build `linux-lts` with `CONFIG_KEXEC_FILE=y` and no disabled default | real kexec; `--test` stays write-free | the project maintains a kernel |
| GRUB one-shot RAM boot instead of kexec: stage kernel and initramfs on `/data`, set a flag in `grubenv`, reboot | no custom kernel; reuses the boot-counter machinery from ADR 0004 | a full reboot rather than a kexec, and staging writes to the disk, which `--test` otherwise avoids |

`docs/deploy.md` and ADR 0005 carry the detail.

## Fast headless boot

The appliance boots the active A/B slot directly from GRUB without displaying
a menu or waiting for keyboard input. Independent OpenRC services start in
parallel, DHCP falls into the background quickly when no lease is immediately
available, and WebUI, SSH, GRBL bridge, and camera startup do not wait for an
assigned network address. The initramfs uses fast zstd decompression and does
not include early KMS or keymap support. Alpine's unused virtual-console gettys
are disabled, and OpenRC suppresses normal console status chatter while still
reporting errors.

`rootwait` remains enabled intentionally: USB, eMMC, and some SATA controllers
can appear asynchronously, and removing it would trade a negligible successful
boot cost for sporadic boot failures.

## Security notes

Two passwords are intentionally known and shared by every image: the
first-boot hotspot password `laserbridge-setup`, and the SSH password
`laserbridge` for the appliance account. Both exist for convenient local
provisioning of a device on an isolated workshop network, and both should be
dealt with promptly: complete setup in a physically trusted environment, and
choose a new SSH password when the wizard offers it. The setup AP is disabled
after successful onboarding, and the generated private SSH key is then removed
from the appliance.

The WebUI has no login. Anyone who can reach port 80 can reconfigure the
appliance, restart services, or reboot it. Two operations are exceptions
because they can replace the running system: installing an update and
changing the appliance password both require that password, and the shipped
default is not accepted for either (ADR 0007). It travels in clear text over
HTTP, like everything else this interface handles, which is only acceptable
on a network you control.

LaserBridgeOS is intended for a trusted local network. Do not forward the WebUI,
SSH, GRBL port 23, or camera port 8080 from an Internet-facing router. The MVP
does not terminate HTTPS, so management traffic should not cross an untrusted
network.

## Troubleshooting

### The setup Wi-Fi does not appear

- Wait for the appliance to finish its first boot.
- Prefer wired Ethernet and open `http://laserbridge.local`.
- Check whether the Wi-Fi adapter and driver support AP mode. Some adapters
  support client mode only.
- Try a supported USB Wi-Fi adapter if the built-in device is unavailable.

### `laserbridge.local` cannot be resolved

- During setup, use `http://10.42.0.1` while connected to the setup AP.
- After setup, find the assigned address in the router's DHCP leases and use
  that address directly.
- Ensure the client network permits multicast DNS/Bonjour traffic.

### LightBurn cannot connect to GRBL

- Verify the configured host and raw TCP port `23`.
- Close serial terminals and other clients; only one client is allowed by
  default.
- Open the WebUI and confirm that the GRBL device exists and `ser2net` is
  running.
- Select the detected `/dev/serial/by-id/` path when available, otherwise use
  the matching `/dev/ttyUSB*` or `/dev/ttyACM*` device.
- Check the USB cable and confirm that the controller uses the configured baud
  rate, normally `115200` for GRBL 1.1.

### The camera stream is unavailable

- Confirm that the camera appears on the Camera page.
- Try YUYV when the device does not provide MJPEG.
- Reduce the resolution or frame rate for older hardware or USB 2.0 cameras.
- Prefer a stable `/dev/v4l/by-id/` path when one is available.

### Recovery or factory reset

Configuration and SSH keys live on the writable `LBDATA` partition. Back up
`config.yaml` and `ssh/authorized_keys` before replacing a disk or reflashing.
If networking is misconfigured, use Ethernet or mount `LBDATA` on another
computer to repair the configuration. Reflashing the image or clearing the
entire `LBDATA` partition performs a factory reset and destroys its settings
and keys.

A device that is unreachable but still boots can also be repaired without
opening it: `./deploy.sh --recovery` starts the RAM system over SSH, which
brings `dd`, `zstd`, `lsblk`, `blkid`, `mount` and `sha256sum` while leaving
the system disk unmounted. That needs a Linux on the device that permits
`kexec`, so it does not help once LaserBridgeOS itself is installed.

See [the build guide](docs/build.md), [installation guide](docs/install.md),
[remote deployment](docs/deploy.md), and
[architecture](docs/architecture.md) for details.

## Development

The backend uses only the Go standard library.

```sh
make fmt
make test
make check
make run
```

`make run` serves the UI on `127.0.0.1:8088` with state stored below
`.local-data/`; appliance-only service actions are rejected unless the
process runs as root on Alpine.

## Project status

This repository implements the initial appliance plus local, owner-signed A/B
updates. Cloud services, multi-user management, job streaming, automatic
release discovery, and fleet update orchestration are deliberately out of
scope. See [MVP tasks](docs/mvp.md).

### Known limitations

- x86_64 UEFI only; no Legacy BIOS, ARM, ARM64, or Raspberry Pi image;
- WPA2-Personal only; no WPA-Enterprise or WPA3-only provisioning;
- setup-AP availability depends on the Wi-Fi chipset and Linux driver;
- some adapters cannot scan while serving the setup AP; manual SSID entry
  remains available;
- static-IP migration and automatic update discovery are not implemented;
- `deploy.sh` cannot kexec out of LaserBridgeOS itself, because Alpine's
  kernel forbids `kexec_load`; a foreign Linux is needed as the launch pad;
- a permanent install writes the image's 962 MiB layout and leaves the rest
  of a larger disk unallocated; `/data` does not grow to fit;
- one GRBL TCP client by default;
- no G-code storage, job streaming, editor, or replacement for LightBurn;
- no TLS termination or WAN-facing security boundary.

## License

MIT. See [LICENSE](LICENSE).

The generated image also contains third-party software under its respective
licenses, including Alpine Linux packages and GPL-3.0-licensed ustreamer.
