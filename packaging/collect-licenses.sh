#!/bin/sh
# Collect this project's licence and the licence and notice files of every
# vendored module into build/licenses, under the modules' paths, for the
# archives, packages and image to carry.
set -eu
out=build/licenses
rm -rf "$out"
mkdir -p "$out"
cp LICENSE "$out/LICENSE"
find vendor -type f \( -name 'LICENSE*' -o -name 'LICENCE*' -o -name 'NOTICE*' -o -name 'PATENTS*' -o -name 'COPYING*' \) |
	while IFS= read -r f; do
		mkdir -p "$out/$(dirname "$f")"
		cp "$f" "$out/$f"
	done
