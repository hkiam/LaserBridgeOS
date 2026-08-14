# ADR 0001: Immutable SquashFS root with a persistent data partition

- Status: accepted
- Date: 2026-08-13

## Context

The appliance must tolerate hard power loss, boot quickly, and be distributed
as a predictable disk image. Alpine diskless mode normally persists selected
changes through `lbu commit` and an APK overlay. That model is excellent for
operator-managed Alpine installations, but makes appliance state depend on an
explicit commit operation and mixes system customization with user state.

## Decision

The initial layout used three GPT partitions:

1. `LBBOOT`: a small FAT32 EFI system partition containing GRUB, kernel, and
   initramfs;
2. `LBROOT`: a SquashFS image mounted read-only as `/`;
3. `LBDATA`: ext4 mounted read-write at `/data` for configuration and keys.

[ADR 0003](0003-wifi-scan-and-ab-updates.md) extends this into four partitions
by splitting `LBROOT` into `LBROOTA` and `LBROOTB`. The immutability and `/data`
boundaries decided here remain unchanged.

All mutable runtime state lives on tmpfs. Service-specific files are generated
under `/run/laserbridge`. The canonical user configuration is
`/data/config.yaml`; `/etc/laserbridge/config.yaml` is a symlink to it.

## Consequences

- Sudden power loss cannot corrupt the operating-system filesystem.
- Every system file is traceable to the image build.
- User state has an explicit, small backup and migration boundary.
- Root changes require building and flashing a new image.
- ext4 on `/data` can still need journal recovery, but its small scope and
  atomic config writes limit exposure.
- A/B image updates fit the model without importing Alpine `lbu` commit
  semantics. LaserBridgeOS update bundles use the `.lbu` suffix but are signed
  ZIP archives, not Alpine Local Backup Utility archives.

## Rejected alternatives

- Alpine diskless plus `lbu`: requires commit semantics and permits drift.
- Writable ext4 root: simpler initially but weakens power-loss behavior and
  reproducibility.
- OverlayFS with persistent upper layer: preserves writes but allows arbitrary
  system drift and complicates updates.
