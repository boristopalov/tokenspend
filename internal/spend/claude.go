package spend

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Claude Code writes one JSONL transcript per session under
// <projects>/<project>/<session>.jsonl, plus <session>/subagents/agent-*.jsonl.
// One API response is spread over several "assistant" lines that share a
// requestId and repeat the same usage, so usage is counted once per requestId.
// "api-request" lines (2.1.29x+) carry the querySource of each main-thread
// request, joined to assistant lines through requestRef.

type ccLine struct {
	Type             string          `json:"type"`
	UUID             string          `json:"uuid"`
	ParentUUID       string          `json:"parentUuid"`
	IsSidechain      bool            `json:"isSidechain"`
	IsMeta           bool            `json:"isMeta"`
	IsCompactSummary bool            `json:"isCompactSummary"`
	Cwd              string          `json:"cwd"`
	Timestamp        string          `json:"timestamp"`
	RequestID        string          `json:"requestId"`
	RequestRef       string          `json:"requestRef"`
	Message          json.RawMessage `json:"message"`
	Attachment       json.RawMessage `json:"attachment"`
	Rendered         json.RawMessage `json:"rendered"`

	// api-request and api-request-shape lines
	ID          string          `json:"id"`
	QuerySource string          `json:"querySource"`
	ShapeHash   string          `json:"shapeHash"`
	Shape       json.RawMessage `json:"shape"`

	// cost-state lines
	TotalCostUSD float64                 `json:"totalCostUSD"`
	ModelUsage   map[string]ccModelUsage `json:"modelUsage"`

	// fork-context-ref lines
	ParentLastUUID string `json:"parentLastUuid"`
}

type ccModelUsage struct {
	InputTokens              int64   `json:"inputTokens"`
	OutputTokens             int64   `json:"outputTokens"`
	CacheReadInputTokens     int64   `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64   `json:"cacheCreationInputTokens"`
	CostUSD                  float64 `json:"costUSD"`
}

type ccMessage struct {
	Model   string          `json:"model"`
	ID      string          `json:"id"`
	Content json.RawMessage `json:"content"`
	Usage   *ccUsage        `json:"usage"`
}

type ccUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	OutputTokensDetails      struct {
		ThinkingTokens int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
	CacheCreation *struct {
		Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

type ccBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

type ccToolUse struct{ name, target string }

// ccThread is one transcript file: the main session or one subagent.
type ccThread struct {
	sessionID, threadID, project string
	subagentType                 string // set for subagent files
	lines                        []ccLine
	nodes                        map[string]*ccNode
	tools                        map[string]ccToolUse
	apiRequests                  map[string]ccAPIRequest // api-request id -> request metadata
	shapeItems                   map[string][]*ContextItem
	forkParentLast               string    // fork subagents continue the parent's chain here
	parent                       *ccThread // main thread, for subagents
}

type ccAPIRequest struct{ querySource, shapeHash string }

// ccShape is the static part of a request: system prompt and tool definitions.
type ccShape struct {
	systemChars int
	tools       []ccShapeTool
}

type ccShapeTool struct {
	name  string
	chars int
}

type ccNode struct {
	parent string
	items  []*ContextItem
}

type ccCostState struct {
	sessionID, project string
	time               time.Time
	totalCostUSD       float64
	modelUsage         map[string]ccModelUsage
}

// claudeUnits lists every session under a Claude Code projects directory.
// A session's main transcript and its subagents load as one unit: subagent
// threads continue the main thread's context, and a session's request
// shapes are defined in its own files.
func claudeUnits(projectsDir string) ([]loadUnit, error) {
	projects, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil, err
	}
	var units []loadUnit
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		dir := filepath.Join(projectsDir, p.Name())
		entries, _ := os.ReadDir(dir)
		sessions := map[string]bool{}
		for _, e := range entries {
			if name, ok := strings.CutSuffix(e.Name(), ".jsonl"); ok && !e.IsDir() {
				sessions[name] = true
			} else if e.IsDir() {
				sessions[e.Name()] = true // subagents may exist without a main file
			}
		}
		names := make([]string, 0, len(sessions))
		for s := range sessions {
			names = append(names, s)
		}
		sort.Strings(names)
		for _, s := range names {
			subs, _ := filepath.Glob(filepath.Join(dir, s, "subagents", "agent-*.jsonl"))
			units = append(units, loadUnit{
				size: fileSizes(append(subs, filepath.Join(dir, s+".jsonl"))...),
				load: func(d *Dataset) { d.loadClaudeSession(dir, s) },
			})
		}
	}
	return units, nil
}

func (d *Dataset) loadClaudeSession(dir, session string) {
	main := d.readClaudeThread(filepath.Join(dir, session+".jsonl"), session, session, nil)
	threads := []*ccThread{main}
	subs, _ := filepath.Glob(filepath.Join(dir, session, "subagents", "agent-*.jsonl"))
	sort.Strings(subs)
	for _, path := range subs {
		agentID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		t := d.readClaudeThread(path, session, session+"/"+agentID, main)
		t.subagentType = "agent"
		var meta struct {
			AgentType string `json:"agentType"`
		}
		if b, err := os.ReadFile(strings.TrimSuffix(path, ".jsonl") + ".meta.json"); err == nil && json.Unmarshal(b, &meta) == nil && meta.AgentType != "" {
			t.subagentType = meta.AgentType
		}
		if t.project == "" {
			t.project = main.project
		}
		threads = append(threads, t)
	}
	if main.project == "" {
		for _, t := range threads {
			if t.project != "" {
				main.project = t.project
				break
			}
		}
	}
	for _, t := range threads {
		d.processClaudeThread(t)
	}
}

// readClaudeThread parses a transcript file and builds its context nodes.
func (d *Dataset) readClaudeThread(path, sessionID, threadID string, parent *ccThread) *ccThread {
	t := &ccThread{
		sessionID: sessionID, threadID: threadID, parent: parent,
		nodes: map[string]*ccNode{}, tools: map[string]ccToolUse{},
		apiRequests: map[string]ccAPIRequest{}, shapeItems: map[string][]*ContextItem{},
	}
	var lastCost *ccLine
	_ = eachLine(path, func(raw []byte) {
		var l ccLine
		if json.Unmarshal(raw, &l) != nil {
			d.badLine(path)
			return
		}
		switch l.Type {
		case "api-request":
			t.apiRequests[l.ID] = ccAPIRequest{querySource: l.QuerySource, shapeHash: l.ShapeHash}
			return
		case "api-request-shape":
			d.addClaudeShape(l.ShapeHash, l.Shape)
			return
		case "cost-state":
			lastCost = &l
			return
		case "fork-context-ref":
			t.forkParentLast = l.ParentLastUUID
			return
		}
		if t.project == "" && l.Cwd != "" {
			t.project = l.Cwd
		}
		if l.UUID != "" {
			t.lines = append(t.lines, l)
		}
	})
	if lastCost != nil && parent == nil {
		cs := &ccCostState{sessionID: sessionID, project: t.project, totalCostUSD: lastCost.TotalCostUSD, modelUsage: lastCost.ModelUsage}
		if prev := d.ccCostStates[sessionID]; prev == nil || prev.totalCostUSD < cs.totalCostUSD {
			d.ccCostStates[sessionID] = cs
		}
	}
	for i := range t.lines {
		l := &t.lines[i]
		t.nodes[l.UUID] = &ccNode{parent: l.ParentUUID, items: d.claudeItems(t, l)}
	}
	return t
}

// claudeItems turns one transcript line into the context items it adds.
func (d *Dataset) claudeItems(t *ccThread, l *ccLine) []*ContextItem {
	base := ContextItem{Tool: ClaudeCode, SessionID: t.sessionID, ThreadID: t.threadID, Project: t.project, Time: parseTime(l.Timestamp)}
	item := func(kind, name, target string, chars, images int) *ContextItem {
		it := base
		it.Kind, it.Name, it.Group, it.Target = kind, name, toolGroup(name), target
		it.EstTokens = estTokens(chars, images)
		return d.newItem(&it)
	}
	var out []*ContextItem
	switch l.Type {
	case "attachment":
		var a struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(l.Attachment, &a)
		var rendered []struct {
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(l.Rendered, &rendered)
		chars, images := 0, 0
		for _, r := range rendered {
			c, i := contentSize(r.Content)
			chars, images = chars+c, images+i
		}
		if chars+images > 0 {
			out = append(out, item(KindAttachment, a.Type, "", chars, images))
		}
	case "user", "assistant":
		var m ccMessage
		if json.Unmarshal(l.Message, &m) != nil {
			return nil
		}
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			out = append(out, item(claudeTextKind(l, s), "", "", len(s), 0))
			return out
		}
		var blocks []ccBlock
		_ = json.Unmarshal(m.Content, &blocks)
		for _, b := range blocks {
			switch b.Type {
			case "text":
				kind := KindAssistantText
				if l.Type == "user" {
					kind = claudeTextKind(l, b.Text)
				}
				out = append(out, item(kind, "", "", len(b.Text), 0))
			case "tool_use", "server_tool_use":
				target := toolTarget(b.Input)
				t.tools[b.ID] = ccToolUse{name: b.Name, target: target}
				out = append(out, item(KindToolCall, b.Name, target, len(b.Input), 0))
			case "tool_result", "web_search_tool_result", "web_fetch_tool_result":
				use, ok := t.tools[b.ToolUseID]
				if !ok && t.parent != nil {
					use = t.parent.tools[b.ToolUseID]
				}
				if use.name == "" {
					use.name = "unknown"
				}
				c, i := contentSize(b.Content)
				out = append(out, item(KindToolResult, use.name, use.target, c, i))
			case "image":
				out = append(out, item(KindImage, "", "", 0, 1))
			case "thinking", "redacted_thinking":
				// Sized from usage on the request's first line instead; the
				// transcript often stores only a summary.
			default:
				c, i := contentSize(json.RawMessage(mustJSON(b)))
				out = append(out, item(KindOther, b.Type, "", c, i))
			}
		}
	}
	return out
}

func claudeTextKind(l *ccLine, text string) string {
	switch {
	case l.IsCompactSummary:
		return KindCompaction
	case l.IsMeta:
		return KindInjected
	case reminderPrefix.MatchString(text):
		return KindSystemReminder
	}
	return KindUserPrompt
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// node finds a context node in this thread or, for subagents, the main thread.
func (t *ccThread) node(id string) *ccNode {
	if n := t.nodes[id]; n != nil {
		return n
	}
	if t.parent != nil {
		return t.parent.nodes[id]
	}
	return nil
}

// context collects the items on the parent chain ending at id. A compaction
// boundary has no parent, so the walk stops there. Fork subagents start from
// a copy of the parent's context, so their chain continues in the main thread.
func (t *ccThread) context(id string) []*ContextItem {
	var out []*ContextItem
	seen := map[string]bool{}
	jumped := false
	for id != "" && !seen[id] {
		seen[id] = true
		n := t.node(id)
		if n == nil {
			break
		}
		out = append(out, n.items...)
		id = n.parent
		if id == "" && !jumped && t.forkParentLast != "" {
			id, jumped = t.forkParentLast, true
		}
	}
	return out
}

// processClaudeThread emits one Request per API response and attributes its input.
func (d *Dataset) processClaudeThread(t *ccThread) {
	type pending struct {
		first *ccLine
		msg   ccMessage
	}
	var order []string
	reqs := map[string]*pending{}
	for i := range t.lines {
		l := &t.lines[i]
		if l.Type != "assistant" {
			continue
		}
		var m ccMessage
		if json.Unmarshal(l.Message, &m) != nil || m.Usage == nil || m.Model == "<synthetic>" {
			continue
		}
		key := l.RequestID
		if key == "" {
			key = m.ID
		}
		if key == "" {
			key = l.UUID
		}
		p := reqs[key]
		if p == nil {
			p = &pending{first: l, msg: m}
			reqs[key] = p
			order = append(order, key)
		} else if m.Usage.OutputTokens > p.msg.Usage.OutputTokens {
			p.msg.Usage = m.Usage // later lines may carry the final output count
		}
	}
	for _, key := range order {
		p := reqs[key]
		u := p.msg.Usage
		r := &Request{
			Tool: ClaudeCode, SessionID: t.sessionID, ThreadID: t.threadID, Project: t.project,
			Time: parseTime(p.first.Timestamp), Model: p.msg.Model, Origin: OriginTranscript, RequestID: key,
			Uncached: u.InputTokens, CacheRead: u.CacheReadInputTokens, Output: u.OutputTokens,
			Reasoning: u.OutputTokensDetails.ThinkingTokens,
		}
		if u.CacheCreation != nil && u.CacheCreation.Ephemeral5m+u.CacheCreation.Ephemeral1h == u.CacheCreationInputTokens {
			r.CacheWrite, r.CacheWrite1h = u.CacheCreation.Ephemeral5m, u.CacheCreation.Ephemeral1h
		} else {
			r.CacheWrite = u.CacheCreationInputTokens
		}
		switch {
		case t.subagentType != "":
			r.Source, r.SourceDetail = SourceSubagent, "agent:"+t.subagentType
		case t.apiRequests[p.first.RequestRef].querySource != "":
			r.SourceDetail = t.apiRequests[p.first.RequestRef].querySource
			r.Source = ClassifyClaudeQuerySource(r.SourceDetail)
		case p.first.IsSidechain:
			r.Source, r.SourceDetail = SourceSubagent, "sidechain"
		default:
			r.Source, r.SourceDetail = SourceMain, "main (no querySource logged)"
		}
		if !d.addRequest(r) {
			continue
		}
		ctx := append(d.claudeShapeItems(t, t.apiRequests[p.first.RequestRef].shapeHash, r), t.context(p.first.ParentUUID)...)
		d.attribute(r, ctx)
		// This response's thinking stays in context for later requests.
		if u.OutputTokensDetails.ThinkingTokens > 0 {
			if n := t.nodes[p.first.UUID]; n != nil {
				n.items = append(n.items, d.newItem(&ContextItem{
					Tool: ClaudeCode, SessionID: t.sessionID, ThreadID: t.threadID, Project: t.project, Time: r.Time,
					Kind: KindThinking, EstTokens: u.OutputTokensDetails.ThinkingTokens,
				}))
			}
		}
	}
}

// ClassifyClaudeQuerySource maps Claude Code's query_source onto a source bucket.
func ClassifyClaudeQuerySource(qs string) string {
	switch {
	case qs == "" || qs == "sdk" || strings.HasPrefix(qs, "repl_main_thread"):
		return SourceMain
	case qs == "agent", strings.HasPrefix(qs, "agent:"), strings.Contains(qs, "subagent"): // not "agent_summary"
		return SourceSubagent
	case strings.Contains(qs, "compact"):
		return SourceCompaction
	}
	return SourceBackground
}

// reconcileClaude compares each session's cost-state (Claude Code's own
// running totals, which include calls never written to the transcript) with
// the requests found for that session, and adds one "unlogged" request per
// model for the token gap.
func (d *Dataset) reconcileClaude() {
	type sums struct{ in, out, read, write int64 }
	found := map[string]map[string]*sums{} // session -> model -> tokens
	transcriptUSD := map[string]float64{}
	otelUSD := map[string]float64{}
	last := map[string]time.Time{}
	for _, r := range d.Requests {
		if r.Tool != ClaudeCode {
			continue
		}
		if found[r.SessionID] == nil {
			found[r.SessionID] = map[string]*sums{}
		}
		s := found[r.SessionID][r.Model]
		if s == nil {
			s = &sums{}
			found[r.SessionID][r.Model] = s
		}
		s.in += r.Uncached
		s.out += r.Output
		s.read += r.CacheRead
		s.write += r.CacheWrite + r.CacheWrite1h
		if r.Origin == OriginOTel {
			otelUSD[r.SessionID] += r.Cost.Total()
		} else {
			transcriptUSD[r.SessionID] += r.Cost.Total()
		}
		if r.Time.After(last[r.SessionID]) {
			last[r.SessionID] = r.Time
		}
	}
	ids := make([]string, 0, len(d.ccCostStates))
	for id := range d.ccCostStates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		cs := d.ccCostStates[id]
		if _, ok := last[id]; !ok && !d.since.IsZero() {
			continue // session entirely outside the time filter
		}
		check := ClaudeSessionCheck{SessionID: id, Project: cs.project, ReportedUSD: cs.totalCostUSD,
			TranscriptUSD: transcriptUSD[id], OTelOnlyUSD: otelUSD[id]}
		models := make([]string, 0, len(cs.modelUsage))
		for m := range cs.modelUsage {
			models = append(models, m)
		}
		sort.Strings(models)
		for _, model := range models {
			mu := cs.modelUsage[model]
			have := found[id][model]
			if have == nil {
				have = &sums{}
			}
			r := &Request{
				Tool: ClaudeCode, SessionID: id, ThreadID: id, Project: cs.project, Time: last[id], Model: model,
				Source: SourceBackground, SourceDetail: "unlogged (cost-state gap)", Origin: OriginCostState,
				Uncached:   max(0, mu.InputTokens-have.in),
				Output:     max(0, mu.OutputTokens-have.out),
				CacheRead:  max(0, mu.CacheReadInputTokens-have.read),
				CacheWrite: max(0, mu.CacheCreationInputTokens-have.write), // TTL unknown; priced as 5-minute writes
			}
			if r.Time.IsZero() {
				r.Time = time.Now()
			}
			if d.addRequest(r) {
				check.UnloggedUSD += r.Cost.Total()
			}
		}
		d.ClaudeSessions = append(d.ClaudeSessions, check)
	}
}

// addClaudeShape records the sizes in an api-request-shape line.
func (d *Dataset) addClaudeShape(hash string, raw json.RawMessage) {
	if hash == "" || d.ccShapes[hash] != nil {
		return
	}
	var shape struct {
		System json.RawMessage   `json:"system"`
		Tools  []json.RawMessage `json:"tools"`
	}
	if json.Unmarshal(raw, &shape) != nil {
		return
	}
	sh := &ccShape{}
	sh.systemChars, _ = contentSize(shape.System)
	for _, tool := range shape.Tools {
		var def struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(tool, &def)
		sh.tools = append(sh.tools, ccShapeTool{name: def.Name, chars: len(tool)})
	}
	d.ccShapes[hash] = sh
}

// claudeShapeItems returns the system prompt and tool definition items for a
// request shape, created once per thread so their cost accumulates.
func (d *Dataset) claudeShapeItems(t *ccThread, hash string, r *Request) []*ContextItem {
	if items, ok := t.shapeItems[hash]; ok || hash == "" {
		return items
	}
	sh := d.ccShapes[hash]
	if sh == nil {
		return nil
	}
	item := func(kind, name string, chars int) *ContextItem {
		return d.newItem(&ContextItem{
			Tool: ClaudeCode, SessionID: t.sessionID, ThreadID: t.threadID, Project: t.project, Time: r.Time,
			Kind: kind, Name: name, Group: toolGroup(name), EstTokens: estTokens(chars, 0),
		})
	}
	items := []*ContextItem{item(KindInstructions, "system_prompt", sh.systemChars)}
	for _, tool := range sh.tools {
		items = append(items, item(KindToolSchemas, tool.name, tool.chars))
	}
	t.shapeItems[hash] = items
	return items
}
