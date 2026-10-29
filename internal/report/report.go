// Package report renders a Dataset as a self-contained HTML dashboard.
package report

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"io"
	"sort"
	"time"

	"tokenspend/internal/spend"
)

//go:embed dashboard.html
var dashboardHTML []byte

var placeholder = []byte("/*__DATA__*/null")

// Write renders the dashboard for d.
func Write(w io.Writer, d *spend.Dataset, now time.Time) error {
	data, err := json.Marshal(buildPayload(d, now)) // escapes <, > and & so it is safe inside <script>
	if err != nil {
		return err
	}
	_, err = w.Write(bytes.Replace(dashboardHTML, placeholder, data, 1))
	return err
}

// payload is everything the dashboard's script needs. Repeated strings are
// dictionary-encoded and requests are stored column-wise to keep the file small.
type payload struct {
	Generated time.Time `json:"generated"`
	Strings   []string  `json:"strings"`

	Requests requestColumns `json:"requests"`

	// Context attribution, rolled up by day so the date filter still applies.
	AttribGroups  []attribRow `json:"attrib_groups"`
	AttribTargets []attribRow `json:"attrib_targets"`
	TopItems      []topItem   `json:"top_items"`

	ClaudeSessions  []spend.ClaudeSessionCheck `json:"claude_sessions"`
	CodexRateLimits []spend.RateLimitSample    `json:"codex_rate_limits"`
	Prices          []spend.ModelPrice         `json:"prices"`
	Warnings        []string                   `json:"warnings"`
}

type requestColumns struct {
	Time    []int64   `json:"t"` // unix seconds
	Tool    []int     `json:"tool"`
	Session []int     `json:"session"`
	Project []int     `json:"project"`
	Model   []int     `json:"model"`
	Source  []int     `json:"source"`
	Detail  []int     `json:"detail"`
	Origin  []int     `json:"origin"`
	Basis   []int     `json:"basis"`
	Unc     []int64   `json:"unc"`
	Read    []int64   `json:"read"`
	Write5  []int64   `json:"w5"`
	Write1h []int64   `json:"w1h"`
	Out     []int64   `json:"out"`
	CUnc    []float64 `json:"c_unc"`
	CRead   []float64 `json:"c_read"`
	CWrite  []float64 `json:"c_write"`
	COut    []float64 `json:"c_out"`
}

// attribRow aggregates context items sharing tool, day, kind, group (and target).
type attribRow struct {
	Tool       int     `json:"tool"`
	Day        string  `json:"day"`
	Kind       int     `json:"kind"`
	Group      int     `json:"group"`
	Target     int     `json:"target,omitempty"`
	Items      int     `json:"items"`
	EstTokens  int64   `json:"est"`
	TokenTurns float64 `json:"tt"`
	CostUSD    float64 `json:"cost"`
}

type topItem struct {
	Tool       int     `json:"tool"`
	Day        string  `json:"day"`
	Session    int     `json:"session"`
	Project    int     `json:"project"`
	Kind       int     `json:"kind"`
	Group      int     `json:"group"`
	Target     int     `json:"target"`
	EstTokens  int64   `json:"est"`
	Requests   int     `json:"requests"`
	TokenTurns float64 `json:"tt"`
	CostUSD    float64 `json:"cost"`
}

type dict struct {
	idx  map[string]int
	list []string
}

func (d *dict) id(s string) int {
	if i, ok := d.idx[s]; ok {
		return i
	}
	d.idx[s] = len(d.list)
	d.list = append(d.list, s)
	return len(d.list) - 1
}

const (
	maxTargets  = 3000
	maxTopItems = 400
)

func buildPayload(d *spend.Dataset, now time.Time) payload {
	s := &dict{idx: map[string]int{}}
	s.id("") // index 0 is the empty string
	p := payload{
		Generated:       now,
		ClaudeSessions:  d.ClaudeSessions,
		CodexRateLimits: thinRateLimits(d.CodexRateLimits),
		Warnings:        d.Warnings,
		Prices:          d.PriceRows(),
	}
	c := &p.Requests
	for _, r := range d.Requests {
		c.Time = append(c.Time, r.Time.Unix())
		c.Tool = append(c.Tool, s.id(r.Tool))
		c.Session = append(c.Session, s.id(r.SessionID))
		c.Project = append(c.Project, s.id(r.Project))
		c.Model = append(c.Model, s.id(r.Model))
		c.Source = append(c.Source, s.id(r.Source))
		c.Detail = append(c.Detail, s.id(r.SourceDetail))
		c.Origin = append(c.Origin, s.id(r.Origin))
		c.Basis = append(c.Basis, s.id(r.Cost.Basis))
		c.Unc = append(c.Unc, r.Uncached)
		c.Read = append(c.Read, r.CacheRead)
		c.Write5 = append(c.Write5, r.CacheWrite)
		c.Write1h = append(c.Write1h, r.CacheWrite1h)
		c.Out = append(c.Out, r.Output)
		c.CUnc = append(c.CUnc, round6(r.Cost.Uncached))
		c.CRead = append(c.CRead, round6(r.Cost.CacheRead))
		c.CWrite = append(c.CWrite, round6(r.Cost.CacheWrite))
		c.COut = append(c.COut, round6(r.Cost.Output))
	}

	groups := map[[4]string]*attribRow{}
	targets := map[[5]string]*attribRow{}
	var items []*spend.ContextItem
	for _, it := range d.Items {
		if it.Requests == 0 {
			continue
		}
		items = append(items, it)
		day := it.Time.Local().Format("2006-01-02")
		gk := [4]string{it.Tool, day, it.Kind, groupLabel(it)}
		g := groups[gk]
		if g == nil {
			g = &attribRow{Tool: s.id(it.Tool), Day: day, Kind: s.id(it.Kind), Group: s.id(gk[3])}
			groups[gk] = g
		}
		g.add(it)
		if it.Target != "" {
			tk := [5]string{it.Tool, day, it.Kind, gk[3], it.Target}
			t := targets[tk]
			if t == nil {
				t = &attribRow{Tool: g.Tool, Day: day, Kind: g.Kind, Group: g.Group, Target: s.id(it.Target)}
				targets[tk] = t
			}
			t.add(it)
		}
	}
	for _, g := range groups {
		p.AttribGroups = append(p.AttribGroups, *g)
	}
	sort.Slice(p.AttribGroups, func(i, j int) bool { return p.AttribGroups[i].CostUSD > p.AttribGroups[j].CostUSD })
	for _, t := range targets {
		p.AttribTargets = append(p.AttribTargets, *t)
	}
	sort.Slice(p.AttribTargets, func(i, j int) bool { return p.AttribTargets[i].CostUSD > p.AttribTargets[j].CostUSD })
	if len(p.AttribTargets) > maxTargets {
		p.AttribTargets = p.AttribTargets[:maxTargets]
	}
	for i := range p.AttribGroups {
		p.AttribGroups[i].round()
	}
	for i := range p.AttribTargets {
		p.AttribTargets[i].round()
	}

	sort.Slice(items, func(i, j int) bool { return items[i].CostUSD > items[j].CostUSD })
	for _, it := range items {
		if len(p.TopItems) == maxTopItems {
			break
		}
		if it.Kind == spend.KindUnattributed {
			continue // shown in the group table; a per-thread remainder isn't one item
		}
		p.TopItems = append(p.TopItems, topItem{
			Tool: s.id(it.Tool), Day: it.Time.Local().Format("2006-01-02"), Session: s.id(it.SessionID), Project: s.id(it.Project),
			Kind: s.id(it.Kind), Group: s.id(groupLabel(it)), Target: s.id(it.Target), EstTokens: it.EstTokens,
			Requests: it.Requests, TokenTurns: round6(it.TokenTurns), CostUSD: round6(it.CostUSD),
		})
	}
	p.Strings = s.list
	return p
}

// groupLabel is what the attribution tables group by: the tool or MCP
// server for tool traffic, the attachment type for attachments.
func groupLabel(it *spend.ContextItem) string {
	if it.Group != "" {
		return it.Group
	}
	return "(" + it.Kind + ")"
}

func (a *attribRow) add(it *spend.ContextItem) {
	a.Items++
	a.EstTokens += it.EstTokens
	a.TokenTurns += it.TokenTurns
	a.CostUSD += it.CostUSD
}

func (a *attribRow) round() {
	a.TokenTurns = float64(int64(a.TokenTurns))
	a.CostUSD = round6(a.CostUSD)
}

func round6(f float64) float64 { return float64(int64(f*1e6+0.5)) / 1e6 }

// thinRateLimits keeps the last sample per hour; Codex logs one per response.
func thinRateLimits(in []spend.RateLimitSample) []spend.RateLimitSample {
	sorted := append([]spend.RateLimitSample(nil), in...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Time.Before(sorted[j].Time) })
	var out []spend.RateLimitSample
	for i, r := range sorted {
		if i+1 < len(sorted) && sorted[i+1].Time.Truncate(time.Hour).Equal(r.Time.Truncate(time.Hour)) {
			continue
		}
		out = append(out, r)
	}
	return out
}
