#!/usr/bin/env bash
#
# Check that every relative link in the documentation points at a file that
# exists.
#
# This is the defect the documentation of a project can actually have. A
# broken link to a file that was renamed reads exactly like a working one, and
# nobody finds it until they follow it.
#
# Only relative links are checked. An address on the internet needs the
# network, and a check that needs the network fails for reasons that have
# nothing to do with this repository.

set -uo pipefail

cd "$(dirname "$0")/.."

broken=0
checked=0

while IFS= read -r file; do
	# Pull the target out of every [text](target) in the file.
	while IFS= read -r target; do
		# Skip anything that is not a relative path.
		case "$target" in
			http://* | https://* | mailto:* | "#"*) continue ;;
		esac

		# Drop an anchor, so that docs.md#a-section is checked as docs.md.
		path="${target%%#*}"
		[ -z "$path" ] && continue

		# Resolve against the directory the link is written in.
		resolved="$(dirname "$file")/$path"

		checked=$((checked + 1))
		if [ ! -e "$resolved" ]; then
			echo "$file: $target does not exist"
			broken=$((broken + 1))
		fi
	done < <(grep -oE '\]\([^)]+\)' "$file" | sed -E 's/^\]\(//; s/\)$//')
done < <(find . -name '*.md' -not -path './.git/*' -not -path './bin/*')

echo "Checked $checked relative links."

if [ "$broken" -gt 0 ]; then
	echo "$broken are broken."
	exit 1
fi

echo "All of them resolve."
