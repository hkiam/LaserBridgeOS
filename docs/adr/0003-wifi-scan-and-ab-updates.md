# ADR 0003: Non-disruptive Wi-Fi scan and owner-signed A/B updates

- Status: accepted
- Date: 2026-08-14

## Context

First-boot Wi-Fi setup currently requires entering an SSID manually. The
immutable single-root image also cannot replace itself safely: overwriting the
mounted root or the only boot files could brick the appliance after power loss.
LaserBridgeOS has no cloud account or central device identity that could
authorize remote software installation.

## Decision

### Wi-Fi scan

The backend discovers the first `nl80211` wireless interface through sysfs and
runs `iw dev <interface> scan` without a shell. It parses only SSID, signal,
frequency, and advertised security, deduplicates access points by SSID, and
returns the strongest first. The setup AP remains running during the scan. If
the adapter cannot scan while acting as an AP, the UI reports that limitation
and keeps manual SSID entry available; the management connection is never
intentionally interrupted just to scan.

### A/B system update

New images contain `LBROOTA` and `LBROOTB` SquashFS partitions. The EFI system
partition stores a kernel and initramfs for each slot plus a small
`active-slot.cfg`. GRUB reads that file and boots the selected slot directly,
without a menu or timeout. The data partition remains independent and is never
part of an update.

An update bundle is a ZIP file with this fixed content:

```text
manifest.json
root.squashfs
vmlinuz-lts
initramfs-lts
```

The manifest records format version, architecture, release version, sizes, and
SHA-256 digests. The backend applies strict file-name and size limits and
checks every digest before writing anything. It writes the inactive root slot,
syncs it, installs that slot's boot files through temporary names, and changes
`active-slot.cfg` last. A loss of power before the final switch leaves the old
slot selected. Offline recovery can select either slot by editing that file on
the FAT32 `LBBOOT` partition.

The update bundle must have an OpenSSH detached signature created with:

```sh
ssh-keygen -Y sign -f laserbridge_ed25519 \
  -n laserbridge-update LaserBridgeOS-x86_64-VERSION.lbu
```

The appliance verifies the signature against `/data/ssh/authorized_keys`.
Thus the existing appliance owner authorizes system updates without a shared
vendor secret, external account, or cloud dependency. Checksums protect
against corruption; the SSH signature protects authenticity. Wi-Fi settings,
SSH keys, and other persistent configuration survive slot changes.

## Consequences

The disk image grows because both roots and boot payloads are present. A 2 GB
or larger target disk is required. The MVP update flow is intentionally local:
the user downloads or builds a bundle, signs it, and uploads bundle plus
signature through the WebUI. Automatic release discovery and unattended fleet
rollouts remain out of scope.

There is no automatic boot-success rollback in this iteration. If a validly
signed release boots badly, the previous slot can be selected through the UI
while it remains reachable or by editing `active-slot.cfg` from another
machine. Adding a boot-attempt counter later does not require another
partition-layout change.
