package spend

import (
	"encoding/json"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

// Pi writes one JSONL file per session under sessions/<cwd>/. Entries form a
// tree through parentId; a request's context is the path from the root to
// its parent, honoring the latest compaction on that path. Pi records its
// own USD cost per message. Billed usage also appears outside assistant
// messages: "usage" entries (cache warming), compaction and branch-summary
// generation, and nested model calls reported on tool results.

type piEntry struct {
	Type      string     `json:"type"`
	ID        string     `json:"id"`
	ParentID  string     `json:"parentId"`
	Timestamp string     `json:"timestamp"`
	Cwd       string     `json:"cwd"`
	Message   *piMessage `json:"message"`

	// usage entries
	Kind     string   `json:"kind"`
	Provider string   `json:"provider"`
	Model    string   `json:"model"`
	Usage    *piUsage `json:"usage"`

	// compaction / branch_summary
	Summary          string `json:"summary"`
	FirstKeptEntryID string `json:"firstKeptEntryId"`

	// custom_message
	CustomType string          `json:"customType"`
	Content    json.RawMessage `json:"content"`
}

type piMessage struct {
	Role               string          `json:"role"`
	Content            json.RawMessage `json:"content"`
	Provider           string          `json:"provider"`
	Model              string          `json:"model"`
	ResponseID         string          `json:"responseId"`
	Usage              *piUsage        `json:"usage"`
	ToolCallID         string          `json:"toolCallId"`
	ToolName           string          `json:"toolName"`
	Command            string          `json:"command"`
	Output             string          `json:"output"`
	ExcludeFromContext bool            `json:"excludeFromContext"`
	CustomType         string          `json:"customType"`
	Summary            string          `json:"summary"`
	Details            json.RawMessage `json:"details"`
}

type piUsage struct {
	Input        int64 `json:"input"`
	Output       int64 `json:"output"`
	CacheRead    int64 `json:"cacheRead"`
	CacheWrite   int64 `json:"cacheWrite"`
	CacheWrite1h int64 `json:"cacheWrite1h"` // subset of CacheWrite
	Reasoning    int64 `json:"reasoning"`    // already included in Output
	Cost         struct {
		Input      float64 `json:"input"`
		Output     float64 `json:"output"`
		CacheRead  float64 `json:"cacheRead"`
		CacheWrite float64 `json:"cacheWrite"`
		Total      float64 `json:"total"`
	} `json:"cost"`
}

type piBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// piUnits lists every session under a Pi sessions directory.
func piUnits(dir string) ([]loadUnit, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)
	units := make([]loadUnit, len(files))
	for i, f := range files {
		units[i] = loadUnit{size: fileSizes(f), load: func(d *Dataset) { d.loadPiSession(f) }}
	}
	return units, err
}

type piSession struct {
	id, project string
	byID        map[string]*piEntry
	items       map[string][]*ContextItem
}

func (d *Dataset) loadPiSession(path string) {
	s := &piSession{byID: map[string]*piEntry{}, items: map[string][]*ContextItem{}}
	var order []*piEntry
	_ = eachLine(path, func(raw []byte) {
		var e piEntry
		if json.Unmarshal(raw, &e) != nil {
			d.badLine(path)
			return
		}
		if e.Type == "session" {
			s.id, s.project = e.ID, e.Cwd
			return
		}
		if e.ID != "" {
			s.byID[e.ID] = &e
			order = append(order, &e)
		}
	})
	if s.id == "" {
		s.id = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	calls := map[string]piBlock{}
	for _, e := range order {
		s.items[e.ID] = d.piItems(s, e, calls)
	}

	lastModel := ""
	for _, e := range order {
		r := &Request{Tool: Pi, SessionID: s.id, ThreadID: s.id, Project: s.project, Time: parseTime(e.Timestamp), Origin: OriginTranscript}
		var u *piUsage
		attributeCtx := true
		switch {
		case e.Type == "message" && e.Message != nil && e.Message.Role == "assistant":
			m := e.Message
			u, r.Model, r.RequestID = m.Usage, m.Model, firstNonEmpty(m.ResponseID, e.ID)
			r.Source, r.SourceDetail = SourceMain, "agent"
			if m.Model != "" {
				lastModel = m.Model
			}
		case e.Type == "message" && e.Message != nil && e.Message.Role == "toolResult" && e.Message.Usage != nil:
			// Model calls made inside a tool (e.g. workflows); their context isn't in this transcript.
			u, r.Model, r.RequestID = e.Message.Usage, lastModel, e.ID
			r.Source, r.SourceDetail = SourceSubagent, "tool:"+e.Message.ToolName
			attributeCtx = false
		case e.Type == "usage":
			u, r.Model, r.RequestID = e.Usage, e.Model, e.ID
			r.Source, r.SourceDetail = SourceBackground, firstNonEmpty(e.Kind, "usage")
		case e.Type == "compaction" && e.Usage != nil:
			u, r.Model, r.RequestID = e.Usage, firstNonEmpty(e.Model, lastModel), e.ID
			r.Source, r.SourceDetail = SourceCompaction, "compaction"
		case e.Type == "branch_summary" && e.Usage != nil:
			u, r.Model, r.RequestID = e.Usage, firstNonEmpty(e.Model, lastModel), e.ID
			r.Source, r.SourceDetail = SourceBackground, "branch_summary"
		}
		if u == nil {
			continue
		}
		r.Uncached, r.CacheRead, r.Output, r.Reasoning = u.Input, u.CacheRead, u.Output, u.Reasoning
		r.CacheWrite, r.CacheWrite1h = u.CacheWrite-u.CacheWrite1h, u.CacheWrite1h
		if u.Cost.Total > 0 {
			r.Cost = Cost{Uncached: u.Cost.Input, CacheRead: u.Cost.CacheRead, CacheWrite: u.Cost.CacheWrite, Output: u.Cost.Output, Basis: BasisRecorded}
			r.ReportedCostUSD = u.Cost.Total
		}
		if d.addRequest(r) && attributeCtx {
			d.attribute(r, s.context(e.ParentID))
		}
	}
}

// piItems turns one entry into the context items it adds.
func (d *Dataset) piItems(s *piSession, e *piEntry, calls map[string]piBlock) []*ContextItem {
	var out []*ContextItem
	add := func(kind, name, target string, chars, images int) {
		if chars+images == 0 {
			return
		}
		out = append(out, d.newItem(&ContextItem{
			Tool: Pi, SessionID: s.id, ThreadID: s.id, Project: s.project, Time: parseTime(e.Timestamp),
			Kind: kind, Name: name, Group: toolGroup(name), Target: target, EstTokens: estTokens(chars, images),
		}))
	}
	switch e.Type {
	case "compaction":
		add(KindCompaction, "compaction", "", len(e.Summary), 0)
	case "branch_summary":
		add(KindCompaction, "branch_summary", "", len(e.Summary), 0)
	case "custom_message":
		c, i := contentSize(e.Content)
		add(KindInjected, e.CustomType, "", c, i)
	case "message":
		m := e.Message
		if m == nil {
			return nil
		}
		switch m.Role {
		case "user":
			text := textOf(m.Content)
			kind := KindUserPrompt
			if reminderPrefix.MatchString(text) {
				kind = KindSystemReminder
			}
			c, i := contentSize(m.Content)
			add(kind, "", "", c, i)
		case "system":
			c, i := contentSize(m.Content)
			add(KindInstructions, "system", "", c, i)
		case "assistant":
			var blocks []piBlock
			_ = json.Unmarshal(m.Content, &blocks)
			for _, b := range blocks {
				switch b.Type {
				case "text":
					add(KindAssistantText, "", "", len(b.Text), 0)
				case "thinking":
					add(KindThinking, "", "", len(b.Thinking), 0)
				case "toolCall":
					calls[b.ID] = b
					add(KindToolCall, b.Name, toolTarget(b.Arguments), len(b.Arguments), 0)
				}
			}
		case "toolResult":
			call := calls[m.ToolCallID]
			name := firstNonEmpty(m.ToolName, call.Name, "unknown")
			c, i := contentSize(m.Content)
			add(KindToolResult, name, toolTarget(call.Arguments), c, i)
		case "bashExecution":
			if !m.ExcludeFromContext {
				add(KindToolResult, "user_bash", truncate(m.Command, 160), len(m.Command)+len(m.Output), 0)
			}
		case "custom":
			c, i := contentSize(m.Content)
			add(KindInjected, m.CustomType, "", c, i)
		case "compactionSummary", "branchSummary":
			add(KindCompaction, m.Role, "", len(m.Summary), 0)
		}
	}
	return out
}

// context walks from id to the root. At the latest compaction it keeps the
// summary plus the retained entries from firstKeptEntryId onward, then stops.
func (s *piSession) context(id string) []*ContextItem {
	var out []*ContextItem
	seen := map[string]bool{}
	stopAt := ""
	for id != "" && !seen[id] {
		seen[id] = true
		e := s.byID[id]
		if e == nil {
			break
		}
		if e.Type == "compaction" {
			if stopAt != "" {
				break // an older compaction is already summarized by the newer one
			}
			out = append(out, s.items[id]...)
			stopAt = e.FirstKeptEntryID
			if stopAt == "" || stopAt == e.ID {
				break // nothing retained
			}
			id = e.ParentID
			continue
		}
		out = append(out, s.items[id]...)
		if id == stopAt {
			break
		}
		id = e.ParentID
	}
	return out
}
