package otelrecv

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tokenspend/internal/spend"
)

const (
	dayLayout  = "2006-01-02"
	tempPrefix = ".compact-"
)

// Compact tidies Dir. For every day before today it splits pre-split otel-*
// files into usage and events files, gzips plain files, and deletes events
// files older than KeepEvents. Today's files, which are still being written,
// are left alone. It is safe to call while the receiver is serving.
func (rv *Receiver) Compact() error {
	rv.mu.Lock()
	defer rv.mu.Unlock()
	now := time.Now()
	today := now.Format(dayLayout)
	var errs []error

	// Leftovers from a compaction that was interrupted.
	if temps, _ := filepath.Glob(filepath.Join(rv.Dir, tempPrefix+"*")); len(temps) > 0 {
		for _, t := range temps {
			errs = append(errs, os.Remove(t))
		}
	}
	// Split first so the halves are compressed (or expired) below.
	for _, f := range spend.OTelFiles(rv.Dir, false) {
		if f.Kind == spend.OTelLegacy && f.Day < today {
			errs = append(errs, splitLegacy(rv.Dir, f))
		}
	}
	for _, f := range spend.OTelFiles(rv.Dir, false) {
		if f.Day >= today {
			continue
		}
		if f.Kind == spend.OTelEvents && rv.KeepEvents > 0 && expired(f.Day, now, rv.KeepEvents) {
			errs = append(errs, os.Remove(f.Path))
			continue
		}
		if !f.Gzip {
			errs = append(errs, gzipInto(f.Path, strings.TrimSuffix(f.Path, ".jsonl")+".jsonl.gz"))
		}
	}
	return errors.Join(errs...)
}

// expired reports whether a whole day lies more than keep before now.
func expired(day string, now time.Time, keep time.Duration) bool {
	t, err := time.ParseInLocation(dayLayout, day, time.Local)
	return err == nil && t.AddDate(0, 0, 1).Before(now.Add(-keep))
}

// splitLegacy moves the lines of a pre-split otel-* file into the usage and
// events files for its day, unchanged, then removes it. Lines that are not
// valid records go to events.
func splitLegacy(dir string, f spend.OTelFile) error {
	in, err := os.Open(f.Path)
	if err != nil {
		return err
	}
	defer in.Close()
	var usage, events bytes.Buffer
	r := bufio.NewReader(in)
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if line[len(line)-1] != '\n' {
				line = append(line, '\n')
			}
			var rec spend.OTelRecord
			if json.Unmarshal(line, &rec) == nil && spend.IsOTelRequest(&rec) {
				usage.Write(line)
			} else {
				events.Write(line)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	for kind, buf := range map[string]*bytes.Buffer{spend.OTelUsage: &usage, spend.OTelEvents: &events} {
		if buf.Len() == 0 {
			continue
		}
		if err := appendFile(filepath.Join(dir, spend.OTelFileName(kind, f.Day)), buf.Bytes()); err != nil {
			return err
		}
	}
	return os.Remove(f.Path)
}

// gzipInto appends src, compressed, to dst as a new gzip member (readers see
// concatenated members as one stream) and removes src. dst is replaced by
// rename, so a crash leaves the old or the new dst, never a torn one; at
// worst src is then compressed twice on the next run, and loading drops the
// duplicate requests.
func gzipInto(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), tempPrefix+"*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := copyExisting(tmp, dst); err != nil {
		tmp.Close()
		return err
	}
	zw := gzip.NewWriter(tmp)
	if _, err := io.Copy(zw, in); err != nil {
		tmp.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return err
	}
	return os.Remove(src)
}

// copyExisting copies path into w if it exists.
func copyExisting(w io.Writer, path string) error {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}
