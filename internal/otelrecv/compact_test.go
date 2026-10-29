package otelrecv_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"tokenspend/internal/otelrecv"
	"tokenspend/internal/spend"
)

// piUsage is a Pi request event. Token counts differ per ID because the
// loader also treats equal counts in one session as the same request.
func piUsage(id string) spend.OTelRecord {
	return spend.OTelRecord{Time: time.Now().UTC(), Signal: "log", Service: "pi", Name: "pi.api_request",
		Attrs: map[string]any{"session.id": "s", "response_id": id, "model": "claude-fable-5", "input_tokens": float64(100 + len(id)), "output_tokens": float64(id[len(id)-1])}}
}

func hookEvent() spend.OTelRecord {
	return spend.OTelRecord{Time: time.Now().UTC(), Signal: "log", Service: "claude-code", Name: "claude_code.hook_execution_start"}
}

func jsonl(t *testing.T, lines ...any) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, l := range lines {
		if s, ok := l.(string); ok {
			buf.WriteString(s + "\n")
			continue
		}
		if err := json.NewEncoder(&buf).Encode(l); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func gz(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func names(dir string) []string {
	var out []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestCompact(t *testing.T) {
	dir := t.TempDir()
	rv := &otelrecv.Receiver{Dir: dir, KeepEvents: 30 * 24 * time.Hour}
	srv := httptest.NewServer(rv.Handler())
	defer srv.Close()
	day := func(daysAgo int) string { return time.Now().AddDate(0, 0, -daysAgo).Format("2006-01-02") }
	today, d3, d5, d40 := day(0), day(3), day(5), day(40)

	// Today: one usage event and one other event, through the receiver.
	body, _ := json.Marshal(map[string]any{"resourceLogs": []any{map[string]any{
		"resource": map[string]any{"attributes": []any{map[string]any{"key": "service.name", "value": map[string]any{"stringValue": "pi"}}}},
		"scopeLogs": []any{map[string]any{"logRecords": []any{
			map[string]any{"eventName": "pi.api_request", "attributes": []any{
				map[string]any{"key": "session.id", "value": map[string]any{"stringValue": "s"}},
				map[string]any{"key": "response_id", "value": map[string]any{"stringValue": "today"}},
				map[string]any{"key": "input_tokens", "value": map[string]any{"intValue": "10"}}}},
			map[string]any{"eventName": "pi.session_start"},
		}}},
	}}})
	post(t, srv.URL+"/v1/logs", "application/json", body, false)
	if got := names(dir); !slices.Equal(got, []string{"events-" + today + ".jsonl", "usage-" + today + ".jsonl"}) {
		t.Fatalf("files after receiving = %v", got)
	}

	// Earlier days, as older receivers and earlier compactions left them.
	writeFile(t, filepath.Join(dir, "otel-"+d5+".jsonl"), jsonl(t, piUsage("d5"), hookEvent(), "not json"))
	writeFile(t, filepath.Join(dir, "usage-"+d3+".jsonl.gz"), gz(t, jsonl(t, piUsage("d3a"))))
	writeFile(t, filepath.Join(dir, "usage-"+d3+".jsonl"), jsonl(t, piUsage("d3b")))
	writeFile(t, filepath.Join(dir, "usage-"+d40+".jsonl"), jsonl(t, piUsage("d40")))
	writeFile(t, filepath.Join(dir, "events-"+d40+".jsonl"), jsonl(t, hookEvent()))
	writeFile(t, filepath.Join(dir, ".compact-123"), []byte("interrupted"))

	want := []string{
		"events-" + d5 + ".jsonl.gz",
		"events-" + today + ".jsonl",
		"usage-" + d3 + ".jsonl.gz",
		"usage-" + d40 + ".jsonl.gz",
		"usage-" + d5 + ".jsonl.gz",
		"usage-" + today + ".jsonl",
	}
	slices.Sort(want)
	for run := 1; run <= 2; run++ { // compacting twice changes nothing more
		if err := rv.Compact(); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		if got := names(dir); !slices.Equal(got, want) {
			t.Fatalf("run %d: files = %v\nwant %v", run, got, want)
		}
		d, err := spend.Load(spend.Options{OTelDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, r := range d.Requests {
			ids = append(ids, r.RequestID)
		}
		slices.Sort(ids)
		if wantIDs := []string{"d3a", "d3b", "d40", "d5", "today"}; !slices.Equal(ids, wantIDs) {
			t.Errorf("run %d: request IDs = %v, want %v", run, ids, wantIDs)
		}
	}

	// The pre-split file's other lines, including the unparseable one, moved to events.
	var events int
	bad, err := spend.ReadOTelFiles([]spend.OTelFile{{Path: filepath.Join(dir, "events-"+d5+".jsonl.gz")}}, func(*spend.OTelRecord) { events++ })
	if err != nil || events != 1 || len(bad) != 1 {
		t.Errorf("events file: %d records, bad lines %v, err %v; want 1 record and 1 bad line", events, bad, err)
	}
}
