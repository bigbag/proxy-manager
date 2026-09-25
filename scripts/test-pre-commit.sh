#!/bin/sh
set -eu
root=$(pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
git init -q "$tmp"
mkdir -p "$tmp/.githooks" "$tmp/bin"
cp "$root/.githooks/pre-commit" "$tmp/.githooks/pre-commit"
chmod +x "$tmp/.githooks/pre-commit"
git -C "$tmp" config user.name Test
git -C "$tmp" config user.email test@example.invalid
printf test > "$tmp/file"
git -C "$tmp" add file
printf '#!/bin/sh\nprintf "%%s\\n" "$*" > "$PROBE_FILE"\nexit 1\n' > "$tmp/bin/go"
chmod +x "$tmp/bin/go"
if PROBE_FILE="$tmp/args" PATH="$tmp/bin:$PATH" git -C "$tmp" -c core.hooksPath=.githooks commit -qm test; then
    printf 'failed scanner allowed commit\n' >&2
    exit 1
fi
if [ "$(cat "$tmp/args")" != 'run github.com/securego/gosec/v2/cmd/gosec@v2.29.0 ./...' ]; then
    printf 'hook did not run the pinned Go scanner\n' >&2
    exit 1
fi
printf '#!/bin/sh\nexit 0\n' > "$tmp/bin/go"
PROBE_FILE="$tmp/args" PATH="$tmp/bin:$PATH" git -C "$tmp" -c core.hooksPath=.githooks commit -qm test
