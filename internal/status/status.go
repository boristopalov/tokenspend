// Package status checks whether tokenspend's OTel layer is working: the
// receiver is up, each tool is configured to export to it, and events are
// actually arriving.
package status

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"tokenspend/internal/spend"
)

// LaunchdLabel is the launchd agent scripts/install.sh creates.
const LaunchdLabel = "com.tokenspend.otel"

// Options says where to look. DefaultOptions fills it from the environment.
type Options struct {
	Addr           string   // receiver host:port
	OTelDir        string   // where the receiver writes
	ClaudeSettings []string // Claude Code settings.json files
	CodexConfig    string   // Codex config.toml
	PiExtension    string   // installed Pi extension file
	LaunchdLabel   string   // launchd agent to check; empty skips the check
	Now            time.Time
}

// DefaultOptions uses the same paths as scripts/install.sh.
func DefaultOptions() Options {
	home, _ := os.UserHomeDir()
	settings := []string{filepath.Join(home, ".claude", "settings.json")}
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		settings = append(settings, filepath.Join(dir, "settings.json"))
	}
	settings = append(settings, strings.Fields(os.Getenv("CLAUDE_SETTINGS"))...)
	codexHome := filepath.Join(home, ".codex")
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		codexHome = dir
	}
	piDir := filepath.Join(home, ".pi", "agent")
	if dir := os.Getenv("PI_CODING_AGENT_DIR"); dir != "" {
		piDir = dir
	}
	o := Options{
		Addr:           "127.0.0.1:4318",
		OTelDir:        spend.DefaultOTelDir(),
		ClaudeSettings: dedupe(settings),
		CodexConfig:    filepath.Join(codexHome, "config.toml"),
		PiExtension:    filepath.Join(piDir, "extensions", "tokenspend-otel.ts"),
		Now:            time.Now(),
	}
	if runtime.GOOS == "darwin" {
		o.LaunchdLabel = LaunchdLabel
	}
	return o
}

// Report is the result of Check.
type Report struct {
	Addr        string
	ReceiverUp  bool
	ReceiverErr string // why the receiver check failed
	Launchd     string // e.g. "running, pid 123"; empty when not checked
	OTelDir     string
	Files       int
	Bytes       int64
	BadLines    int
	Exporters   []Exporter // Claude Code, Codex, Pi
	Now         time.Time  // when the check ran
}

// Exporter is one tool's configuration and recent activity.
type Exporter struct {
	Tool        string
	Configs     []Config
	LastEvent   time.Time // zero if the receiver never got anything from it
	Events24h   int       // all log records and metric points
	Requests24h int       // billed model requests the report will count
}

// Configured reports whether any of the tool's config files exports here.
func (e Exporter) Configured() bool {
	for _, c := range e.Configs {
		if c.OK {
			return true
		}
	}
	return false
}

// Config is the check of one config file.
type Config struct {
	Path   string
	OK     bool   // exports to this receiver
	Detail string // the endpoint when OK, otherwise what is wrong
}

// Check inspects the receiver, the tools' configs and the collected data.
func Check(o Options) Report {
	r := Report{Addr: o.Addr, OTelDir: o.OTelDir, Now: o.Now}
	r.ReceiverUp, r.ReceiverErr = checkReceiver(o.Addr)
	if o.LaunchdLabel != "" {
		r.Launchd = checkLaunchd(o.LaunchdLabel)
	}

	cc := Exporter{Tool: spend.ClaudeCode}
	for _, f := range o.ClaudeSettings {
		cc.Configs = append(cc.Configs, checkClaude(f, o.Addr))
	}
	codex := Exporter{Tool: spend.Codex, Configs: []Config{checkCodex(o.CodexConfig, o.Addr)}}
	pi := Exporter{Tool: spend.Pi, Configs: []Config{checkPi(o.PiExtension)}}
	byTool := map[string]*Exporter{spend.ClaudeCode: &cc, spend.Codex: &codex, spend.Pi: &pi}

	all := spend.OTelFiles(o.OTelDir, false)
	for _, f := range all {
		if fi, err := os.Stat(f.Path); err == nil {
			r.Files++
			r.Bytes += fi.Size()
		}
	}
	dayAgo := o.Now.Add(-24 * time.Hour)
	visit := func(rec *spend.OTelRecord) {
		e := byTool[spend.OTelTool(rec)]
		if e == nil {
			return
		}
		if rec.Time.After(e.LastEvent) {
			e.LastEvent = rec.Time
		}
		if rec.Time.After(dayAgo) {
			e.Events24h++
			if spend.IsOTelRequest(rec) {
				e.Requests24h++
			}
		}
	}
	read := func(files []spend.OTelFile) {
		bad, err := spend.ReadOTelFiles(files, visit)
		for _, n := range bad {
			r.BadLines += n
		}
		if err != nil {
			r.BadLines++ // an unreadable file counts as bad data
		}
	}
	// Today's and yesterday's files cover the last 24 hours. For a tool that
	// sent nothing in them, older usage files are searched newest first for
	// its last request; old events files are skipped because they are large.
	yesterday := o.Now.AddDate(0, 0, -1).Format("2006-01-02")
	var recent, older []spend.OTelFile
	for _, f := range all {
		switch {
		case f.Day >= yesterday:
			recent = append(recent, f)
		case f.Kind != spend.OTelEvents:
			older = append(older, f)
		}
	}
	read(recent)
	unseen := func() bool { return cc.LastEvent.IsZero() || codex.LastEvent.IsZero() || pi.LastEvent.IsZero() }
	for i := len(older) - 1; i >= 0 && unseen(); i-- {
		read(older[i : i+1])
	}
	r.Exporters = []Exporter{cc, codex, pi}
	return r
}

func checkReceiver(addr string) (bool, string) {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return false, "not reachable"
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "ok" {
		return false, fmt.Sprintf("something else is listening (HTTP %d to /healthz)", resp.StatusCode)
	}
	return true, ""
}

var (
	launchdState = regexp.MustCompile(`(?m)^\tstate = (.+)$`)
	launchdPID   = regexp.MustCompile(`(?m)^\tpid = (\d+)$`)
	launchdExit  = regexp.MustCompile(`(?m)^\tlast exit code = (.+)$`)
)

func checkLaunchd(label string) string {
	out, err := exec.Command("launchctl", "print", fmt.Sprintf("gui/%d/%s", os.Getuid(), label)).Output()
	if err != nil {
		return "not loaded"
	}
	s := string(out)
	state := "unknown state"
	if m := launchdState.FindStringSubmatch(s); m != nil {
		state = m[1]
	}
	if m := launchdPID.FindStringSubmatch(s); m != nil {
		return state + ", pid " + m[1]
	}
	if m := launchdExit.FindStringSubmatch(s); m != nil {
		return state + ", last exit code " + m[1]
	}
	return state
}

func checkClaude(path, addr string) Config {
	c := Config{Path: path}
	raw, err := os.ReadFile(path)
	if err != nil {
		c.Detail = "not found"
		return c
	}
	var settings struct {
		Env map[string]any `json:"env"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		c.Detail = "invalid JSON: " + err.Error()
		return c
	}
	env := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := settings.Env[k]; ok {
				return fmt.Sprint(v)
			}
		}
		return ""
	}
	endpoint := env("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT")
	protocol := env("OTEL_EXPORTER_OTLP_LOGS_PROTOCOL", "OTEL_EXPORTER_OTLP_PROTOCOL")
	var problems []string
	if env("CLAUDE_CODE_ENABLE_TELEMETRY") != "1" {
		problems = append(problems, "CLAUDE_CODE_ENABLE_TELEMETRY is not 1")
	}
	if !strings.Contains(env("OTEL_LOGS_EXPORTER"), "otlp") {
		problems = append(problems, "OTEL_LOGS_EXPORTER is not otlp")
	}
	if !strings.HasPrefix(protocol, "http/") {
		problems = append(problems, fmt.Sprintf("protocol %q is not http/protobuf or http/json", protocol))
	}
	if !pointsAt(endpoint, addr) {
		problems = append(problems, fmt.Sprintf("endpoint %q is not this receiver", endpoint))
	}
	if len(problems) > 0 {
		c.Detail = strings.Join(problems, "; ")
		return c
	}
	c.OK, c.Detail = true, endpoint
	return c
}

var tomlEndpoint = regexp.MustCompile(`endpoint\s*=\s*"([^"]*)"`)

// checkCodex looks for the [otel] table (or its subtables) in config.toml.
// It is a line scan, not a TOML parser, which is enough for the forms
// install.sh writes and the Codex docs show.
func checkCodex(path, addr string) Config {
	c := Config{Path: path}
	raw, err := os.ReadFile(path)
	if err != nil {
		c.Detail = "not found"
		return c
	}
	inOTel, seen := false, false
	var otel []string
	for _, line := range strings.Split(string(raw), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") {
			inOTel = t == "[otel]" || strings.HasPrefix(t, "[otel.")
			seen = seen || inOTel
		}
		if inOTel && !strings.HasPrefix(t, "#") {
			otel = append(otel, t)
		}
	}
	if !seen {
		c.Detail = "no [otel] section"
		return c
	}
	section := strings.Join(otel, "\n")
	if strings.Contains(section, "otlp-grpc") {
		c.Detail = "exporter is otlp-grpc; the receiver only speaks OTLP/HTTP"
		return c
	}
	m := tomlEndpoint.FindStringSubmatch(section)
	if m == nil || !strings.Contains(section, "otlp-http") {
		c.Detail = "[otel] has no otlp-http exporter"
		return c
	}
	if !pointsAt(m[1], addr) {
		c.Detail = fmt.Sprintf("endpoint %q is not this receiver", m[1])
		return c
	}
	c.OK, c.Detail = true, m[1]
	return c
}

func checkPi(path string) Config {
	c := Config{Path: path}
	if _, err := os.Stat(path); err != nil {
		c.Detail = "extension not installed"
		return c
	}
	if os.Getenv("TOKENSPEND_OTEL") == "0" {
		c.Detail = "installed, but TOKENSPEND_OTEL=0 disables it in this environment"
		return c
	}
	c.OK, c.Detail = true, "extension installed"
	return c
}

// pointsAt reports whether an OTLP endpoint URL reaches the receiver at addr.
// Loopback names are interchangeable.
func pointsAt(endpoint, addr string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return false
	}
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	wantHost, wantPort, err := net.SplitHostPort(addr)
	if err != nil || port != wantPort {
		return false
	}
	return host == wantHost || (isLoopback(host) && isLoopback(wantHost))
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Write prints the report for a terminal.
func (r Report) Write(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	receiver := "up at http://" + r.Addr
	if !r.ReceiverUp {
		receiver = "DOWN: " + r.ReceiverErr + " at http://" + r.Addr
	}
	fmt.Fprintf(tw, "receiver\t%s\n", receiver)
	if r.Launchd != "" {
		fmt.Fprintf(tw, "launchd\t%s: %s\n", LaunchdLabel, r.Launchd)
	}
	files := "files"
	if r.Files == 1 {
		files = "file"
	}
	data := fmt.Sprintf("%s: %d %s, %s", tilde(r.OTelDir), r.Files, files, size(r.Bytes))
	if r.BadLines > 0 {
		data += fmt.Sprintf(", %d unreadable lines", r.BadLines)
	}
	fmt.Fprintf(tw, "data\t%s\n", data)

	fmt.Fprintf(tw, "\nexporter\tconfig\tlast event\tevents 24h\trequests 24h\n")
	for _, e := range r.Exporters {
		cfg := "off"
		if e.Configured() {
			cfg = "on"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\n", e.Tool, cfg, ago(e.LastEvent, r.Now), e.Events24h, e.Requests24h)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(w)
	for _, e := range r.Exporters {
		for _, c := range e.Configs {
			mark := "ok  "
			if !c.OK {
				mark = "--  "
			}
			fmt.Fprintf(w, "%s %-12s %s: %s\n", mark, e.Tool, tilde(c.Path), c.Detail)
		}
	}

	var hints []string
	if !r.ReceiverUp {
		hints = append(hints, "start the receiver with `tokenspend otel`, or install it as a launchd agent with scripts/install.sh")
	}
	for _, e := range r.Exporters {
		if e.Configured() && e.LastEvent.IsZero() {
			hints = append(hints, fmt.Sprintf("%s is configured but nothing has arrived yet; only sessions started after the config change export", e.Tool))
		}
	}
	if len(hints) > 0 {
		fmt.Fprintln(w)
		for _, h := range hints {
			fmt.Fprintln(w, "note:", h)
		}
	}
	return nil
}

func ago(t, now time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return t.Local().Format("2006-01-02")
}

func size(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func tilde(p string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home+"/") {
		return "~" + p[len(home):]
	}
	return p
}

func dedupe(paths []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range paths {
		key := p
		if real, err := filepath.EvalSymlinks(p); err == nil {
			key = real
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, p)
		}
	}
	return out
}
