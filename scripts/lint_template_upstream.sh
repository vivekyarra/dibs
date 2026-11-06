#!/usr/bin/env bash
# Lint switch/dibs.template.yaml with Switch's own template linter
# (switch_core.template_lint.lint_template), the check Switch Console and the
# server run on upload.
#
# CI ONLY (Linux runner). It clones sandbox-quantum/switch at a pinned commit
# and runs its Python code. Do not run it on a developer machine.
#
# Usage: scripts/lint_template_upstream.sh [template-path]
# Env:   SWITCH_SHA  commit of sandbox-quantum/switch to lint against
#        STRICT=1    also fail on warnings
set -euo pipefail

SWITCH_REPO="https://github.com/sandbox-quantum/switch"
# Pinned: HEAD of sandbox-quantum/switch on 2026-11-06. Bump deliberately.
SWITCH_SHA="${SWITCH_SHA:-ba765fea843ee00592973cd9329400ba1e1e82f0}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEMPLATE="$(cd "$(dirname "${1:-$ROOT/switch/dibs.template.yaml}")" && pwd)/$(basename "${1:-$ROOT/switch/dibs.template.yaml}")"
[ -f "$TEMPLATE" ] || { echo "no template at $TEMPLATE" >&2; exit 2; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "== fetching sandbox-quantum/switch@$SWITCH_SHA"
git init -q "$WORK/switch"
git -C "$WORK/switch" remote add origin "$SWITCH_REPO"
git -C "$WORK/switch" fetch -q --depth 1 origin "$SWITCH_SHA"
git -C "$WORK/switch" checkout -q FETCH_HEAD

if ! command -v uv >/dev/null 2>&1; then
  echo "== installing uv"
  curl -LsSf https://astral.sh/uv/install.sh | sh
  export PATH="$HOME/.local/bin:$PATH"
fi

echo "== python env"
uv venv -q --python 3.12 "$WORK/venv"
PY="$WORK/venv/bin/python"
uv pip install -q --python "$PY" pydantic pyyaml
# template_lint imports switch_core.rooms_yaml, which pulls in more of
# switch_core. If pydantic + pyyaml are not enough at this SHA, install the
# declared dependencies of core/ (without building or installing the package).
if ! PYTHONPATH="$WORK/switch/core" "$PY" -c "import switch_core.template_lint" 2>/dev/null; then
  echo "== installing switch core dependencies"
  uv pip install -q --python "$PY" -r "$WORK/switch/core/pyproject.toml"
fi

echo "== linting $TEMPLATE"
cat > "$WORK/lint.py" <<'PY'
import os
import sys

from switch_core.template_lint import lint_template

path = sys.argv[1]
with open(path, encoding="utf-8") as f:
    result = lint_template(f.read())

for finding in result.errors:
    print(f"ERROR   {finding.code}: {finding.message}")
for finding in result.warnings:
    print(f"WARNING {finding.code}: {finding.message}")
print(f"{len(result.errors)} error(s), {len(result.warnings)} warning(s), blocked={result.blocked}")

if result.errors or result.blocked:
    sys.exit(1)
if os.environ.get("STRICT") == "1" and result.warnings:
    sys.exit(1)
PY

# Run from the scratch dir so nothing from this repo shadows a module.
cd "$WORK"
PYTHONPATH="$WORK/switch/core" "$PY" "$WORK/lint.py" "$TEMPLATE"
