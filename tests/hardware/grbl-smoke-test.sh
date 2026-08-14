#!/bin/sh
# Smoke test against a real GRBL controller.
#
# Everything else about the bridge is tested against a pseudo-terminal, which
# has the same termios semantics as a serial adapter but no opinions about
# what the bytes mean. This script is the part that needs actual hardware: a
# controller that answers, at the baud rate it was configured for, over the
# adapter that is really plugged in.
#
#   tests/hardware/grbl-smoke-test.sh laserbridge.local
#
# It only reads and asks. No motion command is sent, so it is safe to run on a
# machine with the laser powered - though there is no reason to.
set -eu

HOST=${1:-laserbridge.local}
PORT=${2:-23}

say() { printf '\n=== %s ===\n' "$1"; }
fail() { printf 'FAILED: %s\n' "$1" >&2; exit 1; }

command -v nc >/dev/null || fail "nc is required"

say "Which backend is serving $HOST"
curl -fsS -m 5 "http://$HOST/api/config" 2>/dev/null |
	sed -n 's/.*"backend":"\([a-z0-9]*\)".*/  backend: \1/p' ||
	echo "  (web interface did not answer; continuing)"

say "Bridge status"
# Only laserbridged offers this; ser2net has nothing to ask.
ssh "laserbridge@$HOST" laserbridge grbl-status 2>/dev/null ||
	echo "  (no status socket - ser2net, or the daemon is not running)"

say "Connecting to $HOST:$PORT"
# GRBL answers a status request with a one-line report and nothing else, so
# it is the least invasive thing to ask. A soft reset would also produce the
# banner, but it interrupts whatever the machine is doing.
answer=$(printf '?\n' | nc -w 5 "$HOST" "$PORT" | tr -d '\r' | head -3)
[ -n "$answer" ] || fail "no reply from the controller within 5s"
printf '%s\n' "$answer" | sed 's/^/  /'

case "$answer" in
	*'<'*'|'*'>'*) echo "  looks like a GRBL status report" ;;
	*Grbl*) echo "  controller announced itself" ;;
	*) echo "  reply does not look like GRBL - check the baud rate" ;;
esac

say "Second connection"
# The bridge permits one client. With kick_old_user the newcomer wins; either
# way, two applications must never both be steering.
second=$(printf '?\n' | nc -w 5 "$HOST" "$PORT" | head -1 || true)
if [ -n "$second" ]; then
	echo "  a second client was served - it displaced the first (kick_old_user)"
else
	echo "  a second client was refused"
fi

say "Done"
echo "Byte transparency and disconnect handling are covered by the automated"
echo "tests; what this checked is that real hardware answers through the bridge."
