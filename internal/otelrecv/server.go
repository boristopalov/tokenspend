// Package otelrecv is a minimal OTLP/HTTP receiver. It accepts logs and
// metrics (protobuf or JSON, optionally gzipped) and appends them, flattened,
// to one JSONL file per kind and day (see spend.OTelFile). Traces are
// accepted and dropped.
package otelrecv

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	metrics "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"tokenspend/internal/spend"
)

const maxBody = 64 << 20

// DefaultKeepEvents is how long non-usage events are kept.
const DefaultKeepEvents = 30 * 24 * time.Hour

// Receiver stores OTLP exports in Dir. Billed request events go to usage
// files, which are kept forever: once Claude Code deletes old transcripts
// they are the only record of those requests. Everything else goes to
// events files, which Compact deletes after KeepEvents.
type Receiver struct {
	Dir        string
	KeepEvents time.Duration // 0 keeps events forever

	mu sync.Mutex // serializes writes and compaction
}

// ListenAndServe compacts Dir, then serves on addr until it fails,
// compacting again every hour.
func (rv *Receiver) ListenAndServe(addr string) error {
	if err := os.MkdirAll(rv.Dir, 0o755); err != nil {
		return err
	}
	compact := func() {
		if err := rv.Compact(); err != nil {
			log.Printf("compact: %v", err)
		}
	}
	compact()
	go func() {
		for range time.Tick(time.Hour) {
			compact()
		}
	}()
	srv := &http.Server{Addr: addr, Handler: rv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("tokenspend otel receiver listening on http://%s, writing to %s", addr, rv.Dir)
	return srv.ListenAndServe()
}

// Handler serves /v1/logs, /v1/metrics and /v1/traces.
func (rv *Receiver) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/logs", rv.handle(func(body []byte, isJSON bool) ([]spend.OTelRecord, error) {
		var req collogs.ExportLogsServiceRequest
		if err := unmarshal(body, isJSON, &req); err != nil {
			return nil, err
		}
		return flattenLogs(&req), nil
	}))
	mux.HandleFunc("/v1/metrics", rv.handle(func(body []byte, isJSON bool) ([]spend.OTelRecord, error) {
		var req colmetrics.ExportMetricsServiceRequest
		if err := unmarshal(body, isJSON, &req); err != nil {
			return nil, err
		}
		return flattenMetrics(&req), nil
	}))
	mux.HandleFunc("/v1/traces", rv.handle(func([]byte, bool) ([]spend.OTelRecord, error) { return nil, nil }))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	return mux
}

func (rv *Receiver) handle(decode func(body []byte, isJSON bool) ([]spend.OTelRecord, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		body, err := readBody(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		isJSON := strings.Contains(r.Header.Get("Content-Type"), "json")
		recs, err := decode(body, isJSON)
		if err != nil {
			log.Printf("%s: %v", r.URL.Path, err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := rv.write(recs); err != nil {
			log.Printf("write: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// An empty Export*ServiceResponse means full success in both encodings.
		if isJSON {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, "{}")
		} else {
			w.Header().Set("Content-Type", "application/x-protobuf")
		}
	}
}

func readBody(r *http.Request) ([]byte, error) {
	var rd io.Reader = http.MaxBytesReader(nil, r.Body, maxBody)
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(rd)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		rd = io.LimitReader(gz, maxBody)
	}
	return io.ReadAll(rd)
}

func unmarshal(body []byte, isJSON bool, m proto.Message) error {
	if isJSON {
		return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(body, m)
	}
	return proto.Unmarshal(body, m)
}

// write appends recs to today's usage and events files.
func (rv *Receiver) write(recs []spend.OTelRecord) error {
	byKind := map[string]*bytes.Buffer{}
	for i := range recs {
		kind := spend.OTelEvents
		if spend.IsOTelRequest(&recs[i]) {
			kind = spend.OTelUsage
		}
		if byKind[kind] == nil {
			byKind[kind] = &bytes.Buffer{}
		}
		if err := json.NewEncoder(byKind[kind]).Encode(&recs[i]); err != nil {
			return err
		}
	}
	rv.mu.Lock()
	defer rv.mu.Unlock()
	day := time.Now().Format(dayLayout)
	for kind, buf := range byKind {
		if err := appendFile(filepath.Join(rv.Dir, spend.OTelFileName(kind, day)), buf.Bytes()); err != nil {
			return err
		}
	}
	return nil
}

// appendFile appends data to path. The directory is created here rather than
// once at startup so the receiver recovers if it is removed while running.
func appendFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func flattenLogs(req *collogs.ExportLogsServiceRequest) []spend.OTelRecord {
	var out []spend.OTelRecord
	for _, rl := range req.GetResourceLogs() {
		res := attrMap(rl.GetResource().GetAttributes())
		service, _ := res["service.name"].(string)
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				attrs := attrMap(lr.GetAttributes())
				body, _ := anyValue(lr.GetBody()).(string)
				ts := lr.GetTimeUnixNano()
				if ts == 0 {
					ts = lr.GetObservedTimeUnixNano()
				}
				rec := spend.OTelRecord{
					Time: unixNano(ts), Signal: "log", Service: service, Resource: res, Attrs: attrs,
					Name: eventName(lr.GetEventName(), attrs, body),
				}
				if rec.Name != body {
					rec.Body = body
				}
				out = append(out, rec)
			}
		}
	}
	return out
}

// eventName finds the namespaced event name wherever the exporter put it.
// Claude Code uses the record's event_name ("claude_code.api_request", with
// event.name="api_request"); Codex sets event_name to a source location and
// puts "codex.sse_event" in the event.name attribute.
func eventName(eventName string, attrs map[string]any, body string) string {
	attr, _ := attrs["event.name"].(string)
	candidates := []string{eventName, attr, body}
	for i, c := range candidates {
		if len(c) > 100 || strings.ContainsAny(c, " \n") {
			candidates[i] = ""
		}
	}
	for _, c := range candidates {
		if strings.Contains(c, ".") {
			return c
		}
	}
	return firstNonEmpty(candidates...)
}

func flattenMetrics(req *colmetrics.ExportMetricsServiceRequest) []spend.OTelRecord {
	var out []spend.OTelRecord
	for _, rm := range req.GetResourceMetrics() {
		res := attrMap(rm.GetResource().GetAttributes())
		service, _ := res["service.name"].(string)
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				emit := func(ts uint64, attrs []*common.KeyValue, v float64, extra map[string]any) {
					a := attrMap(attrs)
					for k, x := range extra {
						a[k] = x
					}
					out = append(out, spend.OTelRecord{Time: unixNano(ts), Signal: "metric", Service: service, Name: m.GetName(), Attrs: a, Resource: res, Value: &v})
				}
				number := func(dp *metrics.NumberDataPoint) float64 {
					if _, ok := dp.GetValue().(*metrics.NumberDataPoint_AsInt); ok {
						return float64(dp.GetAsInt())
					}
					return dp.GetAsDouble()
				}
				for _, dp := range m.GetSum().GetDataPoints() {
					emit(dp.GetTimeUnixNano(), dp.GetAttributes(), number(dp), nil)
				}
				for _, dp := range m.GetGauge().GetDataPoints() {
					emit(dp.GetTimeUnixNano(), dp.GetAttributes(), number(dp), nil)
				}
				for _, dp := range m.GetHistogram().GetDataPoints() {
					emit(dp.GetTimeUnixNano(), dp.GetAttributes(), dp.GetSum(), map[string]any{"count": dp.GetCount()})
				}
				for _, dp := range m.GetExponentialHistogram().GetDataPoints() {
					emit(dp.GetTimeUnixNano(), dp.GetAttributes(), dp.GetSum(), map[string]any{"count": dp.GetCount()})
				}
			}
		}
	}
	return out
}

func attrMap(kvs []*common.KeyValue) map[string]any {
	m := make(map[string]any, len(kvs))
	for _, kv := range kvs {
		m[kv.GetKey()] = anyValue(kv.GetValue())
	}
	return m
}

func anyValue(v *common.AnyValue) any {
	switch x := v.GetValue().(type) {
	case *common.AnyValue_StringValue:
		return x.StringValue
	case *common.AnyValue_BoolValue:
		return x.BoolValue
	case *common.AnyValue_IntValue:
		return x.IntValue
	case *common.AnyValue_DoubleValue:
		return x.DoubleValue
	case *common.AnyValue_BytesValue:
		return base64.StdEncoding.EncodeToString(x.BytesValue)
	case *common.AnyValue_ArrayValue:
		out := make([]any, 0, len(x.ArrayValue.GetValues()))
		for _, e := range x.ArrayValue.GetValues() {
			out = append(out, anyValue(e))
		}
		return out
	case *common.AnyValue_KvlistValue:
		return attrMap(x.KvlistValue.GetValues())
	}
	return nil
}

func unixNano(ns uint64) time.Time {
	if ns == 0 {
		return time.Now().UTC()
	}
	return time.Unix(0, int64(ns)).UTC()
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
