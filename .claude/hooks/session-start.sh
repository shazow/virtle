#!/bin/bash
# SessionStart hook for Claude Code on the web.
#
# .mcp.json registers Codex as an MCP server (`codex mcp-server`) so Claude
# can drive a Codex session. Remote containers do not ship the codex CLI, so
# install it here and, when an API key is provided, log in non-interactively.
set -euo pipefail

if [ "${CLAUDE_CODE_REMOTE:-}" != "true" ]; then
  exit 0
fi

if ! command -v codex >/dev/null 2>&1; then
  echo "session-start: installing @openai/codex"
  npm install -g @openai/codex
fi

if [ -n "${OPENAI_API_KEY:-}" ] && ! codex login status >/dev/null 2>&1; then
  echo "session-start: logging codex in with OPENAI_API_KEY"
  printenv OPENAI_API_KEY | codex login --with-api-key
fi
