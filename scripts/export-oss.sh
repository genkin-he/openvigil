#!/bin/sh
# Export the committed tree as a fresh open-source repository with no history.
#
#   scripts/export-oss.sh <target-dir> [denylist-file]
#
# Private design notes are left out, and every file is scanned for the strings in the denylist (one per
# line, case-insensitive; default ~/.vigil/oss-denylist.txt, kept outside the repo so it never ships).
# A line starting with "!" names an exact string that may appear (say the public repo's owner/name): it is
# cut out of a hit before checking again, so the word it contains is still caught everywhere else.
# Any remaining hit aborts the export. The result is a new git repo with one commit; nothing is pushed.
set -eu

TARGET="${1:?usage: scripts/export-oss.sh <target-dir> [denylist-file]}"
DENY="${2:-$HOME/.vigil/oss-denylist.txt}"
EXCLUDE="docs/superpowers"

case "$TARGET" in /*) ;; *) TARGET="$PWD/$TARGET" ;; esac # relative to where it was called from
[ -e "$TARGET" ] && { echo "export: $TARGET already exists" >&2; exit 1; }
[ -f "$DENY" ] || { echo "export: denylist $DENY not found" >&2; exit 1; }
ROOT="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)" # the repo this script belongs to
cd "$ROOT"
git diff --quiet HEAD -- || echo "export: note — uncommitted changes are not exported" >&2

mkdir -p "$TARGET"
git archive --format=tar HEAD | tar -x -C "$TARGET"
for p in $EXCLUDE; do rm -rf "$TARGET/$p"; done
rmdir "$TARGET/docs" 2>/dev/null || true

patterns="$(mktemp)"
allow="$(mktemp)"
trap 'rm -f "$patterns" "$allow"' EXIT
grep -v '^[[:space:]]*\(#\|!\|$\)' "$DENY" > "$patterns"
printf '%s\n' "/Use""rs/" >> "$patterns"   # absolute home paths (split so this line does not match itself)
sed -n 's/^!//p' "$DENY" > "$allow"
hits="$(cd "$TARGET" && grep -rniF -f "$patterns" . |
  ALLOW="$allow" perl -pe 'BEGIN { open my $f, "<", $ENV{ALLOW} or die; chomp(@a = <$f>) } for my $s (@a) { s/\Q$s\E//gi }' |
  grep -iF -f "$patterns" || true)"
if [ -n "$hits" ]; then
  printf '%s\n' "$hits"
  echo "export: private strings found above — fix them, commit, and export again ($TARGET left for inspection)" >&2
  exit 1
fi

cd "$TARGET"
git init -q -b main
git add -A
git commit -q -m "Initial release"
echo "Exported to $TARGET ($(git ls-files | wc -l | tr -d ' ') files). Review, then push it to the new public repo."
