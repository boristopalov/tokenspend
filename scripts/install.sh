#!/bin/sh
# Install tokenspend's OTel layer on macOS:
#   1. build tokenspend into ~/.local/bin
#   2. run `tokenspend otel` (OTLP/HTTP on 127.0.0.1:4318) as a launchd agent;
#      it keeps usage events forever and other events for 30 days
#   3. point Claude Code, Codex and Pi at it
# Every config file is backed up to ~/.tokenspend/backups/<timestamp>/ first.
# Re-running is safe. Undo with scripts/uninstall.sh.
set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
BIN="$HOME/.local/bin/tokenspend"
DATA="$HOME/.tokenspend"
BACKUP="$DATA/backups/$(date +%Y%m%d-%H%M%S)"
PLIST="$HOME/Library/LaunchAgents/com.tokenspend.otel.plist"
ENDPOINT="http://127.0.0.1:4318"

command -v jq >/dev/null || { echo "jq is required (brew install jq)" >&2; exit 1; }
mkdir -p "$HOME/.local/bin" "$DATA/otel" "$BACKUP"

backup() { [ -f "$1" ] && cp -p "$1" "$BACKUP/$(echo "$1" | sed "s|^$HOME/||; s|/|__|g")" || true; }

echo "building $BIN"
(cd "$ROOT" && GOTOOLCHAIN=local go build -o "$BIN" .)

echo "installing launchd agent $PLIST"
cat >"$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.tokenspend.otel</string>
  <key>ProgramArguments</key>
  <array><string>$BIN</string><string>otel</string><string>-dir</string><string>$DATA/otel</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>$DATA/otel.log</string>
  <key>StandardErrorPath</key><string>$DATA/otel.log</string>
</dict>
</plist>
EOF
launchctl bootout "gui/$(id -u)/com.tokenspend.otel" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$PLIST"

# Claude Code reads OTel settings from the "env" block of settings.json.
# CLAUDE_SETTINGS may list extra settings files (e.g. per-account config dirs).
for f in "$HOME/.claude/settings.json" ${CLAUDE_SETTINGS:-}; do
  [ -f "$f" ] || echo '{}' >"$f"
  backup "$f"
  echo "configuring Claude Code: $f"
  tmp=$(mktemp)
  # Only logs: tokenspend doesn't read metrics. Metrics an earlier install
  # turned on would also go to this endpoint, so they are switched off.
  jq --arg ep "$ENDPOINT" '.env = ((.env // {}) + {
      "CLAUDE_CODE_ENABLE_TELEMETRY": "1",
      "OTEL_LOGS_EXPORTER": "otlp",
      "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
      "OTEL_EXPORTER_OTLP_ENDPOINT": $ep,
      "OTEL_LOG_TOOL_DETAILS": "1"
    })
    | if .env.OTEL_METRICS_EXPORTER == "otlp" then del(.env.OTEL_METRICS_EXPORTER) else . end' "$f" >"$tmp" && cat "$tmp" >"$f" && rm "$tmp"  # cat keeps the file's mode
done

# Codex: add an [otel] table unless one exists.
CODEX_CONFIG="${CODEX_HOME:-$HOME/.codex}/config.toml"
if [ -f "$CODEX_CONFIG" ] && grep -q '^\[otel\]' "$CODEX_CONFIG"; then
  echo "Codex already has an [otel] section in $CODEX_CONFIG; leaving it alone"
else
  backup "$CODEX_CONFIG"
  echo "configuring Codex: $CODEX_CONFIG"
  cat >>"$CODEX_CONFIG" <<EOF

# Added by tokenspend: export usage events to the local receiver.
[otel]
log_user_prompt = false
exporter = { otlp-http = { endpoint = "$ENDPOINT/v1/logs", protocol = "binary" } }
EOF
fi

# Pi: user extensions load from ~/.pi/agent/extensions.
PI_EXT="${PI_CODING_AGENT_DIR:-$HOME/.pi/agent}/extensions"
mkdir -p "$PI_EXT"
echo "installing Pi extension: $PI_EXT/tokenspend-otel.ts"
cp "$ROOT/extensions/pi/tokenspend-otel.ts" "$PI_EXT/tokenspend-otel.ts"

sleep 1
if curl -fsS "$ENDPOINT/healthz" >/dev/null; then
  echo "receiver is up at $ENDPOINT (data: $DATA/otel, log: $DATA/otel.log)"
else
  echo "receiver did not answer at $ENDPOINT; see $DATA/otel.log" >&2
  exit 1
fi
echo "backups: $BACKUP"
echo "restart running Claude Code, Codex and Pi sessions to pick up the change"
