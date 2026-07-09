package observe_test

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/observe"
)

// record is one log record a captureHandler saw.
type record struct {
	level slog.Level
	msg   string
	attrs map[string]any
}

// captureHandler is a slog.Handler that records everything at or above its
// minimum level, including attributes bound via WithAttrs.
type captureHandler struct {
	mu    *sync.Mutex
	min   slog.Level
	bound []slog.Attr
	out   *[]record
}

func (h *captureHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.min }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	rec := record{level: r.Level, msg: r.Message, attrs: map[string]any{}}
	for _, a := range h.bound {
		rec.attrs[a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.Any()
		return true
	})
	h.mu.Lock()
	*h.out = append(*h.out, rec)
	h.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	nh := *h
	nh.bound = append(append([]slog.Attr{}, h.bound...), attrs...)
	return &nh
}

func (h *captureHandler) WithGroup(string) slog.Handler { return h }

// levels maps a drawn index to the Logger method to call and its slog level.
var levels = []struct {
	name  string
	level slog.Level
	call  func(l observe.Logger, ctx context.Context, msg string, f ...observe.Field)
}{
	{"debug", slog.LevelDebug, observe.Logger.Debug},
	{"info", slog.LevelInfo, observe.Logger.Info},
	{"warn", slog.LevelWarn, observe.Logger.Warn},
	{"error", slog.LevelError, observe.Logger.Error},
}

// Property: for any handler threshold and any sequence of log calls, a record
// reaches the handler exactly when its level clears the threshold, with the
// message and every field key/value intact - level filtering drops records, not
// attributes.
func TestProp_LoggerFiltersByLevelAndKeepsFields(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		var out []record
		minLevel := []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError}[rapid.IntRange(0, 3).Draw(rt, "min")]
		l := observe.NewSlogLogger(&captureHandler{mu: &sync.Mutex{}, min: minLevel, out: &out})

		type call struct {
			levelIdx int
			msg      string
			fields   []observe.Field
		}
		calls := rapid.SliceOfN(rapid.Custom(func(t *rapid.T) call {
			n := rapid.IntRange(0, 3).Draw(t, "nFields")
			fs := make([]observe.Field, n)
			for i := range fs {
				fs[i] = observe.String(fmt.Sprintf("k%d", i), rapid.String().Draw(t, "value"))
			}
			return call{
				levelIdx: rapid.IntRange(0, 3).Draw(t, "level"),
				msg:      rapid.StringMatching(`[a-z ]{1,20}`).Draw(t, "msg"),
				fields:   fs,
			}
		}), 0, 10).Draw(rt, "calls")

		want := 0
		for _, c := range calls {
			lv := levels[c.levelIdx]
			lv.call(l, context.Background(), c.msg, c.fields...)
			if lv.level < minLevel {
				continue
			}
			rec := out[want]
			want++
			if rec.msg != c.msg || rec.level != lv.level {
				rt.Fatalf("record = %q@%v, want %q@%v", rec.msg, rec.level, c.msg, lv.level)
			}
			for _, f := range c.fields {
				if got, ok := rec.attrs[f.Key]; !ok || got != f.Value {
					rt.Fatalf("attr %q = %v (present %v), want %v", f.Key, got, ok, f.Value)
				}
			}
		}
		if len(out) != want {
			rt.Fatalf("handler saw %d records, want %d", len(out), want)
		}
	})
}

// Property: fields bound with With appear on every subsequent record, however
// many records follow, so a scoped logger never drops its binding.
func TestProp_WithBindsFieldsOnEveryRecord(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		var out []record
		l := observe.NewSlogLogger(&captureHandler{mu: &sync.Mutex{}, min: slog.LevelDebug, out: &out})

		nBound := rapid.IntRange(1, 3).Draw(rt, "nBound")
		bound := make([]observe.Field, nBound)
		for i := range bound {
			bound[i] = observe.String(fmt.Sprintf("b%d", i), rapid.String().Draw(rt, "bval"))
		}
		scoped := l.With(bound...)

		n := rapid.IntRange(1, 5).Draw(rt, "n")
		for range n {
			scoped.Info(context.Background(), "m")
		}
		if len(out) != n {
			rt.Fatalf("handler saw %d records, want %d", len(out), n)
		}
		for i, rec := range out {
			for _, f := range bound {
				if got, ok := rec.attrs[f.Key]; !ok || got != f.Value {
					rt.Fatalf("record %d: bound attr %q = %v (present %v), want %v", i, f.Key, got, ok, f.Value)
				}
			}
		}
	})
}

// Property: the typed Field constructors preserve their key and value exactly.
func TestProp_FieldConstructorsPreserveKV(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		key := rapid.StringMatching(`[a-z]{1,8}`).Draw(rt, "key")
		switch rapid.IntRange(0, 4).Draw(rt, "kind") {
		case 0:
			v := rapid.String().Draw(rt, "v")
			if f := observe.String(key, v); f.Key != key || f.Value != v {
				rt.Fatalf("String(%q, %q) = %+v", key, v, f)
			}
		case 1:
			v := rapid.Int().Draw(rt, "v")
			if f := observe.Int(key, v); f.Key != key || f.Value != v {
				rt.Fatalf("Int(%q, %d) = %+v", key, v, f)
			}
		case 2:
			v := rapid.Int64().Draw(rt, "v")
			if f := observe.Int64(key, v); f.Key != key || f.Value != v {
				rt.Fatalf("Int64(%q, %d) = %+v", key, v, f)
			}
		case 3:
			v := rapid.Float64().Draw(rt, "v")
			f := observe.Float64(key, v)
			if f.Key != key || f.Value != v {
				// NaN never compares equal to itself; everything else must.
				if fv, ok := f.Value.(float64); !ok || !(v != v && fv != fv) {
					rt.Fatalf("Float64(%q, %v) = %+v", key, v, f)
				}
			}
		case 4:
			v := rapid.Bool().Draw(rt, "v")
			if f := observe.Bool(key, v); f.Key != key || f.Value != v {
				rt.Fatalf("Bool(%q, %v) = %+v", key, v, f)
			}
		}
	})
}

// Property: Into/FromContext round-trip the exact Observability pointer at any
// nesting depth, and FromContext without one bound returns a usable no-op
// (never nil), so leaf code can always log without a check.
func TestProp_ContextRoundTrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		if o := observe.FromContext(context.Background()); o == nil || o.Log == nil || o.Tracer == nil || o.Meter == nil {
			rt.Fatal("FromContext on a bare context returned an unusable Observability")
		}

		// Bind several in sequence; the innermost wins, however deep.
		ctx := context.Background()
		n := rapid.IntRange(1, 5).Draw(rt, "n")
		var last *observe.Observability
		for range n {
			last = observe.Default()
			ctx = observe.Into(ctx, last)
		}
		if got := observe.FromContext(ctx); got != last {
			rt.Fatalf("FromContext = %p, want the innermost %p", got, last)
		}
	})
}
