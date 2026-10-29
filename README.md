# tokenspend

Shows where LLM token spend goes across Claude Code, Codex and Pi: by tool, by
source (main thread, subagents, compaction, auto-review, background calls), by
model, cache economics, and which context items (tool results, files,
instructions, tool schemas, thinking) the input tokens paid for.

It has two layers:

1. **Normalizer + dashboard.** Reads the transcripts each tool already writes
   and builds one self-contained HTML file. Nothing needs to be installed.
2. **OTel receiver + exporters.** Catches calls that never reach a transcript,
   such as Claude Code's session-title and `agent_summary` calls and Codex
   prewarm requests. A local OTLP/HTTP receiver records them, and the next
   report merges them in.

## Usage

```sh
GOTOOLCHAIN=local go build -o ~/.local/bin/tokenspend .

tokenspend report                  # writes ~/.tokenspend/dashboard.html
tokenspend report -since 7d -o /tmp/spend.html
tokenspend summary -since 30d      # totals by tool, source and model in the terminal
tokenspend export -format csv -table requests > requests.csv
tokenspend export -table context   # per-context-item attribution, JSONL
tokenspend prices > prices.json    # edit, then pass -prices prices.json
tokenspend otel                    # run the receiver on 127.0.0.1:4318
tokenspend status                  # is the receiver up, is each tool exporting to it?
```

Shared flags: `-since` (`7d`, `12h` or `2026-10-01`), `-prices`, `-otel-dir`
(empty skips OTel), `-claude-dir`, `-codex-dir`, `-pi-dir`
(comma-separated), and `-quiet`. Progress goes to stderr: a live status line
on a terminal, one line per stage otherwise. `-quiet` turns it off.

Log files are parsed in parallel, one per CPU. Each Claude Code session,
Codex rollout and Pi session is parsed on its own. The results are then
merged in a fixed order, so the output doesn't depend on the number of CPUs. The default directories honor `CLAUDE_CONFIG_DIR`,
`CODEX_HOME` and `PI_CODING_AGENT_DIR`.

## Sources read

| Tool | Files | Notes |
|---|---|---|
| Claude Code | `~/.claude/projects/**/*.jsonl` and `subagents/` | One API response spans several assistant lines and is deduped by `requestId`. `api-request` lines give `querySource`. `api-request-shape` lines give the system prompt and tool schemas. `cost-state` totals expose calls that were never logged. |
| Codex | `~/.codex/sessions`, `archived_sessions` | One `token_usage_record` per response, with the model from `turn_context`. `session_meta.source` separates guardian auto-review threads and spawned subagents. Rate-limit samples feed the plan-usage chart. |
| Pi | `~/.pi/agent/sessions` | Uses the recorded USD cost when present. Also reads usage from tool results (nested model calls), `usage` entries, compaction and branch summaries. |

## Layer 2: OTel

| Tool | Exporter | Event used |
|---|---|---|
| Claude Code | Built in ([monitoring docs](https://code.claude.com/docs/en/monitoring-usage)) | `claude_code.api_request` |
| Codex | Built in, `[otel]` in `config.toml` | `codex.sse_event` with `event.kind=response.completed` |
| Pi | `extensions/pi/tokenspend-otel.ts` (written for this project) | `pi.api_request` |

`scripts/install.sh` sets all three up and backs up every file it touches to
`~/.tokenspend/backups/<timestamp>/`:

- Builds the binary to `~/.local/bin/tokenspend`.
- Runs `tokenspend otel` as the launchd agent `com.tokenspend.otel`.
- Merges these keys into the `env` block of `~/.claude/settings.json`, plus any
  files listed in `$CLAUDE_SETTINGS`: `CLAUDE_CODE_ENABLE_TELEMETRY=1`,
  `OTEL_LOGS_EXPORTER=otlp`, `OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf`,
  `OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318` and
  `OTEL_LOG_TOOL_DETAILS=1`. The last one makes `api_request` carry
  `agent.name`, which marks subagent calls. Metrics aren't exported because
  nothing reads them; an `OTEL_METRICS_EXPORTER=otlp` left by an earlier
  install is removed.
- Appends an `[otel]` block to the Codex `config.toml` that sends logs to the
  receiver over OTLP/HTTP (binary). Prompts are not logged.
- Copies the Pi extension into `~/.pi/agent/extensions/`.

New sessions pick up these settings; running sessions don't. `scripts/uninstall.sh`
reverses everything except the collected data in `~/.tokenspend/otel`.

`tokenspend status` checks the whole chain:
- the receiver answers `/healthz`, and the launchd agent's state;
- each tool's config exports logs over OTLP/HTTP to this receiver;
- when each tool last sent anything, and its event and billed-request counts
  for the last 24 hours.

It exits non-zero when the receiver is down.

The receiver accepts protobuf or JSON (optionally gzipped) on `/v1/logs` and
`/v1/metrics`. It accepts `/v1/traces` and drops the data. Records are
flattened and appended to one of two daily files in `~/.tokenspend/otel`:

- `usage-YYYY-MM-DD.jsonl` holds the billed request events the report reads.
  These are kept forever. Claude Code deletes transcripts after 30 days by
  default (`cleanupPeriodDays`), and from then on these events are the only
  record of those requests. The report falls back to them automatically.
- `events-YYYY-MM-DD.jsonl` holds everything else (hooks, tool decisions and
  results, metrics). These are deleted after 30 days; change that with
  `tokenspend otel -keep-events-days N`, where 0 keeps them forever.

The receiver tidies the directory when it starts and then every hour:
- Files from earlier days are gzipped, which shrinks them about 11×. The
  loaders read `.gz` files directly.
- Files from before the usage/events split (`otel-YYYY-MM-DD.jsonl`) are
  divided into the two kinds.
- Expired events files are deleted.

Today's files are left alone until the day is over. Usage files grow by
roughly 10–20 MB a year with daily use.

When building a report, an OTel event that matches a transcript request
(Claude Code `request_id`, Pi `response_id`, or otherwise equal token counts in
the same session) only enriches it: Claude Code's `query_source` and reported
cost. Events with no match are added as requests with origin `otel`.

## Method

- **Cost.** Uses the recorded cost when the tool logged one (Pi). Otherwise
  applies the built-in price table (USD per million tokens). Anthropic cache
  writes cost 1.25× input for 5-minute writes and 2× input for 1-hour writes,
  and long-context tiers are applied. Each request's basis is shown as
  `recorded`, `table`, `assumed` (the price table entry is an estimate, not a
  published rate) or `unpriced`.
  Unpriced models produce a warning and count as $0.
- **Claude Code reconciliation.** Claude Code's own `cost-state` totals include
  calls the transcript never logged. The difference becomes a background
  request labeled "unlogged (cost-state gap)". A session whose reported total
  is below the transcript total is usually a resumed session; that is reported
  but not corrected.
- **Context attribution.** For each request, the context it carried is
  rebuilt: the Claude Code and Pi message trees, Codex's linear history reset
  at each compaction, the system prompt and tool schemas, and thinking. The
  request's input cost is split across those items by estimated size. Item
  sizes are estimated at about 4 characters per token, and an image counts as
  1600 tokens. A per-thread fit `actual ≈ slope × estimate + intercept`
  corrects tokenizer bias (the slope) and assigns context that was never
  logged (the intercept) to an `unattributed` item. Attributed item costs
  always sum to the transcript input cost; a test checks this.

## Caveats

- Token counts and costs come from the logs. Item sizes in context attribution
  are estimates, so read them as shares, not exact bills.
- Calls that bypass both the transcripts and OTel can't be seen. For Claude
  Code, the `cost-state` gap bounds these.
- The price table is a snapshot. Check it with `tokenspend prices` and
  override it with `-prices` when rates change or a model shows up unpriced.
- OTel data only exists from the moment the exporters are enabled.

## Development

```sh
GOTOOLCHAIN=local go vet ./... && GOTOOLCHAIN=local go test ./...
```

Go 1.23 with `GOTOOLCHAIN=local`: newer OTLP proto modules require a newer Go,
so `go.opentelemetry.io/proto/otlp` is pinned to v1.7.0. Tests use fixture
transcripts and run the OTLP receiver end to end against the loader.
