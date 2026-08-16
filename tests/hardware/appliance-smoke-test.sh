#!/bin/sh
# Smoke test for the appliance itself, on a booted machine.
#
# The GRBL side has its own script; this one covers everything that only exists
# once an image has been built and started: OpenRC ordering, the hardware
# watchdog, logs that have to survive a reboot, the USB power policy as the
# kernel actually applied it, and a configuration written by a newer version
# being readable by this one. None of that can be established by the unit tests
# - they prove the code is right about a directory they built themselves.
#
#   tests/hardware/appliance-smoke-test.sh laserbridge.local
#
# Everything here reads. The one exception has to be asked for by name:
#
#   tests/hardware/appliance-smoke-test.sh laserbridge.local --stop-watchdog
#
# which stops the watchdog service and waits out its timeout to prove that a
# deliberate stop is not a delayed reset. If the kernel was built with
# CONFIG_WATCHDOG_NOWAYOUT, that check reboots the appliance - which is the
# thing worth finding out, but not while anything is cutting.
set -eu

HOST=${1:-laserbridge.local}
STOP_WATCHDOG=no
[ "${2:-}" = "--stop-watchdog" ] && STOP_WATCHDOG=yes

passed=0
failed=0

say() { printf '\n=== %s ===\n' "$1"; }
ok() { printf '  ok    %s\n' "$1"; passed=$((passed + 1)); }
bad() { printf '  FAIL  %s\n' "$1" >&2; failed=$((failed + 1)); }
note() { printf '        %s\n' "$1"; }

on() { ssh -o BatchMode=yes -o ConnectTimeout=5 "laserbridge@$HOST" "$@"; }
api() { curl -fsS -m 5 "http://$HOST/api/$1"; }

command -v ssh >/dev/null || { echo "ssh is required" >&2; exit 1; }
command -v curl >/dev/null || { echo "curl is required" >&2; exit 1; }
on true >/dev/null 2>&1 || { echo "cannot reach laserbridge@$HOST over SSH" >&2; exit 1; }
api status >/dev/null 2>&1 || { echo "the web interface on $HOST did not answer" >&2; exit 1; }

say "Services"
# grep -oE rather than sed: BSD sed, which is what a Mac in a workshop has, does
# not know \| in a basic expression and silently matches nothing at all.
services=$(api status | grep -oE '"[a-z0-9-]+":(true|false)' | tr -d '"' | tr ':' ' ')
for required in laserbridge-web laserbridge-watchdog; do
	case "$(printf '%s\n' "$services" | sed -n "s/^$required //p")" in
		true) ok "$required is running" ;;
		false) bad "$required is not running" ;;
		*) note "$required was not reported" ;;
	esac
done

say "Hardware watchdog"
# A board without the device is a real answer, not a failure of this appliance -
# but it must be visible rather than silently absent, which is the whole point
# of ADR 0017.
if on test -e /dev/watchdog; then
	ok "/dev/watchdog exists"
	timeout=$(on cat /sys/class/watchdog/watchdog0/timeout 2>/dev/null || echo "")
	[ -n "$timeout" ] && note "driver timeout: ${timeout}s"
	# The strongest evidence available without waiting for a reset: something has
	# the device open, and the board has been up longer than the timeout. If
	# nothing were petting an armed watchdog, neither would be true.
	up=$(on cut -d. -f1 /proc/uptime)
	if [ -n "$timeout" ] && [ "$up" -gt "$timeout" ] 2>/dev/null; then
		ok "armed and fed: up ${up}s with a ${timeout}s timeout"
	fi
	if on grep -q 'watchdog armed' /data/log/messages 2>/dev/null; then
		ok "the watchdog reported arming in the log"
	else
		note "no arming line in the log - it may have rotated out"
	fi
else
	note "no /dev/watchdog on this board: a frozen kernel will not be recovered"
	note "the service is expected to be stopped, and to have said so at boot"
fi

if [ "$STOP_WATCHDOG" = yes ] && on test -e /dev/watchdog; then
	say "Stopping the watchdog on purpose"
	note "this is the CONFIG_WATCHDOG_NOWAYOUT check; if it fails the appliance reboots"
	on doas rc-service laserbridge-watchdog stop >/dev/null 2>&1 || true
	uptime_before=$(on cut -d. -f1 /proc/uptime)
	sleep 45
	if uptime_after=$(on cut -d. -f1 /proc/uptime 2>/dev/null) &&
		[ "$uptime_after" -gt "$uptime_before" ]; then
		ok "a deliberate stop did not reset the board (magic close works)"
	else
		bad "the appliance rebooted or went away after the watchdog was stopped"
	fi
	on doas rc-service laserbridge-watchdog start >/dev/null 2>&1 || true
fi

say "Panic behaviour"
for setting in kernel.panic=10 kernel.panic_on_oops=1; do
	name=${setting%=*}
	want=${setting#*=}
	have=$(on sysctl -n "$name" 2>/dev/null || echo "")
	if [ "$have" = "$want" ]; then
		ok "$name is $want"
	else
		bad "$name is '${have:-unreadable}', want $want"
	fi
done

say "Logs that survive a reboot"
if on test -f /data/log/messages; then
	ok "/data/log/messages exists"
	lines=$(on doas wc -l /data/log/messages | awk '{print $1}')
	note "$lines lines"
	# Quoted for the remote shell as well as this one: an unquoted pattern with
	# a space in it arrives at the appliance as two arguments.
	if on "doas grep -qiE 'kern|usb' /data/log/messages"; then
		ok "kernel messages are in it (klogd is feeding the same file)"
	else
		bad "no kernel messages: klogd is not running, or not logging here"
	fi
	# Older than this boot is the whole point. The first line of the current
	# file predating the boot proves the reboot did not empty it; a rotated
	# generation proves the same thing and that rotation works.
	if on doas test -f /data/log/messages.0; then
		ok "a rotated generation exists, so the size bound is being applied"
	else
		note "no rotation yet - expected on an appliance that has not logged 200 KiB"
	fi
else
	bad "/data/log/messages is missing: syslogd is still writing to the RAM ring"
fi

say "USB power policy"
configured=$(api status | tr ',' '\n' | sed -n 's/.*"usb_autosuspend":\(-\{0,1\}[0-9]*\).*/\1/p' | head -1)
active=$(on cat /sys/module/usbcore/parameters/autosuspend 2>/dev/null || echo "")
if [ -n "$configured" ] && [ "$configured" = "$active" ]; then
	ok "usbcore autosuspend is $active, which is what the configuration asks for"
else
	bad "configured '$configured' but the kernel says '${active:-unreadable}'"
fi
# The parameter alone proves nothing about the adapter that was probed at boot.
device=$(api config | sed -n 's/.*"device":"\([^"]*\)".*/\1/p' | head -1)
if [ -n "$device" ]; then
	# control=on holds a device awake; so does a negative autosuspend_delay_ms,
	# whatever control says - the kernel does not suspend a device whose delay is
	# negative. A device probed after the appliance last applied its policy shows
	# exactly that combination, having taken the delay from the module parameter,
	# and reading it as a fault is how this check first reported a healthy camera
	# as a problem.
	awake=$(on "for d in /sys/bus/usb/devices/*/; do
		c=\$(cat \$d/power/control 2>/dev/null) || continue
		ms=\$(cat \$d/power/autosuspend_delay_ms 2>/dev/null)
		case \"\$c:\$ms\" in
			on:*) ;;
			*:-*) ;;
			*) echo \"\$(basename \$d) control=\$c delay=\$ms\" ;;
		esac
	done")
	if [ -n "$configured" ] && [ "$configured" -lt 0 ] 2>/dev/null; then
		if [ -z "$awake" ]; then
			ok "no USB device can be suspended"
		else
			bad "autosuspend is off but these could still suspend: $awake"
		fi
	else
		note "autosuspend is enabled deliberately; devices may suspend"
	fi
fi

say "A configuration from a newer version"
# The rollback case, checked against the binary that is actually installed
# rather than against a unit test's idea of it. grbl-backend only reads and
# compares, so pointing it at a copy costs nothing.
backend=$(api config | sed -n 's/.*"backend":"\([a-z0-9]*\)".*/\1/p' | head -1)
# /data/config.yaml is 0640 root:root, and grbl-backend has to be able to read
# the copy, so both halves run through doas. It only reads and compares - the
# appliance's own configuration is not touched either way.
if on "doas sh -c 'cp /data/config.yaml /tmp/newer.yaml &&
	printf \"  spindle_warmup_seconds: 8\ncoolant:\n  enabled: true\n\" >> /tmp/newer.yaml &&
	LASERBRIDGE_CONFIG=/tmp/newer.yaml /usr/sbin/laserbridge grbl-backend $backend'" >/dev/null 2>&1; then
	ok "an unknown key and an unknown section are read past, not refused"
else
	bad "this version would quarantine a configuration written by a newer one"
fi
on doas rm -f /tmp/newer.yaml >/dev/null 2>&1 || true

warnings=$(api status | sed -n 's/.*"config_warnings":\[\([^]]*\)\].*/\1/p')
if [ -n "$warnings" ]; then
	note "the appliance is reporting: $warnings"
else
	ok "the stored configuration was fully understood"
fi

say "The controller the bridge is talking to"
# Since the bridge asks $I and $$ in the window before the first client is
# served, the record should name the firmware and the one setting the whole
# escalation turns on. If $32 is off on this machine, a feed hold does not stop
# the output and on_disconnect must stay on reset.
if identity=$(on grep -h "controller:" /data/laserbridge/bridge-journal.log 2>/dev/null | tail -1) &&
	[ -n "$identity" ]; then
	ok "the controller identified itself"
	note "$identity"
	case "$identity" in
		*"laser mode (\$32) is OFF"*)
			note "WARNING: a feed hold will not switch this output off; keep on_disconnect=reset" ;;
		*"laser mode (\$32) unknown"*)
			note "the settings dump did not arrive; the ladder runs without knowing \$32" ;;
	esac
else
	bad "the record does not name the controller: the opening questions never got an answer"
fi

say "The console records only while somebody is reading"
# Two asks a second apart. The first starts the recording and returns nothing,
# because nothing was being kept when the traffic went past; the second says it
# is recording. That is the whole contract, and it is the part that cannot be
# checked without a real controller talking on a real port.
first=$(api "grbl/console?after=0" || true)
case "$first" in
	*'"available":false'*)
		note "laserbridged is not carrying the port; nothing to record" ;;
	*)
		sleep 2
		second=$(api "grbl/console?after=0" || true)
		case "$second" in
			*'"recording":true'*) ok "the transcript is kept for a reader that keeps asking" ;;
			*) bad "the second ask did not report a recording transcript: $second" ;;
		esac ;;
esac

say "A stale save is refused"
# Two tabs, or a tab and somebody at the SSH prompt. Only the rejected write is
# attempted here, so nothing is changed either way.
jar=$(mktemp)
token=$(curl -fsS -m 5 -c "$jar" "http://$HOST/api/session" | sed -n 's/.*"csrf_token":"\([^"]*\)".*/\1/p')
body=$(api config)
status=$(curl -s -o /dev/null -w '%{http_code}' -m 5 -b "$jar" -X PUT \
	-H "X-CSRF-Token: $token" -H 'Content-Type: application/json' \
	-H 'If-Match: "0000000000000000"' -d "$body" "http://$HOST/api/config")
rm -f "$jar"
if [ "$status" = "412" ]; then
	ok "a save naming a configuration that is not the current one is refused"
else
	bad "a stale save answered $status, want 412"
fi

say "Result"
printf '  %d checks passed, %d failed\n' "$passed" "$failed"
if [ "$STOP_WATCHDOG" != yes ]; then
	echo
	echo "Not covered without --stop-watchdog: whether stopping the watchdog"
	echo "service is a delayed reset (CONFIG_WATCHDOG_NOWAYOUT)."
fi
cat <<'MANUAL'

Still worth doing by hand, and not by any script:

  1. Install an update, reboot, then roll back to the previous slot from the
     System page and reboot again. The appliance must come back on the same
     network with the same hostname and Wi-Fi - that is the whole point of the
     configuration being readable by both slots (ADR 0016), and it is the one
     path that involves two real system slots.

  2. Reboot with the laser deliberately left on - M3 S50 from a console, lid
     open, scrap underneath. Coming back up must find the output off: the
     re-enumeration of USB toggles DTR and resets an Arduino-based controller,
     and if it does not, the bridge's own watchdog has one second with nobody
     connected to reach the same end. This is the third branch of the recovery
     invariant and the only one no test can establish.

  3. Pull the power mid-job. Nothing here should be corrupted: /data is ext4
     with errors=remount-ro and everything of consequence is written through
     atomicfile. Check /data/log/messages afterwards - it should still be
     there, and it should say what happened before the lights went out.
MANUAL

[ "$failed" -eq 0 ]
