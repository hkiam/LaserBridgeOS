# Architecture

## Design goals

LaserBridgeOS behaves like firmware: it has one purpose, starts without user
interaction, survives loss of power, and can be reproduced as a complete disk
image. General-purpose package management on the running appliance is not a
goal.

## Storage and boot

```text
UEFI firmware
  -> GRUB EFI + Linux kernel/initramfs (LBBOOT, FAT32)
  -> active immutable Alpine root (LBROOTA or LBROOTB, SquashFS)
  -> volatile /run, /tmp and /var/log (tmpfs)
  -> persistent configuration and keys (LBDATA, ext4, mounted at /data)
```

The active root partition is never remounted read-write. The inactive root is
written only while installing a verified system update. The data filesystem
is empty in the image; the first-boot initializer creates its layout, default
configuration, and SSH host keys. DHCP's
`resolv.conf` target and atomic temporary writes live under `/run`. Service
configuration is generated into `/run/laserbridge`, not into `/etc`.

GRUB reads `active-slot.cfg` and immediately loads that slot's kernel and
initramfs. It has no interactive menu or timeout. Before handing over to the
kernel it raises a boot-attempt counter in `boot/grubenv`;
`laserbridge-boot-confirm` resets that counter once the default runlevel is
up. Three attempts that are never confirmed select the other slot, so a slot
that cannot boot is undone without a console. The initramfs reads the slot
from the kernel command line, finds the uniquely labelled `LBBOOT` partition,
and mounts adjacent partition 2 or 3 as the SquashFS root. This accommodates
SATA, USB, NVMe, MMC, and virtio device names; Alpine's stock initramfs does not
resolve GPT `PARTUUID=` values in its final mount command.

`/etc/laserbridge/config.yaml` is a stable symlink to `/data/config.yaml`.
This gives administrators the conventional path while preserving a single
source of truth.

The rationale and rejected alternatives are recorded in
[ADR 0001](adr/0001-immutable-root-and-data-partition.md). First-boot network
and SSH provisioning are covered by
[ADR 0002](adr/0002-first-boot-ap-and-device-ssh-key.md). Wi-Fi scanning and
the A/B update transaction are covered by
[ADR 0003](adr/0003-wifi-scan-and-ab-updates.md).

## Runtime components

| Component | Responsibility | Privilege |
| --- | --- | --- |
| `laserbridge` | HTTP API, static UI, validation, config generation | root |
| `ser2net` | raw serial/TCP bridge | root (serial device) |
| `ustreamer` | V4L2 MJPEG/encoded camera stream | root (video device) |
| `sshd` | key-first administrative access | privilege-separated |
| `avahi-daemon` | `laserbridge.local` and service discovery | avahi user |
| `laserbridge-network` | setup AP or WPA2 client, selected from central config | root |

The backend runs as root because appliance actions (hostname changes, service
control, and reboot) require it. Its attack surface is kept deliberately
small: Go standard library only, fixed command allowlists, no shell execution,
strict input validation, same-origin checks, and CSRF tokens on all mutations.
The default deployment is intended for a trusted LAN and does not expose a
WAN listener through a firewall rule.

## Configuration flow

```text
PUT /api/config
  -> decode JSON with unknown-field rejection
  -> validate complete model
  -> atomic write /data/config.yaml
  -> generate /run/laserbridge/{ser2net.yaml,sshd_config,network-*}
  -> apply hostname
  -> restart only affected services
```

OpenRC calls the same generator during boot. `ustreamer` arguments are
constructed directly from the validated model by `laserbridge run-ustreamer`;
no generated shell text is evaluated. If MJPEG startup fails, the service
wrapper probes formats with `v4l2-ctl` and retries YUYV, letting ustreamer
encode JPEG in software.

Independent default-runlevel services start concurrently. Web, SSH, ser2net,
and ustreamer need generated appliance state but do not wait for DHCP: binding
to all addresses is valid before an interface obtains a lease. Ethernet and
Wi-Fi DHCP make one short foreground attempt and then continue in the
background. The zstd initramfs excludes keymap and early KMS payloads, and no
virtual-console gettys are spawned on the headless appliance. OpenRC runs in
quiet mode so routine service progress is not rendered to an unused console;
errors remain visible when a display is attached for diagnostics.

## Configuration compatibility between slots

`/data/config.yaml` is shared by both system slots, so it is read by whichever
version happens to be booted. Parsing therefore tolerates what it does not know
— unknown sections, unknown keys, unusable values — records each in
`Store.Notes()`, and leaves the file unchanged so the other slot keeps its
settings. The structural shape (sections of flat two-space-indented `key: value`
scalars) remains a hard requirement, since damage and a newer schema are
otherwise indistinguishable. `DisallowUnknownFields` still applies to JSON
arriving at the API.

A configuration that fails validation is quarantined to `config.yaml.broken`
and replaced by `config.Salvage`, which keeps hostname, Wi-Fi and network
settings where each validates on its own and defaults the rest, so a recovery
never costs the appliance its place on the network. `/api/status` reports the
notes as `config_warnings`. See ADR 0016.

## USB power management

USB autosuspend is disabled by default, on the kernel command line
(`usbcore.autosuspend=-1`) for devices probed during boot and by
`laserbridge apply` afterwards. Applying it touches both the usbcore module
parameter, which only governs devices probed from that point on, and
`/sys/bus/usb/devices/*/power/{control,autosuspend_delay_ms}`, which is where
the already-probed serial adapter and camera keep their own policy.
`system.usb_autosuspend` chooses the delay in seconds; `-1` disables it.

Failing to write those attributes does not fail `Apply` — an unprivileged
backend, a read-only `/sys`, or a kernel without `CONFIG_PM` are absences
rather than misconfigurations — so `/api/status` reports the value usbcore
answers with alongside the configured one, and the System page shows both.

## Reading the running system

Everything the appliance observes about its own machine — `/proc` for load,
memory and sockets, `/sys` for USB power management, the wireless interface and
the camera's name, `/dev` for the devices themselves — is resolved against a
single `Root` field on `api.Server` and `runtime.Manager`. Empty means `/`,
which is the appliance; tests point it at a directory they built. Device paths
are looked up under the root but reported under their real names, since that is
what goes into the configuration and what the bridge opens.

One field rather than one per subsystem, because the alternative had already
started to grow: the first test to write USB power settings wrote them to the
machine running the test, and only failed because a container mounts `/sys`
read-only. `DataDir` stays separate — that is the appliance's own state, and it
is configured independently.

## Device discovery

Discovery prefers `/dev/serial/by-id/*` and `/dev/v4l/by-id/*`, then falls back
to `/dev/ttyUSB*`, `/dev/ttyACM*`, and `/dev/video*`. Stable links are returned
first by the API. Camera capabilities come from `v4l2-ctl --list-formats-ext`.
Commands receive validated paths as individual arguments; no shell is used.

The Wi-Fi scan endpoint discovers the wireless interface through sysfs and
invokes `iw` with fixed arguments. It returns de-duplicated SSIDs in descending
signal order with frequency and advertised security. It does not intentionally
stop the setup AP; adapters that cannot scan concurrently return an actionable
error and the UI retains manual SSID entry.

## First boot and networking

Before setup is complete, `laserbridge-network` detects the first wireless
interface and starts hostapd and dnsmasq as `LaserBridge-XXXXXX`. The browser
wizard at `http://10.42.0.1` provisions Wi-Fi, hostname, and the SSH key. The
backend then generates a WPA supplicant configuration in RAM and restarts the
network service in client mode. No PSK is returned by an API.

Wired DHCP runs independently and remains usable during setup and after a
Wi-Fi migration. The system page can change or disable Wi-Fi later; leaving
the password blank retains it only when the SSID is unchanged. Static
addressing remains represented in the model but is deferred until a safe
network migration workflow exists.

## Failure recovery

OpenRC `supervise-daemon` restarts the web, ser2net, and camera processes with
bounded respawn delays. `BridgeWatch` in the web backend restarts a GRBL bridge
that is running but no longer answering, and a hardware watchdog
(`laserbridge watchdog`, ADR 0017) resets the board when the kernel stops
scheduling userspace at all — the one failure no software layer can act on.
`kernel.panic_on_oops` and `kernel.panic` get most lockups as far as a reboot
before the watchdog has to.

Logs go to `/data/log/messages`, rotated by BusyBox syslogd at 200 KiB with two
generations kept, with `klogd` feeding the kernel's own messages into the same
file. `/api/logs` reads that file and falls back to `logread` where it does not
exist. The point is that an OOM kill, an oops or a USB reset is still readable
after the reboot that followed it; a tmpfs ring buffer is not.

## Updates

The build emits a local `.lbu` ZIP containing a manifest, SquashFS root,
kernel, and initramfs. It is not accepted until its detached OpenSSH signature
has been verified against `/data/ssh/authorized_keys`. Manifest file names,
architecture, sizes, SHA-256 digests, and SquashFS identity are validated
before the inactive slot is touched.

The backend streams uploads onto `/data`, writes and syncs the inactive root,
atomically replaces only that slot's boot payloads, and commits
`active-slot.cfg` last. Until that final write the existing slot remains the
boot default. `/data` is outside both slots, so settings and device keys
survive. Rollback explicitly selects the other slot through the WebUI.

A slot that never reaches userspace is rolled back by GRUB itself after three
unconfirmed attempts. The recovered slot then proves itself, so
`laserbridge-boot-confirm` writes it into `active-slot.cfg` and discards the
staged update that failed instead of asking for another reboot into it. An
operator can still mount the FAT32 `LBBOOT` partition on another system and
change `boot/active-slot.cfg` by hand. Automatic release discovery remains
future work.
