# ADR 0002: First-boot access point and per-device SSH key

- Status: accepted
- Date: 2026-08-13

## Context

An appliance may be installed where wired Ethernet is unavailable. Shipping a
fixed SSH private key would make every installed device impersonable, while
requiring users to edit the data partition before first boot makes onboarding
unnecessarily difficult.

## Decision

An unconfigured appliance starts `laserbridge-network` in setup-AP mode. It
uses a unique `LaserBridge-XXXXXX` SSID, address `10.42.0.1/24`, local DHCP,
and captive-portal DNS. The temporary WPA2 password is
`laserbridge-setup`. Ethernet DHCP remains active as a recovery path.

On first boot the initializer also creates:

- a unique Ed25519 SSH host key;
- a unique Ed25519 client key for the `laserbridge` account;
- `/data/ssh/authorized_keys` containing that client's public key.

The setup UI requires either downloading the generated private key or entering
an existing public key. It validates hostname, country, SSID, PSK, and public
key before atomically persisting the configuration. Completion removes the
generated private key from the appliance and switches Wi-Fi to WPA supplicant
client mode. The public key remains authorized and root login stays disabled.

The setup API exposes the generated private key only while
`system.setup_complete` is false, requires same-origin CSRF protection, sets
`Cache-Control: no-store`, and never exposes the Wi-Fi PSK through the general
configuration API.

## Consequences

There is no fleet-wide SSH key or password. Setup works from a phone or laptop
without prior network access. The well-known temporary AP password is a
deliberate usability tradeoff: first boot must be performed in a physically
trusted location and completed promptly. Deployments requiring stronger
onboarding can pre-populate `/data/config.yaml` and
`/data/ssh/authorized_keys` before first boot.

Wi-Fi hardware and drivers must support AP mode. If they do not, the same
wizard remains available over wired Ethernet at `laserbridge.local`.
