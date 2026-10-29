package otelrecv_test

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	logs "go.opentelemetry.io/proto/otlp/logs/v1"
	resource "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"tokenspend/internal/otelrecv"
	"tokenspend/internal/spend"
)

func str(k, v string) *common.KeyValue {
	return &common.KeyValue{Key: k, Value: &common.AnyValue{Value: &common.AnyValue_StringValue{StringValue: v}}}
}

func num(k string, v int64) *common.KeyValue {
	return &common.KeyValue{Key: k, Value: &common.AnyValue{Value: &common.AnyValue_IntValue{IntValue: v}}}
}

func post(t *testing.T, url, contentType string, body []byte, gz bool) {
	t.Helper()
	if gz {
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		_, _ = w.Write(body)
		_ = w.Close()
		body = buf.Bytes()
	}
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	if gz {
		req.Header.Set("Content-Encoding", "gzip")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s: %s", url, resp.Status)
	}
}

// A Claude Code api_request event for a background call, sent as gzipped
// protobuf, and a Pi event sent as JSON, both become requests in the dataset.
func TestReceiveAndLoad(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer((&otelrecv.Receiver{Dir: dir}).Handler())
	defer srv.Close()

	now := uint64(time.Now().UnixNano())
	ccReq := &collogs.ExportLogsServiceRequest{ResourceLogs: []*logs.ResourceLogs{{
		Resource: &resource.Resource{Attributes: []*common.KeyValue{str("service.name", "claude-code")}},
		ScopeLogs: []*logs.ScopeLogs{{LogRecords: []*logs.LogRecord{{
			TimeUnixNano: now,
			Body:         &common.AnyValue{Value: &common.AnyValue_StringValue{StringValue: "claude_code.api_request"}},
			Attributes: []*common.KeyValue{
				str("event.name", "api_request"), str("session.id", "s1"), str("model", "claude-haiku-5-5"),
				str("query_source", "agent_summary"), str("request_id", "req_bg"),
				num("input_tokens", 1200), num("output_tokens", 80), num("cache_read_tokens", 0),
				num("cache_creation_tokens", 0), str("cost_usd", "0.00016"),
			},
		}}}},
	}}}
	body, err := proto.Marshal(ccReq)
	if err != nil {
		t.Fatal(err)
	}
	post(t, srv.URL+"/v1/logs", "application/x-protobuf", body, true)

	piJSON := `{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"pi"}}]},
	  "scopeLogs":[{"logRecords":[{"timeUnixNano":"` + strconv.FormatUint(now, 10) + `","eventName":"pi.api_request","attributes":[
	    {"key":"session.id","value":{"stringValue":"pi-1"}},{"key":"model","value":{"stringValue":"deepseek/deepseek-v4.1-flash"}},
	    {"key":"response_id","value":{"stringValue":"gen-1"}},{"key":"input_tokens","value":{"intValue":"300"}},
	    {"key":"output_tokens","value":{"intValue":"20"}},{"key":"cache_read_tokens","value":{"intValue":"1000"}},
	    {"key":"cost_usd","value":{"doubleValue":0.0002}},{"key":"cost.input","value":{"doubleValue":0.0001}},
	    {"key":"cost.output","value":{"doubleValue":0.00005}},{"key":"cost.cache_read","value":{"doubleValue":0.00005}}]}]}]}]}`
	post(t, srv.URL+"/v1/logs", "application/json", []byte(piJSON), false)
	// Codex names the record after a source location; the real name is in event.name.
	codexReq := &collogs.ExportLogsServiceRequest{ResourceLogs: []*logs.ResourceLogs{{
		Resource: &resource.Resource{Attributes: []*common.KeyValue{str("service.name", "codex_exec")}},
		ScopeLogs: []*logs.ScopeLogs{{LogRecords: []*logs.LogRecord{{
			TimeUnixNano: now, EventName: "event otel/src/events/session_telemetry.rs:1132",
			Attributes: []*common.KeyValue{
				str("event.name", "codex.sse_event"), str("event.kind", "response.completed"),
				str("conversation.id", "c1"), str("model", "gpt-6.1-sol"),
				str("input_token_count", "17725"), num("cached_token_count", 13184), str("output_token_count", "5"),
			},
		}}}},
	}}}
	if body, err := proto.Marshal(codexReq); err != nil {
		t.Fatal(err)
	} else {
		post(t, srv.URL+"/v1/logs", "application/x-protobuf", body, false)
	}
	// A subagent call whose query_source is just the agent's name.
	subReq := &collogs.ExportLogsServiceRequest{ResourceLogs: []*logs.ResourceLogs{{
		Resource: &resource.Resource{Attributes: []*common.KeyValue{str("service.name", "claude-code")}},
		ScopeLogs: []*logs.ScopeLogs{{LogRecords: []*logs.LogRecord{{
			TimeUnixNano: now, EventName: "claude_code.api_request",
			Attributes: []*common.KeyValue{str("session.id", "s1"), str("model", "claude-opus-5-5"), str("request_id", "req_sub"),
				str("query_source", "Explore"), str("agent.name", "Explore"), num("input_tokens", 10), num("output_tokens", 5)},
		}}}},
	}}}
	if body, err := proto.Marshal(subReq); err != nil {
		t.Fatal(err)
	} else {
		post(t, srv.URL+"/v1/logs", "application/x-protobuf", body, false)
	}
	post(t, srv.URL+"/v1/traces", "application/x-protobuf", nil, false) // accepted and dropped

	d, err := spend.Load(spend.Options{OTelDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Requests) != 4 {
		t.Fatalf("requests = %d, want 4", len(d.Requests))
	}
	byTool := map[string]*spend.Request{}
	for _, r := range d.Requests {
		if r.RequestID == "req_sub" {
			if r.Source != spend.SourceSubagent || r.SourceDetail != "Explore" {
				t.Errorf("subagent request = %+v", r)
			}
			continue
		}
		byTool[r.Tool] = r
	}
	cc := byTool[spend.ClaudeCode]
	if cc == nil || cc.Source != spend.SourceBackground || cc.SourceDetail != "agent_summary" || cc.Uncached != 1200 ||
		cc.Origin != spend.OriginOTel || cc.ReportedCostUSD != 0.00016 {
		t.Errorf("claude code request = %+v", cc)
	}
	pi := byTool[spend.Pi]
	if pi == nil || pi.Cost.Basis != spend.BasisRecorded || pi.CacheRead != 1000 || pi.RequestID != "gen-1" {
		t.Errorf("pi request = %+v", pi)
	}

	if cx := byTool[spend.Codex]; cx == nil || cx.Uncached != 17725-13184 || cx.CacheRead != 13184 || cx.Output != 5 || cx.SessionID != "c1" {
		t.Errorf("codex request = %+v", cx)
	}

	// Re-sending the same events must not double count.
	post(t, srv.URL+"/v1/logs", "application/x-protobuf", body, false)
	if d, _ = spend.Load(spend.Options{OTelDir: dir}); len(d.Requests) != 4 {
		t.Errorf("after duplicate export: requests = %d, want 4", len(d.Requests))
	}
}
