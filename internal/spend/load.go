package spend

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"
)

// Options says where to read logs from. Empty lists use DefaultOptions.
type Options struct {
	ClaudeProjectDirs []string
	CodexSessionDirs  []string
	PiSessionDirs     []string
	OTelDir           string
	Prices            *Prices
	Since             time.Time // ignore requests before this time; zero = everything

	// Workers is how many log files are parsed at once; 0 means one per CPU.
	Workers int
	// Progress, if set, is called as loading advances, from one goroutine at
	// a time.
	Progress func(Progress)
}

// Load stages, in order.
const (
	StageListing     = "listing"     // finding log files
	StageReading     = "reading"     // parsing transcripts
	StageOTel        = "otel"        // merging OTel exports
	StageAttributing = "attributing" // splitting input cost across context items
)

// Progress reports how far Load has got. Files and Bytes count transcript
// units (a Claude Code session with its subagents, a Codex rollout, a Pi
// session) during StageReading.
type Progress struct {
	Stage             string
	Files, TotalFiles int
	Bytes, TotalBytes int64
}

// DefaultOptions returns the standard log locations, honoring CLAUDE_CONFIG_DIR,
// CODEX_HOME and PI_CODING_AGENT_DIR.
func DefaultOptions() Options {
	home, _ := os.UserHomeDir()
	claude := []string{filepath.Join(home, ".claude", "projects")}
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		claude = append(claude, filepath.Join(dir, "projects"))
	}
	codexHome := filepath.Join(home, ".codex")
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		codexHome = dir
	}
	piDir := filepath.Join(home, ".pi", "agent")
	if dir := os.Getenv("PI_CODING_AGENT_DIR"); dir != "" {
		piDir = dir
	}
	return Options{
		ClaudeProjectDirs: claude,
		CodexSessionDirs:  []string{filepath.Join(codexHome, "sessions"), filepath.Join(codexHome, "archived_sessions")},
		PiSessionDirs:     []string{filepath.Join(piDir, "sessions")},
		OTelDir:           DefaultOTelDir(),
	}
}

// Load reads every configured source into one Dataset.
func Load(opts Options) (*Dataset, error) {
	if opts.Prices == nil {
		p, err := LoadPrices("")
		if err != nil {
			return nil, err
		}
		opts.Prices = p
	}
	progress := opts.Progress
	if progress == nil {
		progress = func(Progress) {}
	}
	d := newDataset(opts.Prices, opts.Since)

	progress(Progress{Stage: StageListing})
	var units []loadUnit
	sources := []struct {
		name  string
		dirs  []string
		units func(string) ([]loadUnit, error)
	}{
		{"claude code logs", opts.ClaudeProjectDirs, claudeUnits},
		{"codex logs", opts.CodexSessionDirs, codexUnits},
		{"pi logs", opts.PiSessionDirs, piUnits},
	}
	for _, src := range sources {
		for _, dir := range uniqueDirs(src.dirs) {
			u, err := src.units(dir)
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", src.name, dir, err)
			}
			units = append(units, u...)
		}
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	d.loadUnits(units, workers, progress)

	if opts.OTelDir != "" {
		progress(Progress{Stage: StageOTel})
		if err := d.loadOTel(opts.OTelDir); err != nil {
			return nil, fmt.Errorf("otel data %s: %w", opts.OTelDir, err)
		}
	}
	progress(Progress{Stage: StageAttributing})
	d.attributeAll()
	// After OTel, so requests only OTel saw count as explained.
	d.reconcileClaude()

	sort.SliceStable(d.Requests, func(i, j int) bool { return d.Requests[i].Time.Before(d.Requests[j].Time) })
	for path, n := range d.badLines {
		d.Warnings = append(d.Warnings, fmt.Sprintf("%s: skipped %d unparseable lines", path, n))
	}
	unpriced := map[string]bool{}
	for _, r := range d.Requests {
		if r.Cost.Basis == BasisUnpriced && !unpriced[r.Model] {
			unpriced[r.Model] = true
			d.Warnings = append(d.Warnings, fmt.Sprintf("model %q has no price table entry; its cost shows as $0", r.Model))
		}
	}
	sort.Strings(d.Warnings)
	return d, nil
}

// loadUnit is one independently loadable piece of a log source. Each unit
// is read into its own Dataset so units can be parsed in parallel; the
// results are merged in unit order, so the outcome matches a serial load.
type loadUnit struct {
	size int64 // bytes on disk, for progress
	load func(*Dataset)
}

func (d *Dataset) loadUnits(units []loadUnit, workers int, progress func(Progress)) {
	p := Progress{Stage: StageReading, TotalFiles: len(units)}
	for _, u := range units {
		p.TotalBytes += u.size
	}
	progress(p)

	shards := make([]*Dataset, len(units))
	todo := make(chan int)
	done := make(chan int)
	go func() {
		for i := range units {
			todo <- i
		}
		close(todo)
	}()
	for range min(workers, len(units)) {
		go func() {
			for i := range todo {
				s := newDataset(d.prices, d.since)
				units[i].load(s)
				shards[i] = s
				done <- i
			}
		}()
	}
	finished := make([]bool, len(units))
	next := 0 // first shard not merged yet
	for range units {
		i := <-done
		finished[i] = true
		p.Files++
		p.Bytes += units[i].size
		progress(p)
		for next < len(units) && finished[next] {
			d.merge(shards[next])
			shards[next] = nil
			next++
		}
	}
}

// merge appends a shard loaded by loadUnits. A request that duplicates one
// from an earlier shard is dropped along with its pending attribution, just
// as addRequest would have refused it in a serial load.
func (d *Dataset) merge(s *Dataset) {
	dropped := map[*Request]bool{}
	for _, r := range s.Requests {
		if r.RequestID != "" {
			key := r.Tool + "\x00" + r.RequestID
			if d.seen[key] {
				dropped[r] = true
				continue
			}
			d.seen[key] = true
		}
		d.Requests = append(d.Requests, r)
	}
	d.Items = append(d.Items, s.Items...)
	for _, key := range s.threadOrder {
		for _, pa := range s.pending[key] {
			if dropped[pa.r] {
				continue
			}
			if _, ok := d.pending[key]; !ok {
				d.threadOrder = append(d.threadOrder, key)
			}
			d.pending[key] = append(d.pending[key], pa)
		}
	}
	d.CodexRateLimits = append(d.CodexRateLimits, s.CodexRateLimits...)
	for path, n := range s.badLines {
		d.badLines[path] += n
	}
	for id, cs := range s.ccCostStates {
		if prev := d.ccCostStates[id]; prev == nil || prev.totalCostUSD < cs.totalCostUSD {
			d.ccCostStates[id] = cs
		}
	}
}

// fileSizes is the total size of the files that exist among paths.
func fileSizes(paths ...string) int64 {
	var n int64
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil {
			n += fi.Size()
		}
	}
	return n
}

// uniqueDirs drops missing directories and duplicates (including symlinked
// copies, such as per-account Claude config dirs that link to ~/.claude).
func uniqueDirs(dirs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, dir := range dirs {
		real, err := filepath.EvalSymlinks(dir)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				out = append(out, dir) // let the loader report the error
			}
			continue
		}
		if !seen[real] {
			seen[real] = true
			out = append(out, real)
		}
	}
	return out
}
