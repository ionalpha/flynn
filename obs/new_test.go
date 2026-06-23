package obs_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/obs"
)

// New(nil, nil) must fill both defaults: a discarding logger and the no-op
// tracer. Logging must not panic and must produce no output.
func TestNewNilHandlerDiscards(t *testing.T) {
	o := obs.New(nil, nil)
	if o.Log == nil || o.Tracer == nil {
		t.Fatal("New(nil, nil) must populate Log and Tracer")
	}
	o.Log.Info("dropped", slog.String("k", "v")) // discarded, no panic

	_, span := o.Tracer.Start(context.Background(), "x")
	span.SetAttr("a", 1)
	span.RecordError(errors.New("e"))
	span.End()
}

// A nil tracer falls back to NopTracer even when a real handler is supplied.
func TestNewNilTracerFallsBack(t *testing.T) {
	var buf bytes.Buffer
	o := obs.New(slog.NewTextHandler(&buf, nil), nil)
	if _, ok := o.Tracer.(obs.NopTracer); !ok {
		t.Fatalf("nil tracer should fall back to NopTracer, got %T", o.Tracer)
	}
	o.Log.Info("kept", slog.String("who", "world"))
	if !strings.Contains(buf.String(), "who=world") {
		t.Fatalf("handler did not receive the log: %q", buf.String())
	}
}
