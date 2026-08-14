# Installation

## Flash and boot

1. Decompress `LaserBridgeOS-x86_64.img.gz` if necessary.
2. Flash the image to the complete target disk, not to a partition.
3. Boot the x86_64 machine in UEFI mode.
4. Join the unique `LaserBridge-XXXXXX` Wi-Fi with password
   `laserbridge-setup` and open `http://10.42.0.1`. With Ethernet attached,
   the same setup is available at `http://laserbridge.local`.
5. Scan for the target network or enter its SSID manually, then enter the
   Wi-Fi credentials and hostname.
6. Download the generated SSH key, or paste an existing public key, then
   finish setup. Rejoin the target Wi-Fi and open `http://laserbridge.local`.

The image overwrites the selected target disk. Verify the device name before
using `dd`.

## First SSH login

SSH deliberately has no usable password by default. The first-boot wizard
creates a unique key for this appliance and downloads it once. Protect its
permissions and connect with:

```sh
chmod 600 laserbridge_ed25519
ssh -i laserbridge_ed25519 laserbridge@laserbridge.local
```

Alternatively, paste your existing public key in the wizard. Advanced users
may still pre-provision `ssh/authorized_keys` on the `LBDATA` partition.

The initial user is always `laserbridge`. The root account is locked in the
distributed image, including at the local console. A downstream development
image may set a console password as part of its build policy; network root
login remains prohibited by `sshd`.

## Install or roll back an update

Build or download the `.lbu` for the desired x86_64 release. Sign the exact
file with an SSH key already authorized on the appliance:

```sh
scripts/sign-update.sh LaserBridgeOS-x86_64-VERSION.lbu laserbridge_ed25519
```

Open the System page, select the `.lbu` and its generated `.sig`, and choose
Install. Keep power connected until the UI confirms that the inactive slot is
ready, then reboot. The status card shows the current and staged slots.

If the new release is unsuitable but still reaches the WebUI, choose Roll back
and reboot. There is no GRUB menu or boot delay. If the new slot cannot start
the UI, attach the medium to another system, mount the FAT32 `LBBOOT` partition,
and set `boot/active-slot.cfg` to either `set laserbridge_slot=a` or
`set laserbridge_slot=b`. There is currently no automatic boot-attempt
rollback.

## Configuration and recovery

The UI writes `/data/config.yaml` atomically. A malformed manual edit is
rejected and the last generated runtime files remain in use. To recover, mount
`LBDATA` elsewhere and restore `config.yaml`; deleting it causes defaults to be
created on the next boot.

The root filesystem cannot be modified. Persist custom SSH keys and appliance
settings only under `/data`.
