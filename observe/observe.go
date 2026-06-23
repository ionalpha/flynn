// Package observe is the agent's observability seam: structured logging, tracing,
// and metrics, all reached through Flynn-owned ports so the agent never depends on a
// concrete backend. This mirrors OpenTelemetry's own API/SDK split: this package
// is a dependency-light API with no-op defaults, and an opt-in adapter
// (observe/otel) substitutes the real SDK without any call site changing.
//
// The standalone build uses the no-op defaults here (logs discarded, spans and
// metrics dropped) so the agent runs with zero setup and near-zero overhead. A
// host such as an Ion Alpha instance injects a real slog.Handler plus Tracer and
// Meter adapters via New, without this package importing either. Only this package
// may import log/slog; every other package logs through the Logger port.
package observe

import (
	"context"
	"io"
	"log/slog"
)

// Observability bundles the logger, tracer, and meter threaded through the
// runtime. The zero value is not usable; construct one with Default or New.
type Observability struct {
	Log    Logger
	Tracer Tracer
	Meter  Meter
}

// Default returns an Observability that discards everything: logs to io.Discard, a
// no-op tracer, and a no-op meter. It is the zero-setup standalone default.
func Default() *Observability {
	return &Observability{
		Log:    NewSlogLogger(slog.NewTextHandler(io.Discard, nil)),
		Tracer: NopTracer{},
		Meter:  NopMeter{},
	}
}

// New builds an Observability from a slog.Handler, a Tracer, and a Meter. A nil
// handler discards logs; a nil tracer or meter falls back to the no-op. Hosts call
// this to inject their logging handler and telemetry backends.
func New(h slog.Handler, t Tracer, m Meter) *Observability {
	if h == nil {
		h = slog.NewTextHandler(io.Discard, nil)
	}
	if t == nil {
		t = NopTracer{}
	}
	if m == nil {
		m = NopMeter{}
	}
	return &Observability{Log: NewSlogLogger(h), Tracer: t, Meter: m}
}

// Field is a typed key/value attribute on a log record, span, or metric. Build one
// with the typed constructors (String, Int, Err, ...) so call sites never touch
// the logging backend's own attribute types.
type Field struct {
	Key   string
	Value any
}

// String returns a string-valued Field.
func String(key, v string) Field { return Field{key, v} }

// Int returns an int-valued Field.
func Int(key string, v int) Field { return Field{key, v} }

// Int64 returns an int64-valued Field.
func Int64(key string, v int64) Field { return Field{key, v} }

// Float64 returns a float64-valued Field.
func Float64(key string, v float64) Field { return Field{key, v} }

// Bool returns a bool-valued Field.
func Bool(key string, v bool) Field { return Field{key, v} }

// Any returns a Field with an arbitrary value.
func Any(key string, v any) Field { return Field{key, v} }

// Err is a Field carrying an error under the conventional "error" key.
func Err(err error) Field { return Field{"error", err} }

// Logger is the agent's structured logging port. Call sites depend on this, never
// on log/slog, so the backend stays swappable and the dependency is
// lint-enforceable. slog backs the default; a host may inject any slog.Handler.
type Logger interface {
	Debug(ctx context.Context, msg string, fields ...Field)
	Info(ctx context.Context, msg string, fields ...Field)
	Warn(ctx context.Context, msg string, fields ...Field)
	Error(ctx context.Context, msg string, fields ...Field)
	// With returns a Logger that includes fields on every record (a scoped logger),
	// e.g. binding run_id and scope once at the waist.
	With(fields ...Field) Logger
}

// slogLogger adapts log/slog to Logger. When a host enables source locations they
// point at this adapter rather than the caller, the known cost of wrapping slog;
// it can be revisited with caller-PC capture if a host needs precise sources.
type slogLogger struct{ l *slog.Logger }

// NewSlogLogger returns a Logger backed by the slog.Handler h.
func NewSlogLogger(h slog.Handler) Logger { return slogLogger{slog.New(h)} }

func (s slogLogger) Debug(ctx context.Context, msg string, f ...Field) {
	s.log(ctx, slog.LevelDebug, msg, f)
}

func (s slogLogger) Info(ctx context.Context, msg string, f ...Field) {
	s.log(ctx, slog.LevelInfo, msg, f)
}

func (s slogLogger) Warn(ctx context.Context, msg string, f ...Field) {
	s.log(ctx, slog.LevelWarn, msg, f)
}

func (s slogLogger) Error(ctx context.Context, msg string, f ...Field) {
	s.log(ctx, slog.LevelError, msg, f)
}

func (s slogLogger) With(f ...Field) Logger {
	args := make([]any, len(f))
	for i, x := range f {
		args[i] = slog.Any(x.Key, x.Value)
	}
	return slogLogger{s.l.With(args...)}
}

func (s slogLogger) log(ctx context.Context, level slog.Level, msg string, f []Field) {
	attrs := make([]slog.Attr, len(f))
	for i, x := range f {
		attrs[i] = slog.Any(x.Key, x.Value)
	}
	s.l.LogAttrs(ctx, level, msg, attrs...)
}

var _ Logger = slogLogger{}

// Tracer starts spans around units of work. It is deliberately minimal: the agent
// depends on this interface, and an adapter maps it onto a backend such as
// OpenTelemetry. Implementations must be safe for concurrent use.
type Tracer interface {
	// Start begins a span named name and returns a context carrying it together
	// with the Span to end. The returned context must be used for nested work.
	Start(ctx context.Context, name string) (context.Context, Span)
}

// Span is a single unit of traced work. End must be called exactly once;
// deferring it at the call site is the expected usage.
type Span interface {
	// SetAttr records a key/value attribute on the span.
	SetAttr(key string, value any)
	// RecordError marks the span as failed and attaches err.
	RecordError(err error)
	// End closes the span.
	End()
}

// NopTracer is a Tracer that does nothing; it is the standalone default.
type NopTracer struct{}

// Start implements Tracer by returning the context unchanged and a no-op Span.
func (NopTracer) Start(ctx context.Context, _ string) (context.Context, Span) {
	return ctx, nopSpan{}
}

type nopSpan struct{}

func (nopSpan) SetAttr(string, any) {}
func (nopSpan) RecordError(error)   {}
func (nopSpan) End()                {}

// Meter creates metric instruments. Like the tracer it is a minimal port with a
// no-op default; an adapter (observe/otel) maps it onto a backend such as
// OpenTelemetry, exporting to VictoriaMetrics. Fetching an instrument is cheap, so
// call sites may fetch one per use.
type Meter interface {
	// Counter returns a monotonic counter (requests, tokens, errors).
	Counter(name string) Counter
	// Histogram returns a distribution recorder (latency, sizes).
	Histogram(name string) Histogram
}

// Counter accumulates a monotonic total.
type Counter interface {
	Add(ctx context.Context, n int64, fields ...Field)
}

// Histogram records a distribution of values.
type Histogram interface {
	Record(ctx context.Context, value float64, fields ...Field)
}

// NopMeter is a Meter whose instruments do nothing; the standalone default.
type NopMeter struct{}

// Counter implements Meter.
func (NopMeter) Counter(string) Counter { return nopCounter{} }

// Histogram implements Meter.
func (NopMeter) Histogram(string) Histogram { return nopHistogram{} }

type nopCounter struct{}

func (nopCounter) Add(context.Context, int64, ...Field) {}

type nopHistogram struct{}

func (nopHistogram) Record(context.Context, float64, ...Field) {}

// Compile-time checks that the no-op types satisfy the observe interfaces.
var (
	_ Tracer    = NopTracer{}
	_ Span      = nopSpan{}
	_ Meter     = NopMeter{}
	_ Counter   = nopCounter{}
	_ Histogram = nopHistogram{}
)

type ctxKey struct{}

// nopObs is the shared fallback returned by FromContext when no Observability is
// bound, so the lookup never allocates on the hot path.
var nopObs = Default()

// Into returns a context carrying o, so deep call sites reach the active logger,
// tracer, and meter via FromContext instead of threading them as parameters. The
// dispatch waist binds the run's Observability once; leaf code reads it back.
func Into(ctx context.Context, o *Observability) context.Context {
	return context.WithValue(ctx, ctxKey{}, o)
}

// FromContext returns the Observability bound to ctx, or the no-op default if none
// is present, so a caller can always log, trace, and measure without a nil check.
func FromContext(ctx context.Context) *Observability {
	if o, ok := ctx.Value(ctxKey{}).(*Observability); ok && o != nil {
		return o
	}
	return nopObs
}
