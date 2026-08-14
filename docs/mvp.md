# MVP specification and tasks

## Acceptance criteria

- [x] Alpine x86_64, OpenRC, BusyBox, no graphical stack
- [x] immutable SquashFS root and persistent `/data`
- [x] SSH with root/password login disabled by default
- [x] Avahi hostname and service advertisements
- [x] raw ser2net bridge with one-client default
- [x] ustreamer MJPEG defaults and YUYV fallback
- [x] central YAML configuration with strict validation
- [x] small Go REST backend and static responsive UI
- [x] dashboard, GRBL settings, camera settings and live preview
- [x] restart and reboot actions using POST
- [x] RAM logging and API log tail
- [x] OpenRC supervision and boot initialization
- [x] containerized `make image` producing raw/gzip/checksum artifacts
- [x] tests for configuration, generation, and mutation security
- [x] CI checks and tagged release artifacts
- [x] non-disruptive Wi-Fi scan with manual SSID fallback
- [x] owner-signed local A/B update and explicit rollback
- [x] unattended recovery: boot-attempt rollback, Wi-Fi fallback to the setup
      AP, and quarantine of an unreadable configuration

## Deferred after MVP

- static-IP transition workflow and WPA-Enterprise/WPA3 provisioning
- local WebUI authentication and multiple users
- hardware watchdog and active health monitor
- automatic release discovery and unattended rollout
- VID/PID policy mapping and camera calibration
- GRBL job streaming or G-code editing
