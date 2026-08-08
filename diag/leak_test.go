package diag

// Shared harness for the leak detector's tests: a watchdog wired to a capture logger,
// and the readers that turn a written bundle back into samples and manifest members.
// The tests themselves sit in the leak_*_test.go files alongside this one.

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"

	"github.com/ionalpha/flynn/observe"
)

// testWatchdog builds a watchdog whose dumps are recorded rather than written, so
// the detector's behaviour is tested without a bundle on disk.
func testWatchdog(t *testing.T, cfg LeakConfig) (*watchdog, *[]Finding) {
	t.Helper()
	findings := &[]Finding{}
	w, err := newWatchdog(cfg, func(f Finding) ([]string, error) {
		*findings = append(*findings, f)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("newWatchdog: %v", err)
	}
	return w, findings
}

// captureLogger records what the watchdog reported, so a test asserts on the record
// an operator would actually read.
type captureLogger struct {
	observe.NopLogger
	mu      sync.Mutex
	records []logRecord
}

type logRecord struct {
	msg    string
	fields []observe.Field
}

func (r logRecord) field(key string) any {
	for _, f := range r.fields {
		if f.Key == key {
			return f.Value
		}
	}
	return nil
}

func (l *captureLogger) Warn(_ context.Context, msg string, fields ...observe.Field) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, logRecord{msg: msg, fields: fields})
}

func (l *captureLogger) warnings() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.records)
}

// readSamples decodes a JSONL member as the timeline's own Sample type.
func readSamples(t *testing.T, path string) []Sample {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	var out []Sample
	dec := json.NewDecoder(f)
	for dec.More() {
		var s Sample
		if err := dec.Decode(&s); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		out = append(out, s)
	}
	return out
}

func manifestHasMember(m Manifest, name string) bool {
	for _, mem := range m.Members {
		if mem.Name == name {
			return mem.Bytes > 0 && mem.SHA256 != ""
		}
	}
	return false
}

func equalFloats(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
