#!/bin/sh
# Copies BlitzKVM's approval module (MIT; D20's broker) into
# third_party/blitzkvm-approval, which go.mod's replace points at, so the
# build needs no network and no sibling checkout. Run after changing it:
#   scripts/fork/sync_approval.sh [../blitzkvm]
# It never deletes: a file that left the module is reported, for removal by
# hand. SOURCE records the commit it came from ("+changes": uncommitted, which
# CI refuses: sync from a committed, pushed BlitzKVM).
set -eu
src="${1:-../blitzkvm}/approval"
dst="third_party/blitzkvm-approval"
[ -f "$src/go.mod" ] || { echo "no approval module at $src" >&2; exit 1; }
mkdir -p "$dst"
cp "$src/go.mod" "$src/go.sum" "$src/LICENSE" "$src/PROTOCOL.md" "$dst/"
stale=0
for pkg in wire broker; do
	mkdir -p "$dst/$pkg"
	for f in "$src/$pkg"/*.go; do
		case "$f" in *_test.go) ;; *) cp "$f" "$dst/$pkg/" ;; esac
	done
	for f in "$dst/$pkg"/*.go; do
		[ -f "$src/$pkg/$(basename "$f")" ] || { echo "stale: $f (gone from $src/$pkg)" >&2; stale=1; }
	done
done
rev=$(git -C "$src/.." rev-parse --short HEAD 2>/dev/null || echo '?')
[ -z "$(git -C "$src/.." status --porcelain -- approval 2>/dev/null)" ] || rev="$rev+changes"
# CI checks the copy against this commit (fork-checks: vendored-approval).
echo "$rev" > "$dst/SOURCE"
echo "synced $rev into $dst"
exit $stale
