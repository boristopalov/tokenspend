package spend

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// OTelRecord is one flattened OTLP log record or metric data point, as the
// receiver stores it, one JSON object per line (see OTelFile).
type OTelRecord struct {
	Time     time.Time      `json:"ts"`
	Signal   string         `json:"signal"` // "log" or "metric"
	Service  string         `json:"service"`
	Name     string         `json:"name"` // event name or metric name
	Attrs    map[string]any `json:"attrs,omitempty"`
	Resource map[string]any `json:"resource,omitempty"`
	Body     string         `json:"body,omitempty"`
	Value    *float64       `json:"value,omitempty"` // metrics only
}

// loadOTel adds requests from OTel exports that no transcript recorded, and
// fills in sources the transcript lacked (e.g. Claude Code subagent query_source).
func (d *Dataset) loadOTel(dir string) error {
	byID := map[string]*Request{}
	byTokens := map[string]*Request{}
	for _, r := range d.Requests {
		if r.RequestID != "" {
			byID[r.Tool+"\x00"+r.RequestID] = r
		}
		byTokens[tokenKey(r)] = r
	}
	bad, err := ReadOTelFiles(OTelFiles(dir, true), func(rec *OTelRecord) {
		r := otelRequest(rec)
		if r == nil {
			return
		}
		match := byID[r.Tool+"\x00"+r.RequestID]
		if match == nil || r.RequestID == "" {
			match = byTokens[tokenKey(r)]
		}
		if match != nil {
			// Claude Code's query_source is more specific than what the transcript says.
			if match.Tool == ClaudeCode && r.SourceDetail != "unknown" {
				match.Source, match.SourceDetail = r.Source, r.SourceDetail
			}
			if match.ReportedCostUSD == 0 {
				match.ReportedCostUSD = r.ReportedCostUSD
			}
			return
		}
		if d.addRequest(r) {
			byTokens[tokenKey(r)] = r
			if r.RequestID != "" {
				byID[r.Tool+"\x00"+r.RequestID] = r
			}
		}
	})
	for f, n := range bad {
		d.badLines[f] += n
	}
	return err
}

// The receiver stores records in one file per kind and day:
//
//	usage-YYYY-MM-DD.jsonl    billed request events (IsOTelRequest), kept forever
//	events-YYYY-MM-DD.jsonl   everything else, deleted after a retention period
//	otel-YYYY-MM-DD.jsonl     both kinds, from receivers before the split
//
// Days before today are gzipped (".jsonl.gz").
const (
	OTelUsage  = "usage"
	OTelEvents = "events"
	OTelLegacy = "otel"
)

// OTelFile is one of the receiver's data files.
type OTelFile struct {
	Path string
	Kind string // OTelUsage, OTelEvents or OTelLegacy
	Day  string // YYYY-MM-DD, local time of receipt
	Gzip bool
}

var otelFileName = regexp.MustCompile(`^(usage|events|otel)-(\d{4}-\d{2}-\d{2})\.jsonl(\.gz)?$`)

// OTelFileName is the name of the plain file for kind and day.
func OTelFileName(kind, day string) string { return kind + "-" + day + ".jsonl" }

// OTelFiles lists the receiver's data files in dir, oldest day first. With
// usageOnly it skips events files, which never hold billed requests. A
// missing directory has no files.
func OTelFiles(dir string, usageOnly bool) []OTelFile {
	entries, _ := os.ReadDir(dir)
	var files []OTelFile
	for _, e := range entries {
		m := otelFileName.FindStringSubmatch(e.Name())
		if m == nil || e.IsDir() || (usageOnly && m[1] == OTelEvents) {
			continue
		}
		files = append(files, OTelFile{Path: filepath.Join(dir, e.Name()), Kind: m[1], Day: m[2], Gzip: m[3] != ""})
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].Day != files[j].Day {
			return files[i].Day < files[j].Day
		}
		return files[i].Path < files[j].Path
	})
	return files
}

// ReadOTelFiles calls fn for every record in files, in order. It returns
// how many lines in each file were not valid records.
func ReadOTelFiles(files []OTelFile, fn func(*OTelRecord)) (badLines map[string]int, err error) {
	badLines = map[string]int{}
	for _, file := range files {
		f := file.Path
		err := eachLine(f, func(raw []byte) {
			var rec OTelRecord
			if json.Unmarshal(raw, &rec) != nil {
				badLines[f]++
				return
			}
			fn(&rec)
		})
		if err != nil {
			return badLines, fmt.Errorf("%s: %w", f, err)
		}
	}
	return badLines, nil
}

// OTelTool names the tool that sent rec (ClaudeCode, Codex or Pi), or "" if
// it came from something else.
func OTelTool(rec *OTelRecord) string {
	if r := otelRequest(rec); r != nil {
		return r.Tool
	}
	switch s := strings.ToLower(rec.Service); {
	case strings.Contains(s, "claude"):
		return ClaudeCode
	case strings.Contains(s, "codex"):
		return Codex
	case s == "pi":
		return Pi
	}
	return ""
}

// IsOTelRequest reports whether rec is a billed model request that the
// report counts.
func IsOTelRequest(rec *OTelRecord) bool { return otelRequest(rec) != nil }

func tokenKey(r *Request) string {
	return fmt.Sprintf("%s|%s|%d|%d|%d", r.Tool, r.SessionID, r.Uncached+r.CacheWrite+r.CacheWrite1h, r.CacheRead, r.Output)
}

// otelRequest converts an api-request log event from any of the three tools.
func otelRequest(rec *OTelRecord) *Request {
	if rec.Signal != "log" {
		return nil
	}
	a := rec.Attrs
	name := rec.Name
	if ev := attrString(a, "event.name"); strings.Contains(ev, ".") && !strings.Contains(ev, " ") {
		name = ev // the attribute is authoritative when it is namespaced (Codex)
	}
	r := &Request{Time: rec.Time, Origin: OriginOTel, Model: attrString(a, "model")}
	switch {
	case strings.HasSuffix(name, "api_request") && (strings.Contains(rec.Service, "claude") || strings.HasPrefix(name, "claude_code")):
		r.Tool = ClaudeCode
		r.SessionID = attrString(a, "session.id")
		r.RequestID = attrString(a, "request_id")
		r.Uncached = attrInt(a, "input_tokens")
		r.CacheRead = attrInt(a, "cache_read_tokens")
		r.CacheWrite = attrInt(a, "cache_creation_5m_tokens")
		r.CacheWrite1h = attrInt(a, "cache_creation_1h_tokens")
		if total := attrInt(a, "cache_creation_tokens"); r.CacheWrite+r.CacheWrite1h != total {
			r.CacheWrite, r.CacheWrite1h = total, 0
		}
		r.Output = attrInt(a, "output_tokens")
		r.ReportedCostUSD = attrFloat(a, "cost_usd")
		r.SourceDetail = attrString(a, "query_source")
		r.Source = ClassifyClaudeQuerySource(r.SourceDetail)
		// query_source can be a bare subagent name; agent.name marks subagent calls.
		if agent := attrString(a, "agent.name"); agent != "" && r.Source != SourceCompaction {
			r.Source = SourceSubagent
		}
		if r.SourceDetail == "" {
			r.SourceDetail = "unknown"
		}
	case strings.HasPrefix(name, "codex.") && a["input_token_count"] != nil:
		r.Tool = Codex
		r.SessionID = firstNonEmpty(attrString(a, "conversation.id"), attrString(a, "thread_id"), attrString(a, "session_id"))
		input, cached, written := attrInt(a, "input_token_count"), attrInt(a, "cached_token_count"), attrInt(a, "cache_write_token_count")
		r.Uncached, r.CacheRead, r.CacheWrite = max(0, input-cached-written), cached, written
		r.Output = attrInt(a, "output_token_count")
		r.Reasoning = attrInt(a, "reasoning_token_count")
		r.Source, r.SourceDetail = SourceMain, firstNonEmpty(attrString(a, "session_source"), attrString(a, "originator"), "otel")
		if r.Model == "codex-auto-review" { // guardian threads use this model; OTel doesn't say which thread kind it is
			r.Source, r.SourceDetail = SourceReview, "guardian auto-review"
		}
	case name == "pi.api_request":
		r.Tool = Pi
		r.SessionID = attrString(a, "session.id")
		r.RequestID = attrString(a, "response_id")
		r.Uncached = attrInt(a, "input_tokens")
		r.CacheRead = attrInt(a, "cache_read_tokens")
		r.CacheWrite1h = attrInt(a, "cache_creation_1h_tokens")
		r.CacheWrite = attrInt(a, "cache_creation_tokens") - r.CacheWrite1h
		r.Output = attrInt(a, "output_tokens")
		r.Reasoning = attrInt(a, "reasoning_tokens")
		r.SourceDetail = firstNonEmpty(attrString(a, "source"), "agent")
		r.Source = SourceMain
		if strings.HasPrefix(r.SourceDetail, "tool:") {
			r.Source = SourceSubagent
		}
		if c := attrFloat(a, "cost_usd"); c > 0 {
			r.ReportedCostUSD = c
			r.Cost = Cost{
				Uncached: attrFloat(a, "cost.input"), CacheRead: attrFloat(a, "cost.cache_read"),
				CacheWrite: attrFloat(a, "cost.cache_write"), Output: attrFloat(a, "cost.output"), Basis: BasisRecorded,
			}
		}
	default:
		return nil
	}
	r.ThreadID = r.SessionID
	r.Project = firstNonEmpty(attrString(a, "cwd"), attrString(rec.Resource, "process.cwd"))
	return r
}

func attrString(m map[string]any, k string) string {
	switch v := m[k].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func attrFloat(m map[string]any, k string) float64 {
	switch v := m[k].(type) {
	case float64:
		return v
	case string:
		f, _ := strconv.ParseFloat(v, 64)
		return f
	}
	return 0
}

func attrInt(m map[string]any, k string) int64 { return int64(attrFloat(m, k)) }

// DefaultOTelDir is where the receiver writes and the report reads OTel data.
func DefaultOTelDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".tokenspend", "otel")
}
