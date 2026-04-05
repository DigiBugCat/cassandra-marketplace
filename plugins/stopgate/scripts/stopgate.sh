#!/usr/bin/env bash
# stopgate — Stop hook that uses an Opus LLM call to decide if Claude
# should be allowed to stop or forced to continue working.
#
# Requires either:
#   - ANTHROPIC_API_KEY env var (Console API key, billed per-use)
#   - bare-oauth patch (cassandra-cc-patches) + macOS Keychain OAuth token
#
# Reads JSON from stdin (Claude Code Stop hook protocol).
# Logs all decisions to /tmp/stopgate.log for debugging.

set -euo pipefail

LOG="/tmp/stopgate.log"
HOOK_INPUT=$(cat)
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

log() { echo "[$(date '+%H:%M:%S')] $*" >> "$LOG"; }

log "=== Stop hook fired ==="

# Extract fields
LAST_MSG=$(echo "$HOOK_INPUT" | jq -r '.last_assistant_message // empty')
STOP_HOOK_ACTIVE=$(echo "$HOOK_INPUT" | jq -r '.stop_hook_active // false')
SESSION_ID=$(echo "$HOOK_INPUT" | jq -r '.session_id // empty')

log "session: $SESSION_ID"
log "stop_hook_active: $STOP_HOOK_ACTIVE"

# If stop_hook_active is true, Claude already got blocked once and is
# retrying. Let it through to prevent infinite loops.
if [ "$STOP_HOOK_ACTIVE" = "true" ]; then
  log "verdict: APPROVE (stop_hook_active=true, already blocked once)"
  exit 0
fi

# No message to classify — let it through
if [ -z "$LAST_MSG" ]; then
  log "verdict: APPROVE (no last_assistant_message)"
  exit 0
fi

log "last_msg: $(echo "$LAST_MSG" | head -c 200)..."

# Classifier prompt
read -r -d '' PROMPT << 'CLASSIFY' || true
You are a stop-gate for an AI coding assistant. It is about to stop and return control to the user. Decide if the stop is appropriate.

Reply with APPROVE or BLOCK followed by a directive.

APPROVE when:
- The assistant completed a task with a concrete result (code written, file changed, command run, build verified)
- The assistant answered a question the user asked — providing information, analysis, or estimates IS the task
- The assistant hit a hard blocker only the user can resolve (missing credentials, permission denied)
- The assistant laid out a design space with real tradeoffs and is asking for the user's architectural preference — this is a genuine decision point, not laziness. The key distinction: are the options meaningfully different enough that picking wrong would waste significant work?

BLOCK when:
- The assistant described what it COULD do but didn't do it — just go do it
- The assistant asked a soft question ("want me to...?", "should I...?", "what do you think?") where the answer is obvious or low-stakes
- The assistant pushed code or triggered a build but didn't monitor it to completion
- The assistant told the user to wait for something async instead of watching it
- The assistant stopped mid-task or trailed off with filler

When you BLOCK, your response becomes the instruction the assistant sees. Be direct and actionable — tell it exactly what to do next. The user can always interrupt if they disagree, so bias toward action.

When you APPROVE a design question, it's fine — the user will either answer or tell the assistant to just pick one.

Examples:
- BLOCK Go ahead and build the Chrome extension. Start with the manifest and cookie listener.
- BLOCK Monitor the Woodpecker pipelines and confirm all pods are running before stopping.
- BLOCK You described the fix but didn't implement it. Make the change now.
- APPROVE (answered the user's question with concrete information)
- APPROVE (genuine architectural decision — per-service vs centralized discovery changes the whole system shape)

Last message:
CLASSIFY

# ── Find claude binary ─────────────────────────────────────────────────────

CLAUDE="${STOPGATE_CLAUDE_PATH:-}"
if [ -z "$CLAUDE" ]; then
  CLAUDE=$(command -v claude 2>/dev/null || true)
fi
if [ -z "$CLAUDE" ]; then
  for p in "$HOME/.local/bin/claude" "/usr/local/bin/claude" "/opt/homebrew/bin/claude"; do
    if [ -x "$p" ]; then CLAUDE="$p"; break; fi
  done
fi
if [ -z "$CLAUDE" ]; then
  log "verdict: APPROVE (claude binary not found)"
  exit 0
fi

# ── Resolve auth ────────────────────────────────────────────────────────────
# Priority: ANTHROPIC_API_KEY > CLAUDE_CODE_OAUTH_TOKEN env > macOS Keychain

if [ -z "${CLAUDE_CODE_OAUTH_TOKEN:-}" ] && [ -z "${ANTHROPIC_API_KEY:-}" ]; then
  # Try macOS Keychain (requires bare-oauth patch for --bare mode)
  if command -v security >/dev/null 2>&1; then
    KEYCHAIN_CREDS=$(security find-generic-password -s "Claude Code-credentials" -w 2>/dev/null) || true
    if [ -n "$KEYCHAIN_CREDS" ]; then
      export CLAUDE_CODE_OAUTH_TOKEN=$(echo "$KEYCHAIN_CREDS" | jq -r '.claudeAiOauth.accessToken // empty')
      if [ -z "$CLAUDE_CODE_OAUTH_TOKEN" ]; then
        log "warning: keychain entry found but no accessToken in it"
      fi
    fi
  fi
fi

# ── Call classifier ─────────────────────────────────────────────────────────

VERDICT=$(printf '%s\n%s' "$PROMPT" "$LAST_MSG" | "$CLAUDE" --bare --no-session-persistence -p --model opus --settings "$SCRIPT_DIR/no-hooks-settings.json" 2>>"$LOG") || {
  log "verdict: APPROVE (claude call failed, allowing stop)"
  exit 0
}

log "verdict: $VERDICT"

case "$VERDICT" in
  *BLOCK*|*block*)
    # Extract the classifier's reasoning (everything after BLOCK) as the block reason
    REASON=$(echo "$VERDICT" | sed 's/^.*BLOCK[[:space:]]*//' | tr -d '\n' | head -c 500)
    if [ -z "$REASON" ]; then
      REASON="You stopped without finishing. Continue working on the task."
    fi
    # Escape for JSON
    REASON=$(echo "$REASON" | sed 's/\\/\\\\/g; s/"/\\"/g')
    printf '{"decision":"block","reason":"%s"}' "$REASON"
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
