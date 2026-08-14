#!/bin/sh
set -eu

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
	echo "usage: $0 UPDATE.lbu [SSH_PRIVATE_KEY]" >&2
	exit 2
fi

bundle=$1
key=${2:-laserbridge_ed25519}

if [ ! -f "$bundle" ]; then
	echo "Update bundle not found: $bundle" >&2
	exit 1
fi
if [ ! -f "$key" ]; then
	echo "SSH private key not found: $key" >&2
	exit 1
fi

ssh-keygen -Y sign -f "$key" -n laserbridge-update "$bundle"
echo "Created $bundle.sig"
