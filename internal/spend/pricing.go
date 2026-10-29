package spend

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

//go:embed prices.json
var defaultPrices []byte

// ModelPrice is one price table row, in USD per million tokens.
type ModelPrice struct {
	Model        string   `json:"model"`
	Input        float64  `json:"input"`
	Output       float64  `json:"output"`
	CacheRead    float64  `json:"cache_read"`
	CacheWrite   *float64 `json:"cache_write,omitempty"`
	CacheWrite1h *float64 `json:"cache_write_1h,omitempty"`

	LongThreshold int64   `json:"long_context_threshold,omitempty"`
	LongInput     float64 `json:"long_input,omitempty"`
	LongOutput    float64 `json:"long_output,omitempty"`
	LongCacheRead float64 `json:"long_cache_read,omitempty"`

	Assumed bool   `json:"assumed,omitempty"`
	Note    string `json:"note,omitempty"`
}

// Prices is a price table keyed by normalized model ID.
type Prices struct {
	byModel map[string]ModelPrice
	Rows    []ModelPrice
}

// LoadPrices reads a price table from path, or the embedded default when path is empty.
func LoadPrices(path string) (*Prices, error) {
	data := defaultPrices
	if path != "" {
		var err error
		if data, err = os.ReadFile(path); err != nil {
			return nil, err
		}
	}
	var file struct {
		Models []ModelPrice `json:"models"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse price table: %w", err)
	}
	p := &Prices{byModel: map[string]ModelPrice{}, Rows: file.Models}
	for _, m := range file.Models {
		p.byModel[NormalizeModel(m.Model)] = m
	}
	return p, nil
}

// DefaultPricesJSON returns the embedded price table, for users who want to copy and edit it.
func DefaultPricesJSON() []byte { return defaultPrices }

var dateSuffix = regexp.MustCompile(`-\d{8}$`)

// NormalizeModel maps provider-specific spellings onto price table keys:
// "anthropic/claude-opus-4.8" and "claude-opus-4-8-20260101[1m]" both become "claude-opus-4-8".
func NormalizeModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.IndexByte(m, '['); i >= 0 {
		m = m[:i]
	}
	if i := strings.LastIndexByte(m, '/'); i >= 0 {
		m = m[i+1:]
	}
	m = strings.TrimPrefix(m, "anthropic.")
	if i := strings.IndexByte(m, '@'); i >= 0 {
		m = m[:i]
	}
	m = dateSuffix.ReplaceAllString(m, "")
	if strings.HasPrefix(m, "claude-") {
		m = strings.ReplaceAll(m, ".", "-")
	}
	return m
}

// Lookup returns the price row for a model.
func (p *Prices) Lookup(model string) (ModelPrice, bool) {
	m, ok := p.byModel[NormalizeModel(model)]
	return m, ok
}

// Apply sets r.Cost from the price table. Requests that already carry a
// recorded cost keep it; Apply only fills in requests without one.
func (p *Prices) Apply(r *Request) {
	if r.Cost.Basis == BasisRecorded {
		return
	}
	m, ok := p.Lookup(r.Model)
	if !ok {
		r.Cost = Cost{Basis: BasisUnpriced}
		return
	}
	in, out, read := m.Input, m.Output, m.CacheRead
	write := in
	if strings.HasPrefix(NormalizeModel(m.Model), "claude-") {
		write = 1.25 * in
	}
	if m.CacheWrite != nil {
		write = *m.CacheWrite
	}
	write1h := 2 * in
	if m.CacheWrite1h != nil {
		write1h = *m.CacheWrite1h
	}
	if m.LongThreshold > 0 && r.InputTokens() > m.LongThreshold {
		scale := m.LongInput / in
		in, out, read = m.LongInput, m.LongOutput, m.LongCacheRead
		write, write1h = write*scale, write1h*scale
	}
	const perTok = 1e-6
	r.Cost = Cost{
		Uncached:   float64(r.Uncached) * in * perTok,
		CacheRead:  float64(r.CacheRead) * read * perTok,
		CacheWrite: (float64(r.CacheWrite)*write + float64(r.CacheWrite1h)*write1h) * perTok,
		Output:     float64(r.Output) * out * perTok,
		Basis:      BasisTable,
	}
	if m.Assumed {
		r.Cost.Basis = BasisAssumed
	}
}
