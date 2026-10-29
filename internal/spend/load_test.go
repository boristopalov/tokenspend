package spend_test

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"tokenspend/internal/spend"
)

// writeJSONL writes one JSON value per line, creating parent directories.
func writeJSONL(t *testing.T, path string, lines ...any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			t.Fatal(err)
		}
	}
}

type obj = map[string]any

// fixture lays out a small log tree for all three tools and returns load options for it.
func fixture(t *testing.T) spend.Options {
	t.Helper()
	root := t.TempDir()
	claude := filepath.Join(root, "claude", "projects")
	codex := filepath.Join(root, "codex", "sessions")
	pi := filepath.Join(root, "pi", "sessions")

	// Claude Code: two main-thread requests (the first spread over two lines),
	// a subagent request, and a cost-state counter that also saw a Haiku call.
	sess := "11111111-1111-1111-1111-111111111111"
	usage1 := obj{"input_tokens": 10, "cache_creation_input_tokens": 3000, "cache_read_input_tokens": 0, "output_tokens": 200,
		"cache_creation": obj{"ephemeral_5m_input_tokens": 1000, "ephemeral_1h_input_tokens": 2000}}
	usage2 := obj{"input_tokens": 5, "cache_creation_input_tokens": 500, "cache_read_input_tokens": 3000, "output_tokens": 100,
		"cache_creation": obj{"ephemeral_5m_input_tokens": 500, "ephemeral_1h_input_tokens": 0}}
	writeJSONL(t, filepath.Join(claude, "-proj", sess+".jsonl"),
		obj{"type": "api-request-shape", "shapeHash": "h1", "shape": obj{
			"system": []obj{{"type": "text", "text": "You are Claude Code."}},
			"tools":  []obj{{"name": "Read", "description": "read files"}, {"name": "mcp__github__get_issue", "description": "issue"}}}},
		obj{"type": "api-request", "id": "ref1", "querySource": "repl_main_thread", "shapeHash": "h1"},
		obj{"type": "user", "uuid": "u1", "parentUuid": nil, "cwd": "/proj", "timestamp": "2026-10-01T10:00:00Z",
			"message": obj{"role": "user", "content": "read the file"}},
		obj{"type": "assistant", "uuid": "a1", "parentUuid": "u1", "timestamp": "2026-10-01T10:00:01Z", "requestId": "req_1", "requestRef": "ref1",
			"message": obj{"model": "claude-opus-5-5", "id": "msg_1", "usage": usage1, "content": []obj{{"type": "thinking", "thinking": ""}}}},
		obj{"type": "assistant", "uuid": "a2", "parentUuid": "a1", "timestamp": "2026-10-01T10:00:02Z", "requestId": "req_1", "requestRef": "ref1",
			"message": obj{"model": "claude-opus-5-5", "id": "msg_1", "usage": usage1, "content": []obj{
				{"type": "tool_use", "id": "toolu_1", "name": "Read", "input": obj{"file_path": "/proj/big.go"}}}}},
		obj{"type": "user", "uuid": "u2", "parentUuid": "a2", "timestamp": "2026-10-01T10:00:03Z",
			"message": obj{"role": "user", "content": []obj{{"type": "tool_result", "tool_use_id": "toolu_1", "content": string(make([]byte, 8000))}}}},
		obj{"type": "api-request", "id": "ref2", "querySource": "repl_main_thread", "shapeHash": "h1"},
		obj{"type": "assistant", "uuid": "a3", "parentUuid": "u2", "timestamp": "2026-10-01T10:00:04Z", "requestId": "req_2", "requestRef": "ref2",
			"message": obj{"model": "claude-opus-5-5", "id": "msg_2", "usage": usage2, "content": []obj{{"type": "text", "text": "done"}}}},
		obj{"type": "cost-state", "totalCostUSD": 1.0, "modelUsage": obj{
			"claude-opus-5-5":  obj{"inputTokens": 15 + 7, "outputTokens": 300 + 50, "cacheReadInputTokens": 3000 + 1000, "cacheCreationInputTokens": 3500 + 400},
			"claude-haiku-5-5": obj{"inputTokens": 900, "outputTokens": 40}}},
	)
	writeJSONL(t, filepath.Join(claude, "-proj", sess, "subagents", "agent-abc.jsonl"),
		obj{"type": "user", "uuid": "s1", "parentUuid": nil, "isSidechain": true, "timestamp": "2026-10-01T10:01:00Z",
			"message": obj{"role": "user", "content": "explore"}},
		obj{"type": "assistant", "uuid": "s2", "parentUuid": "s1", "isSidechain": true, "timestamp": "2026-10-01T10:01:01Z", "requestId": "req_sub",
			"message": obj{"model": "claude-opus-5-5", "id": "msg_s", "usage": obj{"input_tokens": 7, "cache_read_input_tokens": 1000,
				"cache_creation_input_tokens": 400, "output_tokens": 50}, "content": []obj{{"type": "text", "text": "found it"}}}},
	)
	if err := os.WriteFile(filepath.Join(claude, "-proj", sess, "subagents", "agent-abc.meta.json"), []byte(`{"agentType":"Explore"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Codex: a main thread with two responses (one logged twice) and a guardian review thread.
	thread := "22222222-2222-2222-2222-222222222222"
	usage := func(in, cached, out int) obj {
		return obj{"input_tokens": in, "cached_input_tokens": cached, "cache_write_input_tokens": 0, "output_tokens": out, "reasoning_output_tokens": 10}
	}
	writeJSONL(t, filepath.Join(codex, "2026", "10", "02", "rollout-2026-10-02T09-00-00-"+thread+".jsonl"),
		obj{"timestamp": "2026-10-02T09:00:00Z", "type": "session_meta", "payload": obj{"id": thread, "session_id": thread, "cwd": "/repo",
			"source": "vscode", "originator": "codex-tui", "base_instructions": obj{"text": "You are Codex."}}},
		obj{"timestamp": "2026-10-02T09:00:01Z", "type": "turn_context", "payload": obj{"turn_id": "t1", "model": "gpt-6.1-sol"}},
		obj{"timestamp": "2026-10-02T09:00:01Z", "type": "response_item", "payload": obj{"type": "message", "role": "user",
			"content": []obj{{"type": "input_text", "text": "fix the bug"}}}},
		obj{"timestamp": "2026-10-02T09:00:02Z", "type": "response_item", "payload": obj{"type": "function_call", "name": "shell",
			"call_id": "c1", "arguments": `{"command":["bash","-lc","cat main.go"]}`}},
		obj{"timestamp": "2026-10-02T09:00:03Z", "type": "token_usage_record", "payload": obj{"thread_id": thread, "session_id": thread,
			"turn_id": "t1", "response_id": "resp_1", "usage": usage(20000, 0, 100)}},
		obj{"timestamp": "2026-10-02T09:00:04Z", "type": "response_item", "payload": obj{"type": "function_call_output", "call_id": "c1",
			"output": string(make([]byte, 12000))}},
		obj{"timestamp": "2026-10-02T09:00:05Z", "type": "token_usage_record", "payload": obj{"thread_id": thread, "session_id": thread,
			"turn_id": "t1", "response_id": "resp_2", "usage": usage(23000, 19000, 50)}},
		obj{"timestamp": "2026-10-02T09:00:05Z", "type": "token_usage_record", "payload": obj{"thread_id": thread, "session_id": thread,
			"turn_id": "t1", "response_id": "resp_2", "usage": usage(23000, 19000, 50)}},
	)
	guardian := "33333333-3333-3333-3333-333333333333"
	writeJSONL(t, filepath.Join(codex, "2026", "10", "02", "rollout-2026-10-02T09-01-00-"+guardian+".jsonl"),
		obj{"timestamp": "2026-10-02T09:01:00Z", "type": "session_meta", "payload": obj{"id": guardian, "session_id": guardian, "cwd": "/repo",
			"source": obj{"subagent": obj{"other": "guardian"}}}},
		obj{"timestamp": "2026-10-02T09:01:00Z", "type": "turn_context", "payload": obj{"turn_id": "g1", "model": "codex-auto-review"}},
		obj{"timestamp": "2026-10-02T09:01:01Z", "type": "token_usage_record", "payload": obj{"thread_id": guardian, "session_id": guardian,
			"turn_id": "g1", "response_id": "resp_g", "usage": usage(5000, 4000, 20)}},
	)

	// Pi: an assistant message with recorded cost, and a cache-warming usage entry.
	writeJSONL(t, filepath.Join(pi, "--proj--", "2026-10-03T08-00-00Z_pi1.jsonl"),
		obj{"type": "session", "version": 3, "id": "pi-session", "timestamp": "2026-10-03T08:00:00Z", "cwd": "/proj"},
		obj{"type": "message", "id": "m1", "parentId": nil, "timestamp": "2026-10-03T08:00:01Z", "message": obj{"role": "user", "content": "hello"}},
		obj{"type": "message", "id": "m2", "parentId": "m1", "timestamp": "2026-10-03T08:00:02Z", "message": obj{
			"role": "assistant", "provider": "anthropic", "model": "claude-fable-5", "responseId": "msg_pi",
			"content": []obj{{"type": "text", "text": "hi"}},
			"usage": obj{"input": 100, "output": 10, "cacheRead": 4000, "cacheWrite": 300, "cacheWrite1h": 0,
				"cost": obj{"input": 0.001, "output": 0.0005, "cacheRead": 0.004, "cacheWrite": 0.00375, "total": 0.00925}}}},
		obj{"type": "usage", "id": "w1", "parentId": "m2", "timestamp": "2026-10-03T08:05:00Z", "kind": "cache_warm",
			"provider": "anthropic", "model": "claude-fable-5", "usage": obj{"input": 0, "output": 0, "cacheRead": 4400, "cacheWrite": 0,
				"cost": obj{"input": 0, "output": 0, "cacheRead": 0.0044, "cacheWrite": 0, "total": 0.0044}}},
	)
	return spend.Options{
		ClaudeProjectDirs: []string{claude},
		CodexSessionDirs:  []string{codex},
		PiSessionDirs:     []string{pi},
		OTelDir:           filepath.Join(root, "otel"),
	}
}

func load(t *testing.T, opts spend.Options) *spend.Dataset {
	t.Helper()
	d, err := spend.Load(opts)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func find(d *spend.Dataset, match func(*spend.Request) bool) []*spend.Request {
	var out []*spend.Request
	for _, r := range d.Requests {
		if match(r) {
			out = append(out, r)
		}
	}
	return out
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestClaudeCode(t *testing.T) {
	d := load(t, fixture(t))
	main := find(d, func(r *spend.Request) bool { return r.Tool == spend.ClaudeCode && r.Source == spend.SourceMain })
	if len(main) != 2 {
		t.Fatalf("main-thread requests = %d, want 2 (one per requestId)", len(main))
	}
	r := main[0]
	if r.SourceDetail != "repl_main_thread" || r.CacheWrite != 1000 || r.CacheWrite1h != 2000 || r.Output != 200 {
		t.Errorf("first request = %+v", r)
	}
	// Opus 5.5: $4 input, $5 5m write, $8 1h write, $20 output per MTok.
	want := (10*4 + 1000*5 + 2000*8 + 200*20) / 1e6
	if !near(r.Cost.Total(), want) || r.Cost.Basis != spend.BasisTable {
		t.Errorf("cost = %v (%s), want %v", r.Cost.Total(), r.Cost.Basis, want)
	}

	sub := find(d, func(r *spend.Request) bool { return r.Tool == spend.ClaudeCode && r.Source == spend.SourceSubagent })
	if len(sub) != 1 || sub[0].SourceDetail != "agent:Explore" {
		t.Fatalf("subagent requests = %+v", sub)
	}

	// cost-state saw a Haiku call and 7 more Opus input tokens than any transcript.
	gap := find(d, func(r *spend.Request) bool { return r.Origin == spend.OriginCostState })
	byModel := map[string]*spend.Request{}
	for _, r := range gap {
		byModel[r.Model] = r
	}
	if h := byModel["claude-haiku-5-5"]; h == nil || h.Uncached != 900 || h.Output != 40 || h.Source != spend.SourceBackground {
		t.Errorf("haiku gap = %+v", h)
	}
	// The counter's Opus tokens match main + subagent transcripts exactly, so no Opus gap.
	if o := byModel["claude-opus-5-5"]; o != nil {
		t.Errorf("opus gap = %+v, want none", o)
	}
	if len(d.ClaudeSessions) != 1 || d.ClaudeSessions[0].ReportedUSD != 1.0 {
		t.Errorf("session checks = %+v", d.ClaudeSessions)
	}
}

func TestCodex(t *testing.T) {
	d := load(t, fixture(t))
	main := find(d, func(r *spend.Request) bool { return r.Tool == spend.Codex && r.Source == spend.SourceMain })
	if len(main) != 2 {
		t.Fatalf("main requests = %d, want 2 (duplicate response_id dropped)", len(main))
	}
	r := main[1]
	if r.Model != "gpt-6.1-sol" || r.Uncached != 4000 || r.CacheRead != 19000 || r.SourceDetail != "codex-tui" || r.Project != "/repo" {
		t.Errorf("second request = %+v", r)
	}
	if want := (4000*2 + 19000*0.10 + 50*10) / 1e6; !near(r.Cost.Total(), want) {
		t.Errorf("cost = %v, want %v", r.Cost.Total(), want)
	}
	review := find(d, func(r *spend.Request) bool { return r.Tool == spend.Codex && r.Source == spend.SourceReview })
	if len(review) != 1 || review[0].Model != "codex-auto-review" || review[0].Cost.Basis != spend.BasisAssumed {
		t.Errorf("review = %+v", review)
	}
}

func TestPi(t *testing.T) {
	d := load(t, fixture(t))
	pi := find(d, func(r *spend.Request) bool { return r.Tool == spend.Pi })
	if len(pi) != 2 {
		t.Fatalf("pi requests = %d, want 2", len(pi))
	}
	if r := pi[0]; r.Cost.Basis != spend.BasisRecorded || !near(r.Cost.Total(), 0.00925) || r.Source != spend.SourceMain {
		t.Errorf("assistant = %+v", r)
	}
	if r := pi[1]; r.Source != spend.SourceBackground || r.SourceDetail != "cache_warm" || r.CacheRead != 4400 {
		t.Errorf("cache warm = %+v", r)
	}
}

// Every attributed request's input cost is fully shared out among context
// items, so item costs add up to exactly that input cost.
func TestAttributionAddsUp(t *testing.T) {
	d := load(t, fixture(t))
	var itemCost, inputCost float64
	for _, it := range d.Items {
		itemCost += it.CostUSD
	}
	for _, r := range d.Requests {
		if r.Origin == spend.OriginTranscript {
			inputCost += r.Cost.Input()
		}
	}
	if math.Abs(itemCost-inputCost) > 1e-9 {
		t.Fatalf("item cost %v != transcript input cost %v", itemCost, inputCost)
	}

	// The 8000-byte file read is in context for the second Claude Code request only.
	var read, schema *spend.ContextItem
	for _, it := range d.Items {
		if it.Kind == spend.KindToolResult && it.Target == "/proj/big.go" {
			read = it
		}
		if it.Kind == spend.KindToolSchemas && it.Group == "mcp:github" {
			schema = it
		}
	}
	if read == nil || read.Requests != 1 || read.Name != "Read" || read.CostUSD <= 0 {
		t.Errorf("file read item = %+v", read)
	}
	if schema == nil || schema.Requests != 2 {
		t.Errorf("MCP tool schema item = %+v, want carried by both requests", schema)
	}
}

func TestSince(t *testing.T) {
	opts := fixture(t)
	opts.Since = time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	d := load(t, opts)
	for _, r := range d.Requests {
		if r.Tool == spend.ClaudeCode {
			t.Errorf("request before -since kept: %+v", r)
		}
	}
	if len(d.Requests) != 5 {
		t.Errorf("requests = %d, want 5 (3 Codex + 2 Pi)", len(d.Requests))
	}
}

func TestParallelLoad(t *testing.T) {
	opts := fixture(t)
	// A forked rollout repeats its parent's records, so the same response IDs
	// show up in a second, independently parsed file.
	day := filepath.Join(opts.CodexSessionDirs[0], "2026", "10", "02")
	parent, err := os.ReadFile(filepath.Join(day, "rollout-2026-10-02T09-00-00-22222222-2222-2222-2222-222222222222.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(day, "rollout-2026-10-02T09-30-00-44444444-4444-4444-4444-444444444444.jsonl"), parent, 0o644); err != nil {
		t.Fatal(err)
	}

	var results [][]byte
	for _, workers := range []int{1, 8} {
		opts.Workers = workers
		var stages []string
		var reading spend.Progress // the last report while reading
		opts.Progress = func(p spend.Progress) {
			if len(stages) == 0 || stages[len(stages)-1] != p.Stage {
				stages = append(stages, p.Stage)
			}
			if p.Stage == spend.StageReading {
				reading = p
			}
		}
		d := load(t, opts)

		if got := len(find(d, func(r *spend.Request) bool { return r.Tool == spend.Codex && r.Source == spend.SourceMain })); got != 2 {
			t.Errorf("workers=%d: codex main requests = %d, want 2 (fork's copies dropped)", workers, got)
		}
		var itemCost, inputCost float64
		for _, it := range d.Items {
			itemCost += it.CostUSD
		}
		for _, r := range d.Requests {
			if r.Origin == spend.OriginTranscript {
				inputCost += r.Cost.Input()
			}
		}
		if math.Abs(itemCost-inputCost) > 1e-9 {
			t.Errorf("workers=%d: item cost %v != transcript input cost %v", workers, itemCost, inputCost)
		}
		want := []string{spend.StageListing, spend.StageReading, spend.StageOTel, spend.StageAttributing}
		if !slices.Equal(stages, want) {
			t.Errorf("workers=%d: stages = %v, want %v", workers, stages, want)
		}
		if reading.Files != reading.TotalFiles || reading.TotalFiles != 5 || reading.Bytes != reading.TotalBytes || reading.TotalBytes == 0 {
			t.Errorf("workers=%d: final reading progress = %+v, want all 5 units read", workers, reading)
		}

		b, err := json.Marshal(struct {
			Requests []*spend.Request
			Items    []*spend.ContextItem
		}{d.Requests, d.Items})
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, b)
	}
	if string(results[0]) != string(results[1]) {
		t.Error("loading with 1 and 8 workers gave different results")
	}
}
