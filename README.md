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
5. Download the unique generated SSH key, or install an existing public key.
6. Finish setup, reconnect the computer to the target network, and open
   `http://laserbridge.local`.

After setup, connect with:

```text
ssh -i laserbridge_ed25519 laserbridge@laserbridge.local
http://laserbridge.local
tcp://laserbridge.local:23
http://laserbridge.local:8080
```

The `laserbridge` account has no valid password by default. The setup wizard
installs either the device-generated key or an existing public key. There is
no shared default private key. Root SSH login and SSH password authentication
are disabled by default.

## A/B system updates

`make image` also creates an update bundle for the built version. Updates are
deliberately local and owner-authorized: sign the exact `.lbu` file with an
SSH key whose public half is installed for the appliance, then upload both
files on the System page.

```sh
scripts/sign-update.sh \
  dist/LaserBridgeOS-x86_64-0.1.0.lbu \
  laserbridge_ed25519
```

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

The RAM boot is verified, but Alpine's stock kernel forbids `kexec_load` and
cannot be made to allow it at runtime, so the transition currently only works
from a foreign Linux. `docs/deploy.md` explains the constraint and the two
ways out; `./deploy.sh --check ./dist` reports whether a given target
qualifies.

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

The temporary first-boot password `laserbridge-setup` is intentionally known
and exists for convenient local provisioning. Complete setup promptly in a
physically trusted environment. The setup AP is disabled after successful
onboarding, and the generated private SSH key is then removed from the
appliance.

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

See [the build guide](docs/build.md), [installation guide](docs/install.md),
and [architecture](docs/architecture.md) for details.

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
- update rollback is explicit; there is no automatic boot-attempt counter;
- one GRBL TCP client by default;
- no G-code storage, job streaming, editor, or replacement for LightBurn;
- no TLS termination or WAN-facing security boundary.

## License

MIT. See [LICENSE](LICENSE).

The generated image also contains third-party software under its respective
licenses, including Alpine Linux packages and GPL-3.0-licensed ustreamer.
