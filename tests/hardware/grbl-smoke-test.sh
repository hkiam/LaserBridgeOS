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

say "Laser mode"
# The setting that decides whether a feed hold switches the beam off. Everything
# the appliance does about an unattended laser is shaped by this answer, and it
# is the one thing no test away from the machine can establish.
# shellcheck disable=SC2016  # the $32 is GRBL's setting name, not a variable
laser=$(printf '$$\n' | nc -w 5 "$HOST" "$PORT" | tr -d '\r' | sed -n 's/^\$32=\(.*\)$/\1/p')
case "$laser" in
	1) echo "  \$32=1, laser mode on: a feed hold is expected to switch the beam off" ;;
	0) echo "  \$32=0, LASER MODE OFF: this controller treats the laser as a spindle." >&2
	   echo "  A feed hold will NOT switch the beam off. Keep on_disconnect on 'reset'." >&2 ;;
	*) echo "  could not read \$32 (answer: '${laser:-none}')" ;;
esac

say "What the bridge has recorded"
ssh "laserbridge@$HOST" laserbridge grbl-journal 2>/dev/null | head -40 ||
	echo "  (no journal - ser2net, or the daemon is not running)"

say "Done"
echo "Byte transparency, disconnect handling and the escalation are covered by"
echo "the automated tests against a simulated controller - which only proves the"
echo "bridge reacts correctly to each answer, not that your controller gives the"
echo "answer we assume. Two things are worth checking by hand, with scrap"
echo "material and the lid open:"
echo
echo "  1. Start a cut, then pull the network cable. The beam must go out."
echo "     Then: ssh laserbridge@$HOST laserbridge grbl-journal"
echo "     and read what the bridge did and why."
echo
echo "  2. With laserbridged running, restart it during a job:"
echo "     ssh laserbridge@$HOST doas rc-service laserbridged restart"
echo "     The controller must NOT reset - no Grbl banner, no lost position."
