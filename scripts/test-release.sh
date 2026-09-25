#!/bin/sh
set -eu

makefile="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)/Makefile"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

git init -q --bare "$tmp/origin.git"
git init -q "$tmp/work"
cd "$tmp/work"
git config user.name 'Release Test'
git config user.email 'release@example.invalid'
git remote add origin "$tmp/origin.git"
printf 'initial\n' > sample.txt
git add sample.txt
git commit -q -m 'Initial service'

if make -s -f "$makefile" sys/changelog >/dev/null 2>&1; then
	echo 'Changelog succeeded without a version tag.' >&2
	exit 1
fi
if printf '1.0.0;bad\n' | make -s -f "$makefile" sys/tag >/dev/null 2>&1; then
	echo 'Tag accepted an invalid version.' >&2
	exit 1
fi
printf '1.0.0\n' | make -s -f "$makefile" sys/tag >/dev/null
printf 'updated\n' > sample.txt
git add sample.txt
git commit -q -m 'Add feature'
printf '1.1.0\n' | make -s -f "$makefile" sys/tag >/dev/null
make -s -f "$makefile" sys/changelog

test "$(git --git-dir="$tmp/origin.git" tag --list | wc -l)" -eq 2
test "$(grep -c '^## v' CHANGELOG.md)" -eq 2
test "$(grep -c '^- Initial service \[Release Test\]$' CHANGELOG.md)" -eq 1
test "$(grep -c '^- Add feature \[Release Test\]$' CHANGELOG.md)" -eq 1
first="$(git hash-object CHANGELOG.md)"
make -s -f "$makefile" sys/changelog
test "$(git hash-object CHANGELOG.md)" = "$first"
