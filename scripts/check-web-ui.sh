#!/bin/sh
# Guards against a mistake this file has already made three times.
#
# event.currentTarget is only set while an event is being dispatched. Every
# handler in app.js continues after an await, and by then it reads null - so
# "event.currentTarget.reset()" after an upload throws "Cannot read properties
# of null", which the surrounding catch then reports as if the upload had
# failed. It had not.
#
# The rule is therefore: capture it into a local on the way in, and use the
# local everywhere else.
set -eu

UI=web/assets/app.js
fail() {
	echo "web UI check failed: $1" >&2
	exit 1
}

[ -f "$UI" ] || fail "$UI not found"

offenders=$(grep -n 'currentTarget' "$UI" |
	grep -v '^[0-9]*: *//' |
	grep -vE '^[0-9]+: *const [A-Za-z_][A-Za-z0-9_]* = event\.currentTarget(\.elements)?;' ||
	true)

if [ -n "$offenders" ]; then
	echo "event.currentTarget must be captured into a local before the first await." >&2
	echo "Offending lines:" >&2
	printf '%s\n' "$offenders" >&2
	exit 1
fi

# The handlers reference plenty of element ids; a typo in one of them is a
# runtime error nobody sees until that panel is opened.
missing=""
# Ids never contain whitespace, so word splitting is the right reader here.
# shellcheck disable=SC2013
for id in $(grep -oE "\\\$\('#[a-zA-Z0-9-]+'\)" "$UI" | sed "s/.*#//;s/')//" | sort -u); do
	grep -q "id=\"$id\"" web/index.html || missing="$missing $id"
done
[ -z "$missing" ] || fail "app.js references ids that index.html does not define:$missing"

echo "Web UI: currentTarget captured before await, element ids resolve"
