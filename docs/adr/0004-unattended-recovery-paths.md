# ADR 0004: Unattended recovery paths

- Status: accepted
- Date: 2026-08-14

## Context

LaserBridgeOS is headless by design (ADR 0001, ADR 0002): no getty is spawned,
root has no password, and the only ways in are the WebUI and SSH over the
network. That decision is sound while the appliance is reachable, but it means
every fault that costs reachability is terminal. Three such faults existed:

- A system slot that boots into a panic or never starts its services. GRUB
  followed `active-slot.cfg` unconditionally, so the rollback endpoint sat
  behind a WebUI that could no longer run.
- Wi-Fi credentials that never associate - a typo in the passphrase, a
  renamed network, a replaced router. The appliance had already left setup-AP
  mode permanently and, without a cable, had no other link.
- A `/data/config.yaml` that no longer parses or validates. Every service
  depends on `laserbridge-init`, which failed on an unreadable configuration
  and took the WebUI and SSH down with it. A stricter validation rule in a
  later release was enough to trigger this.

Each fault turned a recoverable mistake into a device that has to be opened
and reflashed.

## Decision

**Boot attempts are counted.** GRUB raises `laserbridge_try` in
`boot/grubenv` before loading a kernel. The `laserbridge-boot-confirm` service
resets it once the default runlevel is up. Three unconfirmed attempts select
the other slot and record that choice in `laserbridge_override`, which keeps
applying until userspace confirms it. On a confirmed fallback boot the running
slot is written into `active-slot.cfg` and the staged update that failed to
boot is discarded, so the appliance settles on the slot that works instead of
alternating. GRUB script has no arithmetic, so the counter advances through
explicit cases; the environment block is a fixed 1024-byte file that both GRUB
and the appliance rewrite in place.

**Wi-Fi client mode waits and falls back.** `laserbridge-network` polls
`wpa_cli` for `wpa_state=COMPLETED` for up to 45 seconds before starting DHCP,
instead of firing a one-second DHCP attempt at an interface that has not
associated yet. If the association never completes and no Ethernet carrier is
present, the setup access point is reopened so the network can be corrected
from a phone. With a cable connected, Wi-Fi is simply left down.

**An unusable configuration is quarantined, not fatal.** `config.Store.Ensure`
moves a file that fails to parse or validate to `config.yaml.broken`, restores
the defaults, and reports the path it used so the failure is logged rather
than silent.

`scripts/check-boot-policy.sh` asserts all three mechanisms are still wired
up, because each is invisible during normal operation and easy to drop.

## Consequences

The appliance recovers from a bad update, wrong Wi-Fi credentials, or a
corrupt configuration without physical access. Losing customised settings to a
quarantined configuration is recoverable; losing all access is not, so
availability is preferred over preserving state.

Reopening the setup access point on a configured appliance re-exposes the
well-known WPA2 password from ADR 0002. The exposure is bounded: it only
happens when the appliance has no other link, the initial SSH private key is
already gone once setup is complete, and an appliance that is unreachable is
otherwise scrap. Restricting the fallback to the no-carrier case keeps a
cabled deployment from ever opening it.

Boot confirmation mounts the FAT boot partition once per boot. If that mount
fails, the counter is never reset and GRUB will eventually alternate slots.
Both slots hold the same image until the first update, so the alternation is
harmless and self-correcting: whichever slot confirms becomes the recorded one.
