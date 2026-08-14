#!/bin/sh
# Prints every shell script in the tree.
#
# CI scans the whole directory, while the Makefile used to name files one by
# one - so a file nobody remembered to add was checked on GitHub and never
# locally, and the first push failed on it. Both now ask this script.
set -eu

find . \
	-path ./.git -prune -o \
	-path ./dist -prune -o \
	-path ./.local-data -prune -o \
	-path ./.local-run -prune -o \
	-type f -print |
	while IFS= read -r file; do
		case "$file" in
			*.go | *.md | *.yaml | *.yml | *.json | *.html | *.css | *.js) continue ;;
			*.sh) printf '%s\n' "$file"; continue ;;
		esac
		# Otherwise go by the shebang, which is how a script without a
		# suffix - an init script, a hook - announces itself.
		read -r first < "$file" 2>/dev/null || continue
		case "$first" in
			'#!'*sh | '#!'*sh\ * | '#!'*openrc-run*) printf '%s\n' "$file"; continue ;;
		esac
		# A file meant to be sourced has no shebang and says so with a
		# directive instead. ramboot-init is one.
		if head -5 "$file" 2>/dev/null | grep -q '^# shellcheck shell='; then
			printf '%s\n' "$file"
		fi
	done | sort
