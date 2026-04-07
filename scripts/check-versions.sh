#!/usr/bin/env bash
# Pre-commit version check for the cassandra-plugins marketplace.
#
# 1. Ensures marketplace.json and plugin.json versions stay in sync.
# 2. Warns if plugin files changed without a version bump.
# 3. On any error/warning, runs `claude --bare` on the staged diff to suggest fixes.
#
# Usage:
#   scripts/check-versions.sh          # standalone
#   Installed as pre-commit hook via .git/hooks/pre-commit

set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

MARKETPLACE=".claude-plugin/marketplace.json"
issues=()

# ── 1. Version sync check ──
while IFS=$'\t' read -r name mp_ver; do
    plugin_json="plugins/${name}/.claude-plugin/plugin.json"
    if [[ ! -f "$plugin_json" ]]; then
        issues+=("ERROR: ${name} listed in marketplace.json but ${plugin_json} not found")
        continue
    fi
    p_ver=$(python3 -c "import json; print(json.load(open('${plugin_json}'))['version'])")
    if [[ "$mp_ver" != "$p_ver" ]]; then
        issues+=("ERROR: ${name} version mismatch — marketplace.json=${mp_ver}, plugin.json=${p_ver}")
    fi
done < <(python3 -c "
import json
with open('${MARKETPLACE}') as f:
    for p in json.load(f)['plugins']:
        print(f\"{p['name']}\t{p['version']}\")
")

# ── 2. Unbumped plugin check ──
if git rev-parse HEAD >/dev/null 2>&1; then
    for plugin_dir in plugins/*/; do
        name=$(basename "$plugin_dir")
        plugin_json="${plugin_dir}.claude-plugin/plugin.json"
        [[ -f "$plugin_json" ]] || continue

        changed_files=$(git diff --cached --name-only -- "$plugin_dir" 2>/dev/null || true)
        [[ -z "$changed_files" ]] && continue

        version_changed=$(git diff --cached -- "$plugin_json" 2>/dev/null | grep -c '"version"' || true)
        if [[ "$version_changed" -eq 0 ]]; then
            issues+=("WARN: ${name} has staged changes but version was not bumped")
        fi
    done
fi

# ── 3. All clear ──
if [[ ${#issues[@]} -eq 0 ]]; then
    echo "All plugin versions in sync."
    exit 0
fi

# ── 4. Print issues ──
echo "╭─ version-check ──────────────────────────────────╮"
for issue in "${issues[@]}"; do
    echo "│ $issue"
done
echo "╰──────────────────────────────────────────────────╯"
echo ""

# ── 5. Ask Claude for fix suggestions ──
# Find claude binary — check PATH, common locations, and cmux bundle
CLAUDE_BIN=""
for candidate in \
    "$(command -v claude 2>/dev/null || true)" \
    /usr/local/bin/claude \
    /Applications/cmux.app/Contents/Resources/bin/claude \
    "${HOME}/.claude/local/claude"; do
    if [[ -n "$candidate" && -x "$candidate" ]]; then
        CLAUDE_BIN="$candidate"
        break
    fi
done
if [[ -n "$CLAUDE_BIN" ]]; then
    echo "Asking Claude for fix suggestions..."
    echo ""

    diff_context=$(git diff --cached 2>/dev/null || true)
    marketplace_content=$(cat "$MARKETPLACE")

    prompt="You are a pre-commit hook for a Claude Code plugin marketplace repo.

The following issues were found:
$(printf '%s\n' "${issues[@]}")

Here is the current marketplace.json:
${marketplace_content}

Here is the staged diff:
${diff_context}

Give a short, concrete fix — exact commands or JSON edits to resolve each issue. Use semver patch bumps unless the change warrants minor/major. Keep it under 15 lines."

    if suggestion=$(echo "$prompt" | "$CLAUDE_BIN" --bare --model haiku 2>/dev/null); then
        echo "$suggestion"
    else
        echo "(claude not available — fix the issues above manually)"
    fi
else
    echo "Install claude CLI for auto-fix suggestions, or fix the issues above manually."
fi

echo ""

# All issues block the commit
echo "Commit blocked — fix issues above."
exit 1
