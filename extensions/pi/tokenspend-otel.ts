// tokenspend-otel: export Pi's per-request token usage and cost as OTLP logs.
//
// Pi has no built-in OpenTelemetry export. This extension sends one
// "pi.api_request" log record per billed model call (assistant messages, and
// tool results that report nested model usage) to an OTLP/HTTP collector,
// such as `tokenspend otel`. Attribute names follow Claude Code's
// claude_code.api_request event so the three tools line up.
//
// Install: copy to ~/.pi/agent/extensions/tokenspend-otel.ts
// Endpoint: TOKENSPEND_OTLP_ENDPOINT, else OTEL_EXPORTER_OTLP_ENDPOINT, else http://127.0.0.1:4318
// Disable: TOKENSPEND_OTEL=0
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

type Attr = { key: string; value: Record<string, unknown> };
type LogRecord = { timeUnixNano: string; eventName: string; attributes: Attr[] };

const FLUSH_DELAY_MS = 2000;
const TIMEOUT_MS = 3000;

function endpoint(): string {
  const base = (process.env.TOKENSPEND_OTLP_ENDPOINT || process.env.OTEL_EXPORTER_OTLP_ENDPOINT || "http://127.0.0.1:4318").replace(/\/+$/, "");
  return base.endsWith("/v1/logs") ? base : `${base}/v1/logs`;
}

function attr(key: string, v: unknown): Attr | undefined {
  if (v === undefined || v === null || v === "") return undefined;
  if (typeof v === "number") return { key, value: Number.isInteger(v) ? { intValue: String(v) } : { doubleValue: v } };
  if (typeof v === "boolean") return { key, value: { boolValue: v } };
  return { key, value: { stringValue: String(v) } };
}

export default function (pi: ExtensionAPI) {
  if (process.env.TOKENSPEND_OTEL === "0") return;
  const url = endpoint();
  let queue: LogRecord[] = [];
  let timer: ReturnType<typeof setTimeout> | undefined;
  let sessionId = "";
  let cwd = "";

  async function flush(): Promise<void> {
    if (timer) clearTimeout(timer);
    timer = undefined;
    if (queue.length === 0) return;
    const batch = queue;
    queue = [];
    const body = {
      resourceLogs: [{
        resource: { attributes: [attr("service.name", "pi"), attr("process.cwd", cwd)].filter(Boolean) },
        scopeLogs: [{ scope: { name: "tokenspend-otel" }, logRecords: batch }],
      }],
    };
    try {
      await fetch(url, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
        signal: AbortSignal.timeout(TIMEOUT_MS),
      });
    } catch {
      // No collector running: drop the batch. Pi's session file still has the data.
    }
  }

  function record(source: string, model: string, provider: string, responseId: string, usage: any, timestamp?: number) {
    const cost = usage?.cost ?? {};
    const attributes = [
      attr("event.name", "pi.api_request"),
      attr("session.id", sessionId),
      attr("cwd", cwd),
      attr("source", source),
      attr("model", model),
      attr("provider", provider),
      attr("response_id", responseId),
      attr("input_tokens", usage?.input ?? 0),
      attr("output_tokens", usage?.output ?? 0),
      attr("cache_read_tokens", usage?.cacheRead ?? 0),
      attr("cache_creation_tokens", usage?.cacheWrite ?? 0),
      attr("cache_creation_1h_tokens", usage?.cacheWrite1h ?? 0),
      attr("reasoning_tokens", usage?.reasoning ?? 0),
      attr("cost_usd", cost.total ?? 0),
      attr("cost.input", cost.input ?? 0),
      attr("cost.output", cost.output ?? 0),
      attr("cost.cache_read", cost.cacheRead ?? 0),
      attr("cost.cache_write", cost.cacheWrite ?? 0),
    ].filter((a): a is Attr => a !== undefined);
    queue.push({ timeUnixNano: String(BigInt(timestamp ?? Date.now()) * 1_000_000n), eventName: "pi.api_request", attributes });
    if (!timer) timer = setTimeout(() => void flush(), FLUSH_DELAY_MS);
  }

  pi.on("session_start", (_event, ctx) => {
    sessionId = ctx.sessionManager.getSessionId();
    cwd = ctx.cwd;
  });

  pi.on("message_end", (event) => {
    const m: any = event.message;
    const u = m?.usage;
    if (!u || (u.input ?? 0) + (u.output ?? 0) + (u.cacheRead ?? 0) + (u.cacheWrite ?? 0) === 0) return;
    if (m.role === "assistant") {
      record("agent", m.model ?? "", m.provider ?? "", m.responseId ?? "", u, m.timestamp);
    } else if (m.role === "toolResult") {
      // Model calls made inside a tool (e.g. workflows) report their usage here.
      record(`tool:${m.toolName ?? "unknown"}`, m.model ?? "", m.provider ?? "", "", u, m.timestamp);
    }
  });

  // Flush at the end of every run as well as on shutdown: print mode (pi -p)
  // can exit before the batching timer fires.
  pi.on("agent_end", async () => {
    await flush();
  });
  pi.on("session_shutdown", async () => {
    await flush();
  });
}
