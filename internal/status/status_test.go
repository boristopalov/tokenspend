package status_test

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"tokenspend/internal/otelrecv"
	"tokenspend/internal/spend"
	"tokenspend/internal/status"
)

func write(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func exporter(t *testing.T, r status.Report, tool string) status.Exporter {
	t.Helper()
	for _, e := range r.Exporters {
		if e.Tool == tool {
			return e
		}
	}
	t.Fatalf("no exporter %q in report", tool)
	return status.Exporter{}
}

func TestWorkingSetup(t *testing.T) {
	tmp := t.TempDir()
	otelDir := filepath.Join(tmp, "otel")
	srv := httptest.NewServer((&otelrecv.Receiver{Dir: otelDir}).Handler())
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	_, port, _ := net.SplitHostPort(addr)

	// A Pi request, sent the way the Pi extension sends it.
	now := time.Now()
	body := `{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"pi"}}]},
	  "scopeLogs":[{"logRecords":[{"timeUnixNano":"` + strconv.FormatInt(now.UnixNano(), 10) + `","eventName":"pi.api_request","attributes":[
	    {"key":"session.id","value":{"stringValue":"s1"}},{"key":"model","value":{"stringValue":"claude-opus-5-5"}},
	    {"key":"input_tokens","value":{"intValue":"10"}},{"key":"output_tokens","value":{"intValue":"5"}}]}]}]}]}`
	resp, err := http.Post(srv.URL+"/v1/logs", "application/json", strings.NewReader(body))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("post: %v %v", err, resp)
	}
	resp.Body.Close()

	o := status.Options{
		Addr:    addr,
		OTelDir: otelDir,
		ClaudeSettings: []string{write(t, filepath.Join(tmp, "claude", "settings.json"), `{"env":{
			"CLAUDE_CODE_ENABLE_TELEMETRY":"1","OTEL_LOGS_EXPORTER":"otlp",
			"OTEL_EXPORTER_OTLP_PROTOCOL":"http/protobuf","OTEL_EXPORTER_OTLP_ENDPOINT":"http://localhost:`+port+`"}}`)},
		CodexConfig: write(t, filepath.Join(tmp, "codex", "config.toml"), `model = "x"

[otel]
log_user_prompt = false
exporter = { otlp-http = { endpoint = "http://`+addr+`/v1/logs", protocol = "binary" } }
`),
		PiExtension: write(t, filepath.Join(tmp, "pi", "extensions", "tokenspend-otel.ts"), "// ext"),
		Now:         now.Add(time.Minute),
	}
	r := status.Check(o)

	if !r.ReceiverUp {
		t.Fatalf("receiver not up: %s", r.ReceiverErr)
	}
	if r.Files != 1 || r.Bytes == 0 || r.BadLines != 0 {
		t.Errorf("data: files=%d bytes=%d bad=%d", r.Files, r.Bytes, r.BadLines)
	}
	for _, e := range r.Exporters {
		if !e.Configured() {
			t.Errorf("%s not configured: %+v", e.Tool, e.Configs)
		}
	}
	pi := exporter(t, r, spend.Pi)
	if pi.LastEvent.IsZero() || pi.Events24h != 1 || pi.Requests24h != 1 {
		t.Errorf("pi activity = %+v", pi)
	}
	if cc := exporter(t, r, spend.ClaudeCode); !cc.LastEvent.IsZero() || cc.Events24h != 0 {
		t.Errorf("claude code activity = %+v", cc)
	}

	var out bytes.Buffer
	if err := r.Write(&out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"receiver  up", "claude-code is configured but nothing has arrived yet"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestBrokenSetup(t *testing.T) {
	tmp := t.TempDir()
	// Reserve a port, then free it so nothing is listening there.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	cases := []struct {
		name, settings, codex, want string
	}{
		{"telemetry off", `{"env":{"OTEL_LOGS_EXPORTER":"otlp","OTEL_EXPORTER_OTLP_PROTOCOL":"http/protobuf","OTEL_EXPORTER_OTLP_ENDPOINT":"http://` + addr + `"}}`,
			"[otel]\nexporter = { otlp-grpc = { endpoint = \"http://" + addr + "\" } }\n", "CLAUDE_CODE_ENABLE_TELEMETRY is not 1"},
		{"grpc protocol, other port", `{"env":{"CLAUDE_CODE_ENABLE_TELEMETRY":"1","OTEL_LOGS_EXPORTER":"otlp","OTEL_EXPORTER_OTLP_PROTOCOL":"grpc","OTEL_EXPORTER_OTLP_ENDPOINT":"http://127.0.0.1:1"}}`,
			"[tui]\nfoo = 1\n", "is not this receiver"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			r := status.Check(status.Options{
				Addr:           addr,
				OTelDir:        filepath.Join(tmp, "missing"),
				ClaudeSettings: []string{write(t, filepath.Join(dir, "settings.json"), c.settings), filepath.Join(dir, "absent.json")},
				CodexConfig:    write(t, filepath.Join(dir, "config.toml"), c.codex),
				PiExtension:    filepath.Join(dir, "none.ts"),
				Now:            time.Now(),
			})
			if r.ReceiverUp {
				t.Error("receiver reported up on a closed port")
			}
			if r.Files != 0 {
				t.Errorf("files = %d", r.Files)
			}
			for _, e := range r.Exporters {
				if e.Configured() {
					t.Errorf("%s reported configured: %+v", e.Tool, e.Configs)
				}
			}
			cc := exporter(t, r, spend.ClaudeCode)
			if !strings.Contains(cc.Configs[0].Detail, c.want) {
				t.Errorf("claude detail = %q, want %q", cc.Configs[0].Detail, c.want)
			}
			if cc.Configs[1].Detail != "not found" {
				t.Errorf("missing settings detail = %q", cc.Configs[1].Detail)
			}
		})
	}
}
