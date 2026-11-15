// Package observe is the agent's logging, tracing and metrics API, with no-op
// defaults so the standalone agent needs no setup. A host injects a
// slog.Handler and Tracer and Meter adapters through [New]; observe/otel is the
// OpenTelemetry adapter. Only this package may import log/slog.
package observe

import (
	"context"
	"io"
	"log/slog"
)

// Observability bundles the logger, tracer and meter threaded through the
// runtime. The zero value is not usable; construct one with Default or New.
type Observability struct {
	Log    Logger
	Tracer Tracer
	Meter  Meter
}

// Default returns an Observability that discards everything. Its logger is
// [NopLogger] instead of slog over io.Discard, so a log call builds no
// attributes.
func Default() *Observability {
	return &Observability{
		Log:    NopLogger{},
		Tracer: NopTracer{},
		Meter:  NopMeter{},
	}
}

// New builds an Observability for a host's backends. A nil handler discards
// logs; a nil tracer or meter is a no-op.
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

// Field is a key/value attribute on a log record, span or metric, so call sites
// never touch a backend's attribute types.
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

// Err returns err under the key "error".
func Err(err error) Field { return Field{"error", err} }

// Logger is the structured logging port. Call sites depend on it instead of
// log/slog, so a lint rule can keep slog in this package.
type Logger interface {
	Debug(ctx context.Context, msg string, fields ...Field)
	Info(ctx context.Context, msg string, fields ...Field)
	Warn(ctx context.Context, msg string, fields ...Field)
	Error(ctx context.Context, msg string, fields ...Field)
	// With returns a Logger that adds fields to every record, such as a run_id
	// bound once at dispatch.
	With(fields ...Field) Logger
}

// slogLogger adapts log/slog to Logger. Source locations, when enabled, point
// at this adapter and not the caller.
type slogLogger struct{ l *slog.Logger }

// NewSlogLogger returns a Logger backed by the slog.Handler h.
func NewSlogLogger(h slog.Handler) Logger { return slogLogger{slog.New(h)} }

// NewWarnLogger writes Warn and Error records to w as text and drops the rest,
// so a command can report what an operator must see (a leak, a bundle that
// failed to seal) without turning on the agent's own logging.
func NewWarnLogger(w io.Writer) Logger {
	return NewSlogLogger(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

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
	// A disabled level builds nothing, so Debug calls on hot paths cost a host
	// running at Info nothing.
	if !s.l.Enabled(ctx, level) {
		return
	}
	attrs := make([]slog.Attr, len(f))
	for i, x := range f {
		attrs[i] = slog.Any(x.Key, x.Value)
	}
	s.l.LogAttrs(ctx, level, msg, attrs...)
}

var _ Logger = slogLogger{}

// NopLogger discards every record without touching its fields, so it allocates
// nothing.
type NopLogger struct{}

// Debug implements Logger.
func (NopLogger) Debug(context.Context, string, ...Field) {}

// Info implements Logger.
func (NopLogger) Info(context.Context, string, ...Field) {}

// Warn implements Logger.
func (NopLogger) Warn(context.Context, string, ...Field) {}

// Error implements Logger.
func (NopLogger) Error(context.Context, string, ...Field) {}

// With implements Logger.
func (NopLogger) With(...Field) Logger { return NopLogger{} }

var _ Logger = NopLogger{}

// Tracer starts spans around units of work. Implementations must be safe for
// concurrent use.
type Tracer interface {
	// Start begins a span. Nested work must use the returned context.
	Start(ctx context.Context, name string) (context.Context, Span)
}

// Span is one unit of traced work. End must be called exactly once, usually
// deferred.
type Span interface {
	SetAttr(key string, value any)
	// RecordError marks the span failed.
	RecordError(err error)
	End()
}

// NopTracer is a Tracer that does nothing.
type NopTracer struct{}

// Start implements Tracer and returns ctx unchanged.
func (NopTracer) Start(ctx context.Context, _ string) (context.Context, Span) {
	return ctx, nopSpan{}
}

type nopSpan struct{}

func (nopSpan) SetAttr(string, any) {}
func (nopSpan) RecordError(error)   {}
func (nopSpan) End()                {}

// Meter creates metric instruments. Fetching one is cheap, so call sites may
// fetch per use.
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

// NopMeter is a Meter whose instruments do nothing.
type NopMeter struct{}

// Counter implements Meter.
func (NopMeter) Counter(string) Counter { return nopCounter{} }

// Histogram implements Meter.
func (NopMeter) Histogram(string) Histogram { return nopHistogram{} }

type nopCounter struct{}

func (nopCounter) Add(context.Context, int64, ...Field) {}

type nopHistogram struct{}

func (nopHistogram) Record(context.Context, float64, ...Field) {}

var (
	_ Tracer    = NopTracer{}
	_ Span      = nopSpan{}
	_ Meter     = NopMeter{}
	_ Counter   = nopCounter{}
	_ Histogram = nopHistogram{}
)

type ctxKey struct{}

// nopObs is shared so FromContext never allocates.
var nopObs = Default()

// Into returns a context carrying o, which dispatch binds once per run so leaf
// code reads it with FromContext instead of taking it as a parameter.
func Into(ctx context.Context, o *Observability) context.Context {
	return context.WithValue(ctx, ctxKey{}, o)
}

// FromContext returns the Observability bound to ctx, or a no-op one, so it is
// never nil.
func FromContext(ctx context.Context) *Observability {
	if o, ok := ctx.Value(ctxKey{}).(*Observability); ok && o != nil {
		return o
	}
	return nopObs
}
