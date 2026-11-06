#!/usr/bin/env bash
# Publish examples/demo-shop/ as its own public GitHub repo and open its 10 issues.
#
#   scripts/create_demo_repo.sh [--dry-run] <owner>/<name>
#
# Example: scripts/create_demo_repo.sh --dry-run vivekyarra/dibs-demo-shop
#
# It copies examples/demo-shop/ to a temp dir (no .venv or caches), points
# .dibs/team.yaml at <owner>/<name>, makes one commit, runs
# `gh repo create --public --source . --push`, then scripts/create_issues.sh.
# Nothing happens without an explicit <owner>/<name>. --dry-run prints the
# commands and touches nothing on GitHub.
set -euo pipefail

usage() {
  echo "usage: $0 [--dry-run] <owner>/<name>" >&2
  exit 2
}

DRY_RUN=0
REPO=""
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=1 ;;
    -h|--help) usage ;;
    -*) echo "unknown flag: $arg" >&2; usage ;;
    *) [[ -z "$REPO" ]] || usage; REPO="$arg" ;;
  esac
done
[[ "$REPO" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || usage

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="$ROOT/examples/demo-shop"
[[ -f "$SRC/issues.yaml" ]] || { echo "cannot find $SRC/issues.yaml" >&2; exit 1; }

run() {
  if [[ $DRY_RUN -eq 1 ]]; then
    printf '+'; printf ' %q' "$@"; printf '\n'
  else
    "$@"
  fi
}

if [[ $DRY_RUN -eq 0 ]]; then
  for tool in git gh tar; do
    command -v "$tool" >/dev/null || { echo "$tool is not installed" >&2; exit 1; }
  done
  gh auth status >/dev/null 2>&1 || { echo "run 'gh auth login' first" >&2; exit 1; }
  if gh repo view "$REPO" >/dev/null 2>&1; then
    echo "$REPO already exists on GitHub; pick another name or delete it yourself." >&2
    exit 1
  fi
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/dibs-demo-shop.XXXXXX")"
DEST="$WORK/${REPO#*/}"
mkdir -p "$DEST"
echo "staging demo-shop in $DEST"

# The copy and git init are local and harmless, so they run even in --dry-run
# (handy for inspecting what would be pushed).
tar -C "$SRC" \
  --exclude=./.venv --exclude=./.pytest_cache --exclude='__pycache__' \
  --exclude='*.sqlite3' --exclude='*.db' \
  -cf - . | tar -C "$DEST" -xf -

# Point the team config at the new repo.
sed -i.bak "s#^repo: .*#repo: $REPO#" "$DEST/.dibs/team.yaml" && rm -f "$DEST/.dibs/team.yaml.bak"

cd "$DEST"
git init -q -b main
git config core.autocrlf false
git add -A
git commit -q -m "demo-shop: initial import for the Dibs demo"

run gh repo create "$REPO" --public --source . --push \
  --description "Orders API used as the target repo for the Dibs demo"

issue_args=("$REPO")
[[ $DRY_RUN -eq 1 ]] && issue_args=(--dry-run "$REPO")
bash "$DEST/scripts/create_issues.sh" "${issue_args[@]}"

cat <<EOF

Next steps
  1. Edit .dibs/team.yaml in $REPO: replace SENIOR_GITHUB and JUNIOR_GITHUB with real logins.
  2. Check the CI run: https://github.com/$REPO/actions (5 xfailed = the known bugs are real).
  3. From a clone of $REPO (it carries .dibs/):   dibs init && dibs doctor
  4. Run the sweep:   dibs sweep   (or "@dibs sweep" in the team room) -> expect "DIBS for asha" on #3.
Local copy: $DEST
EOF
[[ $DRY_RUN -eq 1 ]] && echo "(dry run: nothing was created on GitHub)"
exit 0
