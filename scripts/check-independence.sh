#!/bin/sh
# Fails when an SDK module requires or imports another olympus module: each SDK must be
# installable on its own. The examples module is exempt because it shows them together.
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
prefix=github.com/svallejo-dev/olympus
status=0

for modfile in "$root"/*/go.mod; do
  dir=$(dirname "$modfile")
  name=$(basename "$dir")
  [ "$name" = examples ] && continue
  self="$prefix/$name"
  if grep -n "$prefix/" "$modfile" | grep -v "module $self\$"; then
    echo "error: $name/go.mod depends on another olympus module" >&2
    status=1
  fi
  if grep -rn --include='*.go' "\"$prefix/" "$dir" | grep -v "\"$self[/\"]"; then
    echo "error: $name imports another olympus module" >&2
    status=1
  fi
done

[ "$status" -eq 0 ] && echo "ok: every SDK module is independent"
exit "$status"
