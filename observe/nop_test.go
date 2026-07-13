package observe_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/observe"
)

// TestFieldConstructors proves each typed constructor puts the value under the key it
// was given, and that Err files an error under the conventional "error" key so every
// backend renders a failure the same way.
func TestFieldConstructors(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name  string
		field observe.Field
		key   string
		value any
	}{
		{"String", observe.String("k", "v"), "k", "v"},
		{"Int", observe.Int("k", 7), "k", 7},
		{"Int64", observe.Int64("k", int64(8)), "k", int64(8)},
		{"Float64", observe.Float64("k", 1.5), "k", 1.5},
		{"Bool", observe.Bool("k", true), "k", true},
		{"Any", observe.Any("k", []string{"a"}), "k", nil}, // compared below
		{"Err", observe.Err(boom), "error", boom},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.field.Key != c.key {
				t.Fatalf("key = %q, want %q", c.field.Key, c.key)
			}
			if c.name == "Any" {
				got, ok := c.field.Value.([]string)
				if !ok || len(got) != 1 || got[0] != "a" {
					t.Fatalf("Any must carry the value verbatim, got %#v", c.field.Value)
				}
				return
			}
			if c.field.Value != c.value {
				t.Fatalf("value = %#v, want %#v", c.field.Value, c.value)
			}
		})
	}
}

// TestNopLoggerDiscards proves the no-op logger drops every level and that its With
// stays a no-op, so a scoped logger built off the standalone default never starts
// writing.
func TestNopLoggerDiscards(t *testing.T) {
	ctx := context.Background()
	var l observe.Logger = observe.NopLogger{}
	l.Debug(ctx, "d", observe.String("k", "v"))
	l.Info(ctx, "i")
	l.Warn(ctx, "w")
	l.Error(ctx, "e", observe.Err(errors.New("x")))

	scoped := l.With(observe.String("run_id", "r1"))
	if _, ok := scoped.(observe.NopLogger); !ok {
		t.Fatalf("NopLogger.With must stay a no-op logger, got %T", scoped)
	}
	scoped.Error(ctx, "still discarded")
}

// TestWarnLoggerFiltersBelowWarn proves NewWarnLogger writes the records an operator
// must see and drops the rest, the exact contract a command relies on when it reports a
// condition without turning on the agent's logging.
func TestWarnLoggerFiltersBelowWarn(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	l := observe.NewWarnLogger(&buf)

	l.Debug(ctx, "debug-record")
	l.Info(ctx, "info-record")
	l.Warn(ctx, "warn-record", observe.String("who", "watchdog"))
	l.Error(ctx, "error-record", observe.Err(errors.New("sealing failed")))

	out := buf.String()
	for _, dropped := range []string{"debug-record", "info-record"} {
		if strings.Contains(out, dropped) {
			t.Fatalf("record below Warn leaked: %q", out)
		}
	}
	if !strings.Contains(out, "warn-record") || !strings.Contains(out, "who=watchdog") {
		t.Fatalf("warn record missing: %q", out)
	}
	if !strings.Contains(out, "error-record") || !strings.Contains(out, "sealing failed") {
		t.Fatalf("error record missing: %q", out)
	}
}

// TestWarnLoggerScopedFieldsCarry proves a scoped warn logger binds its fields onto
// the records it does emit.
func TestWarnLoggerScopedFieldsCarry(t *testing.T) {
	var buf bytes.Buffer
	observe.NewWarnLogger(&buf).
		With(observe.String("cmd", "seal")).
		Warn(context.Background(), "leak", observe.Int("count", 2))

	out := buf.String()
	if !strings.Contains(out, "cmd=seal") || !strings.Contains(out, "count=2") {
		t.Fatalf("scoped and record fields must both appear: %q", out)
	}
}

// TestNopTelemetryDrops proves the no-op tracer and meter accept every call and return
// usable instruments, so code on the standalone path traces and measures unconditionally.
func TestNopTelemetryDrops(t *testing.T) {
	ctx := context.Background()

	var tr observe.Tracer = observe.NopTracer{}
	got, span := tr.Start(ctx, "unit")
	if got != ctx {
		t.Fatal("NopTracer must return the context unchanged")
	}
	span.SetAttr("k", "v")
	span.RecordError(errors.New("boom"))
	span.End()

	var m observe.Meter = observe.NopMeter{}
	c := m.Counter("requests")
	if c == nil {
		t.Fatal("NopMeter must return a usable counter")
	}
	c.Add(ctx, 3, observe.String("k", "v"))
	h := m.Histogram("latency")
	if h == nil {
		t.Fatal("NopMeter must return a usable histogram")
	}
	h.Record(ctx, 12.5, observe.Bool("ok", true))
}

// TestDefaultLoggerIsNop proves the standalone default's logger is the true no-op, not
// slog over io.Discard: a disabled log call must reach no handler at all.
func TestDefaultLoggerIsNop(t *testing.T) {
	if _, ok := observe.Default().Log.(observe.NopLogger); !ok {
		t.Fatalf("Default().Log = %T, want NopLogger", observe.Default().Log)
	}
}
