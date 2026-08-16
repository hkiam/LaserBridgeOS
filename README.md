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

The image and checksum are written to `dist/`. It is 802 MiB raw and 303 MiB
compressed; the section on [image size](#what-fills-the-image) explains what
that consists of and why the raw figure is mostly empty partition.

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
shared default private key. `ssh-copy-id` works the way it does everywhere
else — sshd reads both the appliance's own list under `/data/ssh` and the
conventional `~/.ssh/authorized_keys`. Only the first is consulted when an
update signature is verified. On a network you do not fully trust, install a key
and switch **SSH password authentication** off on the System page. Root SSH
login is disabled and empty passwords are rejected regardless. See ADR 0006.

### Diagnosing the appliance

Once logged in, `doas` gives root without a further password, and the kernel
log is readable:

```sh
ssh laserbridge@laserbridge.local
dmesg | less                     # kernel log, no privileges needed
doas rc-status                   # what OpenRC thinks is running
doas cat /run/laserbridge/*.conf # the generated service configuration
doas lsblk; doas blkid           # disks and filesystems
```

The appliance has no console, so this is the only way to see why something
misbehaves — and the interfaces do not describe every failure. The trade is
explicit: whoever can log in as `laserbridge` is root (ADR 0008). The web
interface has no login at all, so on a local network this changes less than
it sounds; on a network you do not control, use a key and turn password
authentication off.

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
proves the uploader knew it — and sends it across the network in clear text,
where it is also the SSH login and, through `doas`, root. Signatures are the
better path wherever a second machine can reach this one. See ADR 0007.

The appliance verifies the OpenSSH signature and every payload checksum,
writes only the inactive root slot, and changes the next boot slot last. The
current configuration and SSH keys under `/data` are retained. Reboot after a
successful installation. The WebUI can stage the previous slot for the next
boot. There is deliberately no interactive boot menu or boot delay. Offline
recovery can select a slot by editing `boot/active-slot.cfg` on `LBBOOT`.

### Rolling back to an older slot

`/data` is shared by both slots, so a rollback hands an older binary a
configuration file a newer one wrote. That file is read past rather than
refused: an unknown section, an unknown key, or a value this version cannot
make sense of is skipped and reported, and everything else is used. What was
skipped is named in the service log and on the System page, and the file is
left as it is — so booting the newer slot again finds its settings where it
left them. Saving anything from the older version's web interface does drop
them, because it can only write what it knows.

A configuration that still cannot be used is moved to `config.yaml.broken`,
and what replaces it keeps the hostname, the Wi-Fi credentials and the network
settings where those are usable on their own. Everything else returns to its
default. The point is that the appliance stays where it can be found: losing
the camera resolution is repairable through the web interface, and losing the
network is not. See ADR 0016.

## The GRBL bridge

The serial connection is served by one of two backends, chosen in the
configuration:

```yaml
grbl:
  backend: ser2net       # or: laserbridged
```

`ser2net` is the default and has carried this job from the start.
`laserbridged` is the appliance's own bridge. It carries bytes exactly as
ser2net does — LightBurn connects the same way and every byte, including the
control characters `0x18`, `!`, `~` and `?`, crosses unchanged — and it also
understands what it is carrying.

Both services are installed and each refuses to start unless the
configuration names it, so exactly one ever owns the serial port. Switching is
a configuration change; changing it on the System page stops one and starts
the other.

**ser2net stays.** Not as a transitional measure while the new bridge proves
itself, but permanently: it is the fallback for the day laserbridged does
something unexpected in the middle of a job. It is a well-worn program that
does one thing, and a bridge that understands GRBL has more ways to be wrong
than one that does not. Whatever laserbridged learns to do, `backend:
ser2net` remains one line and one restart away — including over SSH, when the
web interface is the thing that is not working:

```console
$ ssh laserbridge@laserbridge.local
$ doas sed -i 's/backend: laserbridged/backend: ser2net/' /data/config.yaml
$ doas rc-service laserbridged stop && doas rc-service ser2net start
```

Falling back costs something, and the GRBL page says so plainly rather than
just hiding the machine panel: with ser2net selected, the machine's state and
laser output are not read, a beam left on while nothing moves is not noticed,
a client that disappears mid-job gets no feed hold and no soft reset, and
nothing is recorded about alarms or the line that caused them. An absence on a
page is not a statement, and somebody who switched backends a month ago has
nothing else left to remind them.

### What the machine is doing

`laserbridged` reads along with the controller's replies and keeps the
result:

```console
$ laserbridge grbl-status
{
  "state": "CLIENT_CONNECTED",
  "device": "/dev/serial/by-id/usb-1a86_USB_Serial-if00-port0",
  "tcp_port": 23,
  "client": "192.168.178.42:53122",
  "rx_bytes": 819233,
  "tx_bytes": 55342,
  "machine": {
    "state": "Run",
    "position": {"x": 112.4, "y": 68.02, "z": 0},
    "has_position": true,
    "feed": 900,
    "spindle": 255,
    "last_report_unix": 1786579200,
    "resets": 0
  },
  "controller_silent": false
}
```

That comes over a Unix socket at `/run/laserbridge/laserbridged.sock`, and it
is also where the web backend asks — `GET /api/grbl` returns the same reading,
or `available: false` when ser2net is in charge and there is nobody to ask.
Nothing but the daemon opens the serial port.

Reading along happens on a copy of what has already gone to the client, so a
misparsed line can produce a wrong number on the status page but never a wrong
byte at the controller.

`controller_silent` is set when the controller stops answering. Since the
appliance asks for a status report whenever nobody else has for a second, that
is a real absence rather than nobody having asked - so it also catches a
controller that never answers at all: wrong device, wrong baud rate, dead
board. It is reported, never acted on.

`laser_mode` is GRBL's `$32`, picked up from the settings dump LightBurn
requests when it connects. The appliance does not ask for it itself: `$$` is a
queued command and GRBL answers those with `ok`, and an `ok` the client did not
earn desynchronises its flow control - which does not fail a job, it corrupts
one.

### When the client disappears mid-job

A laptop that closes its lid leaves the controller working through its planner
buffer with nobody watching. `grbl.on_disconnect` decides what happens:

| Value | Effect |
| --- | --- |
| `none` | Nothing. The machine finishes its buffer. |
| `hold` | A feed hold, then a check that the beam actually went off — and an escalation if it did not. Keeps the position. |
| `reset` (default) | A feed hold, then — once the machine reports it has stopped — a soft reset. The job ends, and the output is off whatever the controller is configured like. |

`reset` waits because a soft reset while the axes are still turning loses the
position. The bridge sends `?` until GRBL reports a state that means standing
still, and gives up after three seconds rather than wait forever. Note that
`Hold` is one of those states and `Idle` is not required: a held machine has
stopped, it simply still has a job in its buffer.

**Why `reset` is the default, and why a feed hold alone is not enough.**
Whether `!` switches the beam off depends on GRBL setting `$32`. With laser
mode on it does. With laser mode off the output is a spindle — and a feed hold
deliberately leaves a spindle running, because a router bit stopping in the cut
is its own kind of damage. `M3` constant power plus `$32=0` means the axes stop
and the beam keeps burning where it stands. A soft reset has no such
dependency: it switches the output off unconditionally. And the job it ends
was over anyway, because LightBurn streams — when the connection dies there is
nothing left to resume into.

A machine that is already standing still skips the two lower rungs: GRBL
ignores a feed hold from Idle and the spindle-stop override outside a hold, so
for a laser burning where it stands only the soft reset does anything at all.

Choosing `hold` gets the reversible version, with the check that makes it
honest. GRBL fills the accessory field of its status report from the actual
output, so `A:S` is the controller saying the beam is on, and an `Ov:` field
without an accessory beside it is the controller saying nothing is on. If the
beam is still on after the hold, the bridge sends the spindle-stop override
(`0x9E`, which leaves the job resumable) and then a soft reset if that was not
enough. If the controller never says, and `$32=0` is known, it resets — that
is not an open question. Otherwise it records that it could not confirm.

### The laser may not be on while nothing moves

The disconnect handler reacts to a symptom. The hazard is the laser being on
with the machine standing still, and that has causes a disconnect cannot see —
LightBurn hanging with its connection intact, for one. So it is watched
directly:

| Condition | Grace |
| --- | --- |
| Laser on, nobody connected | 1 second |
| Laser on, machine has not moved | `grbl.stationary_beam_seconds`, 20 by default, 0 to switch off |

The second one has to be generous: piercing thick material is a stationary burn
on purpose. Set it above your longest pierce. Both end in the same ladder, and
both stay out of it entirely when `on_disconnect` is `none`.

The check needs a status report that is not stale, so the appliance asks for
one when nobody else has for a second. During a job LightBurn polls several
times a second and the appliance stays quiet; when the conversation stops, it
takes over the asking.

**An intervention ends the connection.** If a client is still streaming when the
bridge takes the machine over, it is told — `[MSG:LaserBridgeOS took over: …]`,
which LightBurn prints in its console — and then dropped. This is not a
courtesy. A soft reset empties GRBL's planner and its 128-byte receive buffer,
and a sender that knows nothing about it keeps counting `ok`s against a buffer
that no longer holds what it thinks: what follows is a stream of `error:9`, or
G-code executed while the two ends disagree about where the job is. A controller
that has been reset is not going to finish the job anyway. See ADR 0018.

**What the controller says about its output has a date on it.** GRBL mentions
its outputs only alongside `Ov:`, roughly every tenth report, and reports what
the output was at that instant — which in laser mode with `M4` is genuinely off
during rapids and between moves. So the GRBL page shows the reading with its
age, and says "not conclusive" rather than "off" when a machine is cutting at a
non-zero power. For the same reason the stationary check declines to act when
the controller has said nothing about its outputs within the grace period, and
records that it declined.

An idle machine is left alone; a hold would only leave the next client
something to clear. A service restart does not count as a client walking away,
so changing settings during a job does not pause it. Whatever the bridge did
is in `last_intervention` and on the GRBL Bridge page, so a paused machine
does not need guessing about.

> **This is not an emergency stop.** It needs the appliance running, the cable
> seated and the controller answering, and it assumes a feed hold makes the
> situation safe. A hardware emergency stop needs none of that. Keep using the
> machine's own.

Why it is worth having anyway: with `M3` constant laser power, a stream that
stops leaves the beam on at its last setting while the machine stands still.
That is a hole in the workpiece at best. A feed hold in laser mode switches
the beam off, which is precisely what nobody was there to do. ser2net cannot,
because it never knew the client had gone. Check `$32` is 1 on your controller
and prefer `M4` in LightBurn regardless — this setting is the second line of
defence, not the first.

### What happened

A job that fails at three in the morning leaves a stopped machine and no
explanation. The bridge keeps one:

```console
$ laserbridge grbl-journal
[
  {
    "unix": 1786752191,
    "kind": "error",
    "text": "error:9",
    "code": 9,
    "state": "Idle",
    "position": {"x": 112.4, "y": 68.02, "z": 0},
    "line": "G1 X118 F900",
    "context": ["G0 X112.4 Y68.02", "M3 S255", "G1 X118 F900"]
  },
  {"unix": 1786752206, "kind": "intervention", "text": "feed hold after the client disconnected while Run"}
]
```

The `line` is the point. GRBL answers `error:9` and names no line number — but
it answers in order, exactly one `ok` or `error:` per line it accepted, so the
line a reply refers to is the oldest one not yet answered for. The bridge
counts them and names it. When it cannot be sure — a client attached to a
controller that was already working, so the count never lined up — it says
nothing rather than guessing, and `context` still shows the neighbourhood.
Alarms answer for no line, so they carry the position instead.

Two hundred events are kept in memory and the notable ones survive a reboot in
`/data/laserbridge/bridge-journal.log`, rotated at 256 KB. The file is read back
into memory when the bridge starts, so an incident is still on the page after
the restart it caused — which used to be exactly when it disappeared from view.
Status reports are not recorded; there are several a second during a job and
they would bury the three lines that matter. Routine client connects and
disconnects stay in memory only, for the same reason; when the bridge drops a
client itself, that is recorded as an intervention, because it is one.
`GET /api/grbl/journal` and the GRBL Bridge page show the same thing.

### What counts as a job

The appliance keeps a notion of the work in front of the machine, separate from
what the machine is doing this instant. A job begins when the machine starts
moving and ends when it has stood still for fifteen seconds, when the client
leaves, or when the bridge stops it — and it says which. It carries the lines
and bytes the client sent, where the machine started and where it was last
seen, and how long it ran, measured on the uptime clock so a wrong wall clock
cannot distort it.

This exists because "is the machine moving right now" is false during a pierce,
during a pause and while somebody changes the material, so the guards below
were open at exactly the moments they were written for. They now ask whether a
job is running.

### Not interrupting a job

Saving GRBL settings restarts the bridge, installing an update rewrites a root
slot, and rebooting is obvious. All three now refuse while the machine is
moving:

```console
$ curl -X POST http://laserbridge.local/api/system/reboot
{"error":"not rebooting while the machine is cutting at X 112.4 Y 68.02. Repeat with force=true if that is what you want.","busy":true}
```

Add `?force=true` to go ahead anyway. When the appliance cannot tell — ser2net
in charge, or the daemon not answering — it allows: an appliance that refused
to reboot because it was unsure would be worse than one that never asked.

### Watching without interfering

`grbl.monitor_port` opens a second TCP port that shows the traffic and accepts
none of it. Whatever a watcher sends is read and thrown away, so watching a
job cannot become part of it.

```console
$ nc laserbridge.local 2300
# LaserBridgeOS monitor: > is towards the controller, < is from it. Input is ignored.
> ?
< <Run|MPos:112.400,68.020,0.000|FS:900,255>
> G1 X118 F900
< ok
```

It is off unless a port is set. Until now, watching what LightBurn and the
controller said to each other meant taking the port away from LightBurn, which
changes the situation you were investigating.

### Watching the bridge itself

The bridge supervises the machine, which makes its own hanging the failure that
matters most - and the one thing it cannot notice about itself. OpenRC restarts
a process that exits, not one that deadlocks, and a wedged daemon keeps its
listening socket because the kernel accepts on its behalf.

So the web backend asks it, every fifteen seconds, whether it still knows what
it is doing. Three unanswered questions and it restarts it; not more than once
in five minutes, because a restart costs the client its connection. A bridge
that is not answering is reported as not running, rather than as running.

That is only worth doing because a restart no longer resets the controller.

The guarantee is narrower than the mechanism suggests, and worth stating: a
bridge that **dies** is respawned by OpenRC. A bridge that **hangs** is
restarted, if OpenRC can stop it — a process frozen at the kernel level cannot
be, and then the appliance can only report it. What it will not do is leave the
bridge stopped: a restart that ends that way is followed by a start, because
the configuration says which backend should be running and that is the
authority.

### The wrong baud rate

The most common setup mistake, and unmistakable from here: bytes arrive and
none of them are GRBL. The status page says so instead of showing an empty
machine panel and leaving you to wonder.

Two details worth knowing. It holds the port open for its whole life rather
than opening it per client, because opening a USB adapter toggles DTR and
resets an Arduino-based GRBL controller — reconnecting LightBurn should not
reset the machine. And it prefers the stable `/dev/serial/by-id/` name over
`/dev/ttyUSB0`, which can point at different hardware after a reboot.

An adapter that is unplugged is noticed: the client is dropped rather than
left writing G-code into a void, and the port is reopened when the adapter
comes back — no restart needed.

Two details that only matter when something is going wrong. The port is opened
with `HUPCL` cleared, so DTR stays asserted when the daemon exits and
restarting it does not reset an Arduino-based controller — a restart during a
job costs nothing. And writes towards the client carry a five-second deadline:
a laptop on a stalled Wi-Fi link is still connected as far as the kernel is
concerned, and without the deadline the bridge would wait on it forever while
the controller's output piled up unread.

The decisions behind all of this are in ADR 0009 (why our own bridge),
ADR 0010 (what it may do on its own) and ADR 0012 (evidence, restraint and a
window). There will be no hardware interlock: a
relay in the laser-enable line is the only thing that would remove the "it
depends on this daemon" caveat, and this appliance will not get one. The
caveat is permanent.

`tests/hardware/grbl-smoke-test.sh <host>` checks the whole path against a
real controller; everything else is covered by tests against a
pseudo-terminal.

`tests/hardware/appliance-smoke-test.sh <host>` covers the appliance around it
— the watchdog, the panic settings, logs that survived a reboot, the USB power
policy as the kernel actually applied it, and a configuration written by a
newer version being readable by this one. Those exist only in a built and
booted image, so no unit test can reach them. It reads and asks; the one check
that stops a service has to be requested with `--stop-watchdog`.

### When the appliance itself stops

Each recovery mechanism here covers something specific, and all of them need
the kernel to still be running: a bridge that exits is respawned, a bridge that
hangs is restarted by the web backend, a slot that never boots is replaced by
the other one after three attempts.

A kernel that has stopped scheduling userspace is covered by a hardware
watchdog. `laserbridge watchdog` holds `/dev/watchdog` open and writes to it
every ten seconds; when the writes stop, the board resets. Coming back through
the reboot re-enumerates USB, which toggles DTR, which soft-resets an
Arduino-based controller — and a soft reset switches the output off whatever
`$32` says. The kernel is also told to treat an oops as fatal and reboot ten
seconds later (`kernel.panic_on_oops`, `kernel.panic`).

The watchdog does not judge the system's health. It pets while it is being
scheduled and asks nothing else, because a false positive here costs a reboot
in the middle of a cut. Stopping the service writes the magic close character
first, so a deliberate stop is not a delayed reset.

Boards without a watchdog device — most virtual machines — boot with that one
service failed and a message saying so. That is deliberate: an appliance
without a last resort should not look like one that has it. See ADR 0017.

### Reading what happened afterwards

System logs are written to `/data/log/messages`, rotated at 200 KiB with two
generations kept, and kernel messages are fed into the same file by `klogd`.
They used to live in a RAM ring buffer, which meant they were gone at exactly
the moment somebody wanted them: after the reboot. An out-of-memory kill, a
kernel oops, a controller that reset the USB bus — on a headless machine those
leave no other trace.

```console
$ ssh laserbridge@laserbridge.local
$ tail -f /data/log/messages
$ grep -i 'usb\|oops\|killed process' /data/log/messages*
```

The Logs page shows the same file. The GRBL journal is separate and stays
separate: it records what the bridge saw of the machine, this records what the
appliance carrying it was doing.

### USB power saving is off

The kernel may suspend a USB device that has been idle for a while and wake it
on the next access. On a laptop that is worth doing; on a machine whose USB
port is carrying G-code it is not. A CH340 or FTDI adapter coming out of
suspend can swallow the first characters of the line that woke it, and GRBL
parses lines — a dropped byte is a wrong cut, not a failed job. Some adapters
do worse and re-enumerate, which moves `/dev/ttyUSB0` out from under the bridge
mid-job. The power it saves on an appliance bolted to a laser and plugged into
the wall is not worth measuring.

So the appliance switches it off in three places, because no one of them is
enough:

- `usbcore.autosuspend=-1` on the kernel command line, for devices probed
  during boot — long before `/data` is mounted and the configuration is
  readable;
- `options usbcore autosuspend=-1` in `/etc/modprobe.d`, carried into the
  initramfs as well. This one exists because the command line above cannot be
  changed by an update — it is baked into the GRUB image on the ESP — while
  the root filesystem is replaced by every update;
- `system.usb_autosuspend`, applied by `laserbridge apply` at every boot and
  after every configuration change. It sets the same kernel parameter for
  whatever is plugged in next, and walks `/sys/bus/usb/devices/*/power/` to
  reach the adapter and camera that were probed before anyone had a say. This
  is the only one of the three that can express a value other than "off".

```yaml
system:
  usb_autosuspend: -1    # -1 is off; 0 to 3600 is an idle delay in seconds
```

The System page carries the same setting and shows what the kernel answers
beside what was asked of it. The two differing means the setting did not take —
a backend running without root cannot write `sysfs` at all — and that is worth
seeing on the page that offers the setting.

Turning it back on is a real option for a device whose driver depends on
runtime power management, or for measuring what this appliance draws. Doing so
while a job is running is refused unless the request repeats with `force=true`:
the setting reaches the adapter that is carrying the job.

Note for existing installations: the kernel command line lives in the GRUB
image on the ESP, and an update bundle carries only the root filesystem, kernel
and initramfs. An appliance updated in place therefore keeps whatever command
line it was installed with — which is why the same default is also a module
option and a configuration setting, both of which updates do replace. The rule
this follows: boot policy that has to be changeable belongs where updates can
reach it, as a sysctl where it can be one, and on the command line only as a
default for freshly installed images.

## What fills the image

An appliance this narrow producing an 800 MiB image looks wrong until you
break it down. Most of it is partition, not content.

| Partition | Size | Holds | Free |
| --- | --- | --- | --- |
| ESP | 96 MiB | two kernels and two initramfs, 60 MiB | 36 MiB |
| root A | 192 MiB | the SquashFS, 124 MiB | 68 MiB |
| root B | 192 MiB | the same, for the other slot | 68 MiB |
| data | 320 MiB | empty at build; must fit a 150 MiB update bundle during an upload | — |

The A/B design pays for its safety by storing the system twice. What is
actually in those 124 MiB is Linux, not this project:

| | Uncompressed |
| --- | --- |
| kernel modules | 125 MiB |
| firmware blobs | 42 MiB |
| userland: busybox, OpenSSH, ser2net, dnsmasq, hostapd, ustreamer, the appliance binary | 36 MiB |

Two things were changed after measuring this. Alpine ships kernel modules
individually gzipped, which inside an xz SquashFS means compressing the same
bytes twice, badly: unpacking them first and letting `mksquashfs` do the work
took the SquashFS from 161 MiB to 124 and the compressed image from 384 MiB
to 303. And the partitions were cut to fit what goes in them, which took the
raw image from 962 MiB to 802 — worth more than it sounds, because a
permanent install writes every one of those bytes across the network.

Going meaningfully below this means giving up hardware support: dropping
`linux-firmware-intel` alone would save 25 MiB but leaves Intel Wi-Fi
adapters without firmware, and pruning kernel modules trades size against
running on hardware you have not tried yet. That is a decision about which
machines the image must boot, not a build tweak.

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
| Streaming write | 962 MiB at 13 MB/s onto a blank disk (QEMU) |
| Read-back verification | checksum matched `SHA256SUMS` exactly |
| The written disk boots | comes up in first-boot setup with its own AP SSID |
| Privileged half on the appliance | `doas` rule exercised on real hardware |
| **Permanent install** | **Z83F eMMC: 1 008 730 112 bytes written in 31 min 46 s (517 KB/s over the setup AP), read back, checksum matched, rebooted** |

The install on real hardware ran as `--resume` against a recovery system that
was already up. Two earlier attempts over the same access point aborted
mid-transfer — both times because the client machine roamed back to a known
network, and both times before the reboot, exactly as intended.

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

A deployment has two phases, and **the appliance changes address between
them**. Treat that as the normal course of events rather than a fault.

**Phase one — into RAM.**

```sh
./deploy.sh --dry-run --install ./dist   # prints the plan, changes nothing
./deploy.sh --install ./dist
```

The second command prints the target, the disk, its size and the image
version, and requires the disk path to be typed back before it proceeds.
`--yes` skips that prompt and should be reserved for unattended use — it
removes the last thing standing between a typo and a wiped disk. A context
with no terminal, such as an editor or an agent, cannot answer the prompt and
is told to use `--yes` deliberately.

**Between the phases — find the appliance again.** Once the kexec has
happened, the RAM system asks DHCP under the name `laserbridge` rather than
whatever the installed system was called, so the router usually hands it a
**different lease**. If it has no credentials for any network — the normal
case for a device without a cable — it raises its own access point at
`10.42.0.1` instead. `laserbridge.local` may answer late, or answer with a
stale address from a previous session.

`deploy.sh` tries the configured address, `laserbridge.local` and `10.42.0.1`
in turn. When none of them is right, look the address up in the router or
join `LaserBridge-XXXXXX`, and name it explicitly:

```sh
./deploy.sh --resume --install --recovery-host laserbridge@10.42.0.1 ./dist
```

**Phase two — onto the disk.** `--resume` skips the kexec and writes to the
RAM system that is already running. It is not a recovery path bolted on after
the fact; on a device that has to move address it is the ordinary second half
of the procedure.

Expect several minutes: roughly 180 MiB of initramfs before the kexec, then
the image compressed (about 384 MiB rather than 962), then a full read-back
for verification. Over the appliance's own access point that is far slower —
one measured install took 31 minutes for the image alone.

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

SSH now leads to root: the `laserbridge` account may use `doas` without a
password, so that a headless appliance can actually be diagnosed (ADR 0008).
Whoever holds the SSH credential holds the machine.

The WebUI has no login. Anyone who can reach port 80 can reconfigure the
appliance, restart services, or reboot it. Two operations are exceptions
because they can replace the running system: installing an update and
changing the appliance password both require that password, and the shipped
default is not accepted for either (ADR 0007).

That password is worth being precise about, because it is not only a web
credential. It is the SSH login, and SSH leads to root through `doas`, so it
is the whole machine — and it crosses the network in clear text over HTTP,
like everything else this interface handles. Two consequences follow:

- **Prefer the signature path for updates.** It proves the bundle came from
  the key holder, it never puts a root credential on the wire, and it works on
  an appliance whose password you would rather not type on a shared network.
  The password path exists for appliances set up without a key at all.
- **Guessing is now rate limited rather than merely slowed.** Five wrong
  passwords are free — the operator with two passwords in their head is not
  the threat — and after that the wait doubles from fifteen seconds to a
  five-minute cap. Both endpoints share one counter, because they check the
  same secret. The previous one-second sleep was not a limit: it did nothing
  against attempts made in parallel.

A challenge-response scheme would remove the clear-text exposure, and it does
not fit here: the appliance stores a `crypt(3)` SHA-512 hash, and a browser
cannot compute one — WebCrypto has no such primitive. Doing it properly would
mean a second, browser-computable verifier kept in sync with the account
password, including when it is changed over SSH. Signatures already solve the
same problem without a new secret to keep in step.

LaserBridgeOS is intended for a trusted local network. Do not forward the WebUI,
SSH, GRBL port 23, or camera port 8080 from an Internet-facing router. The MVP
does not terminate HTTPS, so management traffic should not cross an untrusted
network.

## Troubleshooting

### The setup hotspot appears but gives out no IP address

Fixed in images built after August 2026. `dnsmasq` kept its lease file under
`/var/lib/misc`, which is on the read-only SquashFS root, so it exited with
`cannot open or create lease file: Read-only file system` and the access point
associated clients without ever answering their DHCP requests. The lease file
now lives under `/run`.

On an affected image, configure the address by hand: `10.42.0.60/24` with
gateway and DNS `10.42.0.1`, then open `http://10.42.0.1`.

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
