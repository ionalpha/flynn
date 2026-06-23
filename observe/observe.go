// Package observe is the agent's observability seam: structured logging plus
// tracing, both reached through Flynn-owned types so the agent never depends on a
// concrete backend.
//
// The standalone build uses the no-op defaults here (logs discarded, spans
// dropped) so the agent runs with zero setup and zero overhead. A host such as
// an Ion Alpha instance injects a real slog.Handler (bridging into its logging
// pipeline) and a Tracer adapter (e.g. OpenTelemetry) without this package ever
// importing either.
package observe

import (
	"context"
	"io"
	"log/slog"
)

// Observability bundles the logger and tracer threaded through the runtime.
// The zero value is not usable; construct one with Default or New.
type Observability struct {
	// Log is the structured logger. The host injects the handler; the agent
	// only ever sees the standard library *slog.Logger.
	Log *slog.Logger
	// Tracer starts spans around units of work.
	Tracer Tracer
}

// Default returns an Observability that discards everything: a logger writing to
// io.Discard and a no-op tracer. It is the zero-setup standalone default.
func Default() *Observability {
	return &Observability{
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Tracer: NopTracer{},
	}
}

// New builds an Observability from a slog.Handler and a Tracer. A nil handler
// discards logs; a nil tracer is replaced with NopTracer. Hosts call this to
// inject their own logging handler and tracing backend.
func New(h slog.Handler, t Tracer) *Observability {
	if h == nil {
		h = slog.NewTextHandler(io.Discard, nil)
	}
	if t == nil {
		t = NopTracer{}
	}
	return &Observability{Log: slog.New(h), Tracer: t}
}

// Tracer starts spans around units of work. It is deliberately minimal: the
// agent depends on this interface, and an adapter maps it onto a backend such as
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

// Compile-time checks that the no-op types satisfy the observe interfaces.
var (
	_ Tracer = NopTracer{}
	_ Span   = nopSpan{}
)
