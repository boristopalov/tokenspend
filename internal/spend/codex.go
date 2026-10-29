package spend

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Codex writes one rollout-*.jsonl per thread under sessions/YYYY/MM/DD.
// Each model response gets a token_usage_record (input_tokens includes cached
// tokens). The model comes from the turn_context of the record's turn.
// Subagent threads (guardian auto-review, spawned agents) get their own
// rollout whose session_meta.source says what spawned them.

type cxLine struct {
	Timestamp string    `json:"timestamp"`
	Type      string    `json:"type"`
	Payload   cxPayload `json:"payload"`
}

type cxPayload struct {
	Type string `json:"type"`

	// session_meta
	ID               string          `json:"id"`
	SessionID        string          `json:"session_id"`
	Cwd              string          `json:"cwd"`
	Source           json.RawMessage `json:"source"`
	Originator       string          `json:"originator"`
	ThreadSource     string          `json:"thread_source"`
	BaseInstructions json.RawMessage `json:"base_instructions"`

	// turn_context
	TurnID string `json:"turn_id"`
	Model  string `json:"model"`

	// response_item
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Arguments json.RawMessage `json:"arguments"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
	Tools     json.RawMessage `json:"tools"`

	// token_usage_record
	ThreadID   string   `json:"thread_id"`
	ResponseID string   `json:"response_id"`
	Usage      *cxUsage `json:"usage"`

	// event_msg/token_count
	RateLimits *struct {
		LimitID  string `json:"limit_id"`
		PlanType string `json:"plan_type"`
		Primary  *struct {
			UsedPercent   float64 `json:"used_percent"`
			WindowMinutes int     `json:"window_minutes"`
		} `json:"primary"`
		Secondary *struct {
			UsedPercent   float64 `json:"used_percent"`
			WindowMinutes int     `json:"window_minutes"`
		} `json:"secondary"`
	} `json:"rate_limits"`

	// compacted
	ReplacementHistory   json.RawMessage `json:"replacement_history"`
	CompactionResponseID string          `json:"compaction_response_id"`
}

type cxUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
}

type cxToolUse struct{ name, group, target string }

// codexUnits lists every rollout under a Codex sessions (or archived_sessions) directory.
func codexUnits(dir string) ([]loadUnit, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() && strings.HasPrefix(e.Name(), "rollout-") && strings.HasSuffix(e.Name(), ".jsonl") {
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)
	units := make([]loadUnit, len(files))
	for i, f := range files {
		units[i] = loadUnit{size: fileSizes(f), load: func(d *Dataset) { d.loadCodexRollout(f) }}
	}
	return units, err
}

func (d *Dataset) loadCodexRollout(path string) {
	// Pass 1: models per turn and which responses were compactions. Only
	// lines that mention those types can be one, which skips decoding the
	// bulk of the file (tool output) a second time.
	turnModel := map[string]string{}
	compactions := map[string]bool{}
	_ = eachLine(path, func(raw []byte) {
		if !bytes.Contains(raw, []byte("turn_context")) && !bytes.Contains(raw, []byte("compacted")) {
			return
		}
		var l cxLine
		if json.Unmarshal(raw, &l) != nil {
			return
		}
		p := &l.Payload
		if l.Type == "turn_context" && p.TurnID != "" {
			turnModel[p.TurnID] = p.Model
		}
		if l.Type == "compacted" && p.CompactionResponseID != "" {
			compactions[p.CompactionResponseID] = true
		}
	})

	fileThread := ""
	if m := rolloutID.FindStringSubmatch(filepath.Base(path)); m != nil {
		fileThread = m[1]
	}
	var (
		meta      *cxPayload
		project   string
		source    = SourceMain
		detail    string
		lastModel string
		items     []*ContextItem
		respStart = -1 // len(items) when the current model response began
		tools     = map[string]cxToolUse{}
	)
	base := func(ts string) ContextItem {
		sid := fileThread
		if meta != nil && meta.SessionID != "" {
			sid = meta.SessionID
		}
		return ContextItem{Tool: Codex, SessionID: sid, ThreadID: fileThread, Project: project, Time: parseTime(ts)}
	}
	add := func(ts, kind, name, group, target string, chars, images int) {
		it := base(ts)
		it.Kind, it.Name, it.Group, it.Target, it.EstTokens = kind, name, group, target, estTokens(chars, images)
		if it.EstTokens > 0 {
			items = append(items, d.newItem(&it))
		}
	}
	// markOutput records where a model response starts: its own output items
	// are not part of the input that response was billed for.
	markOutput := func() {
		if respStart < 0 {
			respStart = len(items)
		}
	}
	var addResponseItem func(ts string, p *cxPayload)
	addResponseItem = func(ts string, p *cxPayload) {
		switch p.Type {
		case "message":
			text := textOf(p.Content)
			chars, images := contentSize(p.Content)
			switch {
			case p.Role == "assistant":
				markOutput()
				add(ts, KindAssistantText, "", "", "", chars, images)
			case p.Role == "developer" || p.Role == "system":
				add(ts, KindInstructions, p.Role, p.Role, "", chars, images)
			case reminderPrefix.MatchString(text):
				add(ts, KindInjected, "", "", "", chars, images)
			default:
				add(ts, KindUserPrompt, "", "", "", chars, images)
			}
		case "reasoning":
			markOutput() // sized from usage when the response's record arrives
		case "function_call", "custom_tool_call", "local_shell_call", "tool_search_call", "web_search_call":
			markOutput()
			args := p.Arguments
			if len(args) == 0 {
				args = p.Input
			}
			use := codexToolUse(p.Name, args)
			tools[p.CallID] = use
			add(ts, KindToolCall, use.name, use.group, use.target, len(args), 0)
		case "function_call_output", "custom_tool_call_output", "local_shell_call_output", "tool_search_output":
			use, ok := tools[p.CallID]
			if !ok {
				use = cxToolUse{name: "unknown", group: "unknown"}
			}
			chars, images := contentSize(p.Output)
			add(ts, KindToolResult, use.name, use.group, use.target, chars, images)
		case "additional_tools":
			add(ts, KindToolSchemas, "additional_tools", "additional_tools", "", len(p.Tools), 0)
		case "agent_message":
			chars, images := contentSize(p.Content)
			add(ts, KindInjected, "agent_message", "agent_message", "", chars, images)
		case "compaction", "ghost_snapshot":
			// Context is reset by the "compacted" line instead.
		default:
			chars, images := contentSize(p.Content)
			add(ts, KindOther, p.Type, p.Type, "", chars, images)
		}
	}

	_ = eachLine(path, func(raw []byte) {
		var l cxLine
		if json.Unmarshal(raw, &l) != nil {
			d.badLine(path)
			return
		}
		p := l.Payload
		switch l.Type {
		case "session_meta":
			// Forked rollouts repeat the parent's meta after their own; keep the first.
			if meta != nil {
				return
			}
			meta = &p
			project = p.Cwd
			source, detail = codexSource(&p)
			var bi struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(p.BaseInstructions, &bi) == nil && bi.Text != "" {
				add(l.Timestamp, KindInstructions, "base_instructions", "base_instructions", "", len(bi.Text), 0)
			}
		case "turn_context":
			if p.Model != "" {
				lastModel = p.Model
			}
		case "response_item":
			addResponseItem(l.Timestamp, &p)
		case "compacted":
			// The compacted history replaces everything before it.
			items = nil
			respStart = -1
			chars, images := contentSize(p.ReplacementHistory)
			add(l.Timestamp, KindCompaction, "compacted", "compacted", "", chars, images)
		case "event_msg":
			if p.Type == "token_count" && p.RateLimits != nil && p.RateLimits.Primary != nil {
				s := RateLimitSample{Time: parseTime(l.Timestamp), Plan: p.RateLimits.PlanType,
					PrimaryPercent: p.RateLimits.Primary.UsedPercent, PrimaryWindow: p.RateLimits.Primary.WindowMinutes}
				if p.RateLimits.Secondary != nil {
					s.SecondaryPercent, s.SecondaryWindow = p.RateLimits.Secondary.UsedPercent, p.RateLimits.Secondary.WindowMinutes
				}
				if p.RateLimits.LimitID == "" || p.RateLimits.LimitID == "codex" {
					d.CodexRateLimits = append(d.CodexRateLimits, s)
				}
			}
		case "token_usage_record":
			if p.Usage == nil {
				return
			}
			u := p.Usage
			model := turnModel[p.TurnID]
			if model == "" {
				model = lastModel
			}
			sid := p.SessionID
			if sid == "" {
				sid = p.ThreadID
			}
			r := &Request{
				Tool: Codex, SessionID: sid, ThreadID: p.ThreadID, Project: project, Time: parseTime(l.Timestamp),
				Model: model, Source: source, SourceDetail: detail, Origin: OriginTranscript, RequestID: p.ResponseID,
				Uncached:   max(0, u.InputTokens-u.CachedInputTokens-u.CacheWriteInputTokens),
				CacheRead:  u.CachedInputTokens,
				CacheWrite: u.CacheWriteInputTokens,
				Output:     u.OutputTokens, Reasoning: u.ReasoningOutputTokens,
			}
			if compactions[p.ResponseID] {
				r.Source, r.SourceDetail = SourceCompaction, "remote compaction"
			}
			ctx := items
			if respStart >= 0 && respStart <= len(items) {
				ctx = items[:respStart]
			}
			respStart = -1
			if d.addRequest(r) {
				d.attribute(r, ctx)
			}
			if u.ReasoningOutputTokens > 0 {
				it := base(l.Timestamp)
				it.Kind, it.EstTokens = KindThinking, u.ReasoningOutputTokens
				items = append(items, d.newItem(&it))
			}
		}
	})
}

var (
	rolloutID   = regexp.MustCompile(`([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`)
	codeModeUse = regexp.MustCompile(`tools\.([A-Za-z0-9_]+)\s*\(`)
	codeModeCmd = regexp.MustCompile(`\bcmd\s*:\s*"((?:[^"\\]|\\.)*)"`)
)

// codexToolUse names a tool call. Code-mode "exec" calls run a script that
// calls other tools, so they are named after the first tool the script uses.
func codexToolUse(name string, args json.RawMessage) cxToolUse {
	if name != "exec" {
		return cxToolUse{name: name, group: toolGroup(name), target: toolTarget(args)}
	}
	var script string
	if json.Unmarshal(args, &script) != nil {
		script = string(args)
	}
	use := cxToolUse{name: "exec", group: "exec"}
	if m := codeModeUse.FindStringSubmatch(script); m != nil {
		use.name, use.group = m[1], toolGroup(m[1])
	}
	if m := codeModeCmd.FindStringSubmatch(script); m != nil {
		use.target = truncate(m[1], 160)
	}
	return use
}

// codexSource classifies a thread from its session_meta.
func codexSource(p *cxPayload) (source, detail string) {
	var s string
	if json.Unmarshal(p.Source, &s) == nil {
		return SourceMain, firstNonEmpty(p.Originator, s)
	}
	var obj struct {
		Subagent map[string]json.RawMessage `json:"subagent"`
	}
	if json.Unmarshal(p.Source, &obj) == nil && obj.Subagent != nil {
		if other, ok := obj.Subagent["other"]; ok {
			var kind string
			_ = json.Unmarshal(other, &kind)
			if kind == "guardian" {
				return SourceReview, "guardian auto-review"
			}
			return SourceSubagent, kind
		}
		for kind := range obj.Subagent { // e.g. "thread_spawn"
			return SourceSubagent, kind
		}
	}
	return SourceMain, firstNonEmpty(p.Originator, "unknown")
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
