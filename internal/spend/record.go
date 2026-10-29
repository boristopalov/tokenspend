// Package spend normalizes token usage from Claude Code, Codex and Pi logs
// (plus OTel exports of the same tools) into one table of model requests,
// prices them, and attributes input cost to the context that caused it.
package spend

import "time"

// Tool names used in Request.Tool.
const (
	ClaudeCode = "claude-code"
	Codex      = "codex"
	Pi         = "pi"
)

// Source buckets used in Request.Source. SourceDetail keeps the raw value.
const (
	SourceMain       = "main"
	SourceSubagent   = "subagent"
	SourceCompaction = "compaction"
	SourceReview     = "auto-review"
	SourceBackground = "background"
)

// Origin values used in Request.Origin.
const (
	OriginTranscript = "transcript"
	OriginOTel       = "otel"
	OriginCostState  = "cost-state" // Claude Code's own per-session totals, minus what transcripts explain
)

// Request is one billed model call. Token fields never overlap: the total
// input the model read is Uncached + CacheRead + CacheWrite + CacheWrite1h.
type Request struct {
	Tool         string    `json:"tool"`
	SessionID    string    `json:"session"`
	ThreadID     string    `json:"thread"` // one transcript thread (main loop or one subagent)
	Project      string    `json:"project"`
	Time         time.Time `json:"ts"`
	Model        string    `json:"model"`
	Source       string    `json:"source"`
	SourceDetail string    `json:"source_detail"`
	Origin       string    `json:"origin"`
	RequestID    string    `json:"request_id,omitempty"`

	Uncached     int64 `json:"uncached"`
	CacheRead    int64 `json:"cache_read"`
	CacheWrite   int64 `json:"cache_write"`    // default TTL (Anthropic: 5 minutes)
	CacheWrite1h int64 `json:"cache_write_1h"` // Anthropic 1-hour TTL
	Output       int64 `json:"output"`         // includes Reasoning
	Reasoning    int64 `json:"reasoning"`

	Cost Cost `json:"cost"`

	// ReportedCostUSD is the tool's own cost figure when it logs one (Pi, CC OTel).
	ReportedCostUSD float64 `json:"reported_cost_usd,omitempty"`
}

// InputTokens is everything the model read for this request.
func (r *Request) InputTokens() int64 {
	return r.Uncached + r.CacheRead + r.CacheWrite + r.CacheWrite1h
}

// TotalTokens is input plus output.
func (r *Request) TotalTokens() int64 { return r.InputTokens() + r.Output }

// Cost basis values used in Cost.Basis.
const (
	BasisRecorded = "recorded" // cost written by the tool itself
	BasisTable    = "table"    // computed from the price table
	BasisAssumed  = "assumed"  // computed from a price table entry marked as an assumption
	BasisUnpriced = "unpriced" // model missing from the price table; cost is zero
)

// Cost is a request's cost in USD, split the same way as its tokens.
type Cost struct {
	Uncached   float64 `json:"uncached"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"` // both TTLs
	Output     float64 `json:"output"`
	Basis      string  `json:"basis"`
}

// Input is the cost of everything the model read.
func (c Cost) Input() float64 { return c.Uncached + c.CacheRead + c.CacheWrite }

// Total is the full request cost.
func (c Cost) Total() float64 { return c.Input() + c.Output }

// Context item kinds used in ContextItem.Kind.
const (
	KindUserPrompt     = "user_prompt"
	KindSystemReminder = "system_reminder"
	KindInjected       = "injected_context" // harness-inserted user-role text (skills, meta messages)
	KindAttachment     = "attachment"
	KindInstructions   = "instructions" // system/developer prompts that appear in the transcript
	KindToolSchemas    = "tool_schemas"
	KindToolCall       = "tool_call" // the model's tool arguments, re-read on later requests
	KindToolResult     = "tool_result"
	KindAssistantText  = "assistant_text"
	KindThinking       = "thinking"
	KindImage          = "image"
	KindCompaction     = "compaction_summary"
	KindOther          = "other"
	// KindUnattributed is the per-thread remainder: input the transcript can't
	// explain, mostly the system prompt and tool definitions.
	KindUnattributed = "unattributed"
)

// ContextItem is one piece of transcript content that sits in the model's
// context. Every later request that includes it pays to re-read it; the
// attribution pass accumulates those costs here.
type ContextItem struct {
	Tool      string    `json:"tool"`
	SessionID string    `json:"session"`
	ThreadID  string    `json:"thread"`
	Project   string    `json:"project"`
	Time      time.Time `json:"ts"`
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`   // tool name, attachment type, ...
	Group     string    `json:"group"`  // Name, or "mcp:<server>" for MCP tools
	Target    string    `json:"target"` // file path, command, URL, ...
	EstTokens int64     `json:"est_tokens"`

	Requests   int     `json:"requests"`    // later requests that carried this item
	TokenTurns float64 `json:"token_turns"` // sum of attributed tokens over those requests
	CostUSD    float64 `json:"cost_usd"`    // attributed share of those requests' input cost
}
