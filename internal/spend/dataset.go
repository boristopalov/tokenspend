package spend

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Dataset is the normalized result of loading every log source.
type Dataset struct {
	Requests []*Request
	Items    []*ContextItem

	// CodexRateLimits are Codex plan-usage samples (ChatGPT plans bill by
	// window usage rather than per token).
	CodexRateLimits []RateLimitSample
	// ClaudeSessions compares Claude Code's own per-session cost totals
	// with what this tool computes from the session's transcripts.
	ClaudeSessions []ClaudeSessionCheck
	// Warnings lists data problems worth surfacing (unparseable lines, unpriced models).
	Warnings []string

	prices       *Prices
	since        time.Time
	seen         map[string]bool
	unattributed map[string]*ContextItem
	pending      map[string][]pendingAttribution // thread key -> requests awaiting attribution
	threadOrder  []string
	badLines     map[string]int
	ccCostStates map[string]*ccCostState
	ccShapes     map[string]*ccShape
}

// RateLimitSample is one Codex rate-limit reading.
type RateLimitSample struct {
	Time             time.Time `json:"ts"`
	Plan             string    `json:"plan"`
	PrimaryPercent   float64   `json:"primary_pct"`
	PrimaryWindow    int       `json:"primary_window_min"`
	SecondaryPercent float64   `json:"secondary_pct"`
	SecondaryWindow  int       `json:"secondary_window_min"`
}

// ClaudeSessionCheck reconciles one Claude Code session.
type ClaudeSessionCheck struct {
	SessionID     string  `json:"session"`
	Project       string  `json:"project"`
	ReportedUSD   float64 `json:"reported_usd"`   // Claude Code's cost-state total
	TranscriptUSD float64 `json:"transcript_usd"` // computed from transcript requests
	OTelOnlyUSD   float64 `json:"otel_only_usd"`  // computed from OTel requests with no transcript record
	UnloggedUSD   float64 `json:"unlogged_usd"`   // computed from the token gap that remains
}

// PriceRows returns the price table the dataset was priced with.
func (d *Dataset) PriceRows() []ModelPrice { return d.prices.Rows }

func newDataset(prices *Prices, since time.Time) *Dataset {
	return &Dataset{
		prices:       prices,
		since:        since,
		seen:         map[string]bool{},
		unattributed: map[string]*ContextItem{},
		pending:      map[string][]pendingAttribution{},
		badLines:     map[string]int{},
		ccCostStates: map[string]*ccCostState{},
		ccShapes:     map[string]*ccShape{},
	}
}

// addRequest prices r and adds it unless it duplicates an earlier request or
// falls before the time filter. It reports whether r was added; callers only
// attribute context for added requests.
func (d *Dataset) addRequest(r *Request) bool {
	if r.TotalTokens() == 0 {
		return false
	}
	if r.RequestID != "" {
		key := r.Tool + "\x00" + r.RequestID
		if d.seen[key] {
			return false
		}
		d.seen[key] = true
	}
	if !d.since.IsZero() && r.Time.Before(d.since) {
		return false
	}
	d.prices.Apply(r)
	d.Requests = append(d.Requests, r)
	return true
}

// newItem registers a context item.
func (d *Dataset) newItem(it *ContextItem) *ContextItem {
	d.Items = append(d.Items, it)
	return it
}

func (d *Dataset) badLine(path string) { d.badLines[path]++ }

const maxLine = 1 << 30

// lineReader holds the buffers eachLine reuses across files. Lines in
// agent logs reach several MB, and growing a fresh buffer for every file
// was a large share of load time.
type lineReader struct {
	r    *bufio.Reader
	long []byte // assembles lines longer than r's buffer
}

var lineReaders = sync.Pool{New: func() any { return &lineReader{r: bufio.NewReaderSize(nil, 1<<20)} }}

// eachLine calls fn for every non-empty line of a JSONL file, gunzipping
// files named *.gz. The line is only valid during the call.
func eachLine(path string, fn func(line []byte)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	lr := lineReaders.Get().(*lineReader)
	defer func() {
		lr.r.Reset(nil)
		if cap(lr.long) > 64<<20 {
			lr.long = nil // don't keep a rare huge line's buffer alive
		}
		lineReaders.Put(lr)
	}()
	var src io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		src = gz
	}
	lr.r.Reset(src)
	for {
		line, err := lr.r.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			lr.long = append(lr.long[:0], line...)
			for err == bufio.ErrBufferFull {
				if len(lr.long) > maxLine {
					return fmt.Errorf("line longer than %d bytes", maxLine)
				}
				line, err = lr.r.ReadSlice('\n')
				lr.long = append(lr.long, line...)
			}
			line = lr.long
		}
		if line := bytes.TrimSpace(line); len(line) > 0 {
			fn(line)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// Token estimates. Real tokenizers vary; attribution only uses these for
// proportions and rescales them to the request's actual input.
const (
	charsPerToken = 4
	imageTokens   = 1600
)

func estTokens(chars, images int) int64 {
	return int64((chars+charsPerToken-1)/charsPerToken + images*imageTokens)
}

// contentSize measures message content in any of the shapes the three tools
// use: a plain string, a block, or an array of blocks (text, image, tool
// result). Unknown shapes count their raw JSON length.
func contentSize(raw json.RawMessage) (chars, images int) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return 0, 0
	}
	switch raw[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return len(raw), 0
		}
		// Codex stores some tool outputs as a JSON-encoded block array.
		if t := strings.TrimSpace(s); len(t) > 1 && (t[0] == '[' || t[0] == '{') && json.Valid([]byte(t)) {
			return contentSize(json.RawMessage(t))
		}
		return len(s), 0
	case '[':
		var arr []json.RawMessage
		if json.Unmarshal(raw, &arr) != nil {
			return len(raw), 0
		}
		for _, b := range arr {
			c, i := blockSize(b)
			chars += c
			images += i
		}
		return chars, images
	case '{':
		return blockSize(raw)
	}
	return len(raw), 0
}

func blockSize(raw json.RawMessage) (int, int) {
	var b struct {
		Type    string          `json:"type"`
		Text    *string         `json:"text"`
		Content json.RawMessage `json:"content"`
		Output  json.RawMessage `json:"output"`
	}
	if json.Unmarshal(raw, &b) != nil {
		return len(raw), 0
	}
	switch {
	case strings.Contains(b.Type, "image"):
		return 0, 1
	case b.Text != nil:
		return len(*b.Text), 0
	case len(b.Content) > 0:
		return contentSize(b.Content)
	case len(b.Output) > 0:
		return contentSize(b.Output)
	}
	return len(raw), 0
}

// textOf returns the concatenated text of string or block-array content.
func textOf(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range blocks {
		sb.WriteString(b.Text)
	}
	return sb.String()
}

// toolGroup groups MCP tools by server: "mcp__github__get_issue" -> "mcp:github".
func toolGroup(name string) string {
	if rest, ok := strings.CutPrefix(name, "mcp__"); ok {
		server, _, _ := strings.Cut(rest, "__")
		return "mcp:" + server
	}
	return name
}

// toolTarget picks the most identifying argument of a tool call: a file path,
// URL or command.
func toolTarget(input json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		var s string
		if json.Unmarshal(input, &s) == nil {
			// Arguments encoded as a JSON string (Codex function calls).
			if json.Unmarshal([]byte(s), &m) != nil {
				return ""
			}
		}
	}
	for _, k := range []string{"file_path", "path", "notebook_path", "filePath", "url", "command", "cmd", "pattern", "query", "description"} {
		switch v := m[k].(type) {
		case string:
			if v != "" {
				return truncate(v, 160)
			}
		case []any: // Codex shell: ["bash", "-lc", "..."]
			if len(v) > 0 {
				if s, ok := v[len(v)-1].(string); ok {
					return truncate(s, 160)
				}
			}
		}
	}
	return ""
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8Boundary(s, cut) {
		cut--
	}
	return s[:cut] + "…"
}

func utf8Boundary(s string, i int) bool { return i >= len(s) || s[i]&0xC0 != 0x80 }

var reminderPrefix = regexp.MustCompile(`^\s*<(system-reminder|environment_context|user_instructions|permissions|skills_instructions|local-command|command-)`)
