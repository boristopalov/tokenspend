package spend

import (
	"math"
	"sort"
)

// Context attribution.
//
// Each request re-reads its whole context, so its input cost is shared among
// the context items it carried. Item sizes are character-based estimates, and
// some context is never logged (system prompts, tool schemas), so each thread
// is fitted as
//
//	actual input tokens ≈ slope × estimated item tokens + intercept
//
// The slope corrects the estimate for the model's tokenizer; the intercept is
// context present in every request that the transcript doesn't contain. Each
// request's real input is then split in proportion to slope × item estimate,
// with the intercept going to the thread's "unattributed" item.

type pendingAttribution struct {
	r   *Request
	ctx []*ContextItem
	est float64
}

// Bounds for a believable tokens-per-estimated-token slope.
const (
	minSlope = 0.3
	maxSlope = 4.0
)

// attribute queues r for attribution once its whole thread has been read.
func (d *Dataset) attribute(r *Request, ctx []*ContextItem) {
	if r.InputTokens() <= 0 {
		return
	}
	var est float64
	for _, it := range ctx {
		est += float64(it.EstTokens)
	}
	key := r.Tool + "\x00" + r.ThreadID
	if _, ok := d.pending[key]; !ok {
		d.threadOrder = append(d.threadOrder, key)
	}
	d.pending[key] = append(d.pending[key], pendingAttribution{r: r, ctx: ctx, est: est})
}

type threadFit struct {
	slope, intercept float64
	ok               bool
}

// attributeAll fits every thread and distributes each request's input cost.
func (d *Dataset) attributeAll() {
	fits := map[string]threadFit{}
	slopes := map[string][]float64{} // per tool, for threads too short to fit
	for _, key := range d.threadOrder {
		f := fitThread(d.pending[key])
		fits[key] = f
		if f.ok {
			tool := d.pending[key][0].r.Tool
			slopes[tool] = append(slopes[tool], f.slope)
		}
	}
	for _, key := range d.threadOrder {
		list := d.pending[key]
		f := fits[key]
		if !f.ok {
			f = fallbackFit(list, median(slopes[list[0].r.Tool], 1))
		}
		for _, p := range list {
			d.distribute(p, f)
		}
	}
	d.pending, d.threadOrder = nil, nil
}

// fitThread is an ordinary least-squares fit of actual input on estimated
// size, with the intercept kept between zero and the smallest request.
func fitThread(list []pendingAttribution) threadFit {
	n := float64(len(list))
	if len(list) < 3 {
		return threadFit{}
	}
	var sx, sy, sxx, sxy, minY float64
	minY = math.Inf(1)
	for _, p := range list {
		x, y := p.est, float64(p.r.InputTokens())
		sx, sy, sxx, sxy = sx+x, sy+y, sxx+x*x, sxy+x*y
		minY = math.Min(minY, y)
	}
	varX := sxx/n - (sx/n)*(sx/n)
	if varX <= 0 || sxx == 0 {
		return threadFit{}
	}
	slope := (sxy/n - (sx/n)*(sy/n)) / varX
	intercept := sy/n - slope*sx/n
	if intercept < 0 || intercept > minY {
		// Refit with the intercept pinned to the nearest allowed value.
		intercept = math.Max(0, math.Min(intercept, minY))
		slope = (sxy - intercept*sx) / sxx
	}
	if slope < minSlope || slope > maxSlope {
		return threadFit{}
	}
	return threadFit{slope: slope, intercept: intercept, ok: true}
}

// fallbackFit uses the tool's typical slope and takes the intercept from the
// thread's own unexplained input.
func fallbackFit(list []pendingAttribution, slope float64) threadFit {
	var rest float64
	for _, p := range list {
		rest += float64(p.r.InputTokens()) - slope*p.est
	}
	return threadFit{slope: slope, intercept: math.Max(0, rest/float64(len(list))), ok: true}
}

func (d *Dataset) distribute(p pendingAttribution, f threadFit) {
	r := p.r
	actual := float64(r.InputTokens())
	weight := f.slope*p.est + f.intercept
	if weight <= 0 {
		weight, f.intercept = actual, actual // nothing logged: all of it is unattributed
	}
	scale := actual / weight
	inputCost := r.Cost.Input()
	for _, it := range p.ctx {
		if it.EstTokens == 0 {
			continue
		}
		t := float64(it.EstTokens) * f.slope * scale
		it.Requests++
		it.TokenTurns += t
		it.CostUSD += inputCost * t / actual
	}
	if rest := f.intercept * scale; rest > 0 {
		key := r.Tool + "\x00" + r.ThreadID
		u := d.unattributed[key]
		if u == nil {
			u = d.newItem(&ContextItem{
				Tool: r.Tool, SessionID: r.SessionID, ThreadID: r.ThreadID, Project: r.Project, Time: r.Time,
				Kind: KindUnattributed, Name: "never logged: system prompt, tool schemas, other", Group: "(unattributed)",
			})
			d.unattributed[key] = u
		}
		u.Requests++
		u.TokenTurns += rest
		u.CostUSD += inputCost * rest / actual
	}
}

func median(xs []float64, fallback float64) float64 {
	if len(xs) == 0 {
		return fallback
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return s[len(s)/2]
}
