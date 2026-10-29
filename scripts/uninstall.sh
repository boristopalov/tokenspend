#!/bin/sh
# Undo scripts/install.sh. Collected data in ~/.tokenspend/otel is kept.
set -eu

ENDPOINT="http://127.0.0.1:4318"
launchctl bootout "gui/$(id -u)/com.tokenspend.otel" 2>/dev/null || true
rm -f "$HOME/Library/LaunchAgents/com.tokenspend.otel.plist"

for f in "$HOME/.claude/settings.json" ${CLAUDE_SETTINGS:-}; do
  [ -f "$f" ] || continue
  tmp=$(mktemp)
  # Only remove the keys install.sh set, and only if they still point here.
  jq --arg ep "$ENDPOINT" 'if .env.OTEL_EXPORTER_OTLP_ENDPOINT == $ep then
      .env |= del(.CLAUDE_CODE_ENABLE_TELEMETRY, .OTEL_LOGS_EXPORTER, .OTEL_METRICS_EXPORTER,
                  .OTEL_EXPORTER_OTLP_PROTOCOL, .OTEL_EXPORTER_OTLP_ENDPOINT, .OTEL_LOG_TOOL_DETAILS)
      | if .env == {} then del(.env) else . end
    else . end' "$f" >"$tmp" && cat "$tmp" >"$f" && rm "$tmp"  # cat keeps the file's mode
  echo "cleaned $f"
done

CODEX_CONFIG="${CODEX_HOME:-$HOME/.codex}/config.toml"
if [ -f "$CODEX_CONFIG" ] && grep -q '^# Added by tokenspend' "$CODEX_CONFIG"; then
  tmp=$(mktemp)
  # Drop the block install.sh appended: the blank line before it, the marker
  # comment, [otel] and its two keys. Lines are printed one behind so the
  # blank line can still be dropped when the marker shows up.
  awk '/^# Added by tokenspend/ { if (has && held != "") print held; has = 0; skip = 3; next }
       skip > 0 { skip--; next }
       { if (has) print held; held = $0; has = 1 }
       END { if (has) print held }' "$CODEX_CONFIG" >"$tmp"
  cat "$tmp" >"$CODEX_CONFIG" && rm "$tmp"
  echo "cleaned $CODEX_CONFIG"
fi

rm -f "${PI_CODING_AGENT_DIR:-$HOME/.pi/agent}/extensions/tokenspend-otel.ts"
echo "removed Pi extension; binary left at ~/.local/bin/tokenspend"
