// Command tokenspend shows where Claude Code, Codex and Pi tokens go.
//
//	tokenspend report  [-o dashboard.html]   build the HTML dashboard
//	tokenspend summary                       print totals by tool, source and model
//	tokenspend export  [-format jsonl|csv]   dump the normalized request table
//	tokenspend otel    [-addr 127.0.0.1:4318] run the OTLP/HTTP receiver
//	tokenspend status                        check the receiver and each tool's exporter
//	tokenspend prices                        print the built-in price table
package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"tokenspend/internal/otelrecv"
	"tokenspend/internal/report"
	"tokenspend/internal/spend"
	"tokenspend/internal/status"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "report":
		err = cmdReport(args)
	case "summary":
		err = cmdSummary(args)
	case "export":
		err = cmdExport(args)
	case "otel":
		err = cmdOTel(args)
	case "status":
		err = cmdStatus(args)
	case "prices":
		_, err = os.Stdout.Write(spend.DefaultPricesJSON())
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tokenspend:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: tokenspend <command> [flags]

commands:
  report    build the HTML dashboard
  summary   print totals by tool, source and model
  export    write the normalized request table (jsonl or csv)
  otel      run the local OTLP/HTTP receiver
  status    check the receiver, each tool's exporter config, and recent events
  prices    print the built-in price table (copy it, edit it, pass -prices)

Run "tokenspend <command> -h" for flags.
`)
}

// loadFlags registers the flags shared by commands that read logs.
type loadFlags struct {
	since, prices, otelDir *string
	claude, codex, pi      *string
	quiet                  *bool
}

func addLoadFlags(fs *flag.FlagSet) loadFlags {
	return loadFlags{
		since:   fs.String("since", "", "only include requests after this: a duration like 7d or 12h, or a date like 2026-10-01"),
		prices:  fs.String("prices", "", "price table JSON (default: built-in; see `tokenspend prices`)"),
		otelDir: fs.String("otel-dir", spend.DefaultOTelDir(), "directory the OTel receiver writes to (empty to skip)"),
		claude:  fs.String("claude-dir", "", "Claude Code projects dirs, comma-separated (default ~/.claude/projects)"),
		codex:   fs.String("codex-dir", "", "Codex sessions dirs, comma-separated (default ~/.codex/sessions,archived_sessions)"),
		pi:      fs.String("pi-dir", "", "Pi sessions dirs, comma-separated (default ~/.pi/agent/sessions)"),
		quiet:   fs.Bool("quiet", false, "don't show progress on stderr"),
	}
}

// load reads the logs, showing progress unless -quiet. The returned
// progress stays usable for the command's own later stages.
func (f loadFlags) load() (*spend.Dataset, *progress, error) {
	p := newProgress(*f.quiet)
	opts := spend.DefaultOptions()
	opts.Progress = p.update
	opts.OTelDir = *f.otelDir
	if *f.claude != "" {
		opts.ClaudeProjectDirs = strings.Split(*f.claude, ",")
	}
	if *f.codex != "" {
		opts.CodexSessionDirs = strings.Split(*f.codex, ",")
	}
	if *f.pi != "" {
		opts.PiSessionDirs = strings.Split(*f.pi, ",")
	}
	prices, err := spend.LoadPrices(*f.prices)
	if err != nil {
		return nil, nil, err
	}
	opts.Prices = prices
	if opts.Since, err = parseSince(*f.since); err != nil {
		return nil, nil, err
	}
	d, err := spend.Load(opts)
	if err != nil {
		p.clear()
		return nil, nil, err
	}
	return d, p, nil
}

func parseSince(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return time.Time{}, fmt.Errorf("bad -since %q", s)
		}
		return time.Now().AddDate(0, 0, -n), nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d), nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("bad -since %q: use 7d, 12h or 2026-10-01", s)
}

func cmdReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	lf := addLoadFlags(fs)
	home, _ := os.UserHomeDir()
	out := fs.String("o", filepath.Join(home, ".tokenspend", "dashboard.html"), "output HTML file")
	_ = fs.Parse(args)
	d, p, err := lf.load()
	if err != nil {
		return err
	}
	p.step("writing dashboard")
	defer p.clear()
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	if err := report.Write(f, d, time.Now()); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	p.clear()
	fmt.Printf("wrote %s (%d requests, %d context items)%s\n", *out, len(d.Requests), len(d.Items), p.elapsed())
	for _, w := range d.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	return nil
}

func cmdSummary(args []string) error {
	fs := flag.NewFlagSet("summary", flag.ExitOnError)
	lf := addLoadFlags(fs)
	_ = fs.Parse(args)
	d, p, err := lf.load()
	if err != nil {
		return err
	}
	p.clear()
	type agg struct {
		n          int
		in, out    int64
		cost       float64
		readTokens int64
	}
	print := func(title string, key func(*spend.Request) string) {
		m := map[string]*agg{}
		var total float64
		for _, r := range d.Requests {
			k := key(r)
			if m[k] == nil {
				m[k] = &agg{}
			}
			a := m[k]
			a.n++
			a.in += r.InputTokens()
			a.out += r.Output
			a.readTokens += r.CacheRead
			a.cost += r.Cost.Total()
			total += r.Cost.Total()
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return m[keys[i]].cost > m[keys[j]].cost })
		fmt.Printf("\n== %s\n", title)
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
		fmt.Fprintln(w, "\trequests\tinput tok\tcache hit\toutput tok\tcost $\tshare\t")
		for _, k := range keys {
			a := m[k]
			hit := 0.0
			if a.in > 0 {
				hit = float64(a.readTokens) / float64(a.in) * 100
			}
			fmt.Fprintf(w, "%s\t%d\t%d\t%.1f%%\t%d\t%.2f\t%.1f%%\t\n", k, a.n, a.in, hit, a.out, a.cost, a.cost/total*100)
		}
		w.Flush()
	}
	print("by tool", func(r *spend.Request) string { return r.Tool })
	print("by tool / source", func(r *spend.Request) string { return r.Tool + " / " + r.Source + " / " + r.SourceDetail })
	print("by model", func(r *spend.Request) string { return r.Tool + " / " + r.Model + " [" + r.Cost.Basis + "]" })
	if len(d.ClaudeSessions) > 0 {
		fmt.Printf("\n== Claude Code sessions: reported vs computed\n")
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
		fmt.Fprintln(w, "session\treported $\ttranscript $\totel-only $\tunlogged $\t")
		for _, c := range d.ClaudeSessions {
			fmt.Fprintf(w, "%s\t%.2f\t%.2f\t%.2f\t%.2f\t\n", c.SessionID, c.ReportedUSD, c.TranscriptUSD, c.OTelOnlyUSD, c.UnloggedUSD)
		}
		w.Flush()
	}
	for _, warn := range d.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", warn)
	}
	return nil
}

func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	lf := addLoadFlags(fs)
	format := fs.String("format", "jsonl", "jsonl or csv")
	what := fs.String("table", "requests", "requests or context")
	_ = fs.Parse(args)
	d, p, err := lf.load()
	if err != nil {
		return err
	}
	p.clear()
	switch *format {
	case "jsonl":
		enc := json.NewEncoder(os.Stdout)
		if *what == "context" {
			for _, it := range d.Items {
				if err := enc.Encode(it); err != nil {
					return err
				}
			}
			return nil
		}
		for _, r := range d.Requests {
			if err := enc.Encode(r); err != nil {
				return err
			}
		}
		return nil
	case "csv":
		w := csv.NewWriter(os.Stdout)
		if *what == "context" {
			_ = w.Write([]string{"tool", "session", "thread", "project", "ts", "kind", "name", "group", "target", "est_tokens", "requests", "token_turns", "cost_usd"})
			for _, it := range d.Items {
				_ = w.Write([]string{it.Tool, it.SessionID, it.ThreadID, it.Project, it.Time.Format(time.RFC3339), it.Kind, it.Name, it.Group, it.Target,
					itoa(it.EstTokens), strconv.Itoa(it.Requests), ftoa(it.TokenTurns), ftoa(it.CostUSD)})
			}
		} else {
			_ = w.Write([]string{"tool", "session", "thread", "project", "ts", "model", "source", "source_detail", "origin", "request_id",
				"uncached", "cache_read", "cache_write", "cache_write_1h", "output", "reasoning", "cost_usd", "cost_basis", "reported_cost_usd"})
			for _, r := range d.Requests {
				_ = w.Write([]string{r.Tool, r.SessionID, r.ThreadID, r.Project, r.Time.Format(time.RFC3339), r.Model, r.Source, r.SourceDetail,
					r.Origin, r.RequestID, itoa(r.Uncached), itoa(r.CacheRead), itoa(r.CacheWrite), itoa(r.CacheWrite1h), itoa(r.Output),
					itoa(r.Reasoning), ftoa(r.Cost.Total()), r.Cost.Basis, ftoa(r.ReportedCostUSD)})
			}
		}
		w.Flush()
		return w.Error()
	}
	return fmt.Errorf("unknown -format %q", *format)
}

func cmdOTel(args []string) error {
	fs := flag.NewFlagSet("otel", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:4318", "listen address (OTLP/HTTP)")
	dir := fs.String("dir", spend.DefaultOTelDir(), "directory for the usage-* and events-* files")
	keepDays := fs.Int("keep-events-days", int(otelrecv.DefaultKeepEvents/(24*time.Hour)),
		"delete non-usage events after this many days (0 keeps them); usage events are always kept")
	_ = fs.Parse(args)
	rv := &otelrecv.Receiver{Dir: *dir, KeepEvents: time.Duration(*keepDays) * 24 * time.Hour}
	return rv.ListenAndServe(*addr)
}

func cmdStatus(args []string) error {
	o := status.DefaultOptions()
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	fs.StringVar(&o.Addr, "addr", o.Addr, "receiver address")
	fs.StringVar(&o.OTelDir, "dir", o.OTelDir, "directory the receiver writes to")
	_ = fs.Parse(args)
	r := status.Check(o)
	if err := r.Write(os.Stdout); err != nil {
		return err
	}
	if !r.ReceiverUp {
		return fmt.Errorf("receiver at %s is not running", o.Addr)
	}
	return nil
}

func itoa(n int64) string   { return strconv.FormatInt(n, 10) }
func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', 6, 64) }
