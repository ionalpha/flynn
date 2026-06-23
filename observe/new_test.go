package observe_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/observe"
)

// New(nil, nil, nil) must fill all defaults: a discarding logger, the no-op
// tracer, and the no-op meter. None of them may panic or produce output.
func TestNewNilArgsDiscard(t *testing.T) {
	ctx := context.Background()
	o := observe.New(nil, nil, nil)
	if o.Log == nil || o.Tracer == nil || o.Meter == nil {
		t.Fatal("New(nil, nil, nil) must populate Log, Tracer, and Meter")
	}
	o.Log.Info(ctx, "dropped", observe.String("k", "v")) // discarded, no panic

	_, span := o.Tracer.Start(ctx, "x")
	span.SetAttr("a", 1)
	span.RecordError(errors.New("e"))
	span.End()

	o.Meter.Counter("c").Add(ctx, 1, observe.String("k", "v"))
	o.Meter.Histogram("h").Record(ctx, 1.5)
}

// A nil tracer or meter falls back to the no-op even when a real handler is
// supplied.
func TestNewNilTracerFallsBack(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	o := observe.New(slog.NewTextHandler(&buf, nil), nil, nil)
	if _, ok := o.Tracer.(observe.NopTracer); !ok {
		t.Fatalf("nil tracer should fall back to NopTracer, got %T", o.Tracer)
	}
	if _, ok := o.Meter.(observe.NopMeter); !ok {
		t.Fatalf("nil meter should fall back to NopMeter, got %T", o.Meter)
	}
	o.Log.Info(ctx, "kept", observe.String("who", "world"))
	if !strings.Contains(buf.String(), "who=world") {
		t.Fatalf("handler did not receive the log: %q", buf.String())
	}
}
