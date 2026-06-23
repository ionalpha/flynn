package observe_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/observe"
)

func TestDefaultIsUsable(t *testing.T) {
	ctx := context.Background()
	o := observe.Default()
	if o.Log == nil || o.Tracer == nil || o.Meter == nil {
		t.Fatal("Default must populate Log, Tracer, and Meter")
	}
	// The no-op tracer returns a usable span and the original context.
	got, span := o.Tracer.Start(ctx, "unit")
	if got != ctx {
		t.Fatal("NopTracer should return the context unchanged")
	}
	span.SetAttr("k", "v")
	span.RecordError(nil)
	span.End()
	// The no-op meter's instruments do nothing but must not panic.
	o.Meter.Counter("c").Add(ctx, 2)
	o.Meter.Histogram("h").Record(ctx, 3.0, observe.Int("n", 1))
}

func TestNewInjectsHandler(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	o := observe.New(slog.NewTextHandler(&buf, nil), nil, nil)
	o.Log.Info(ctx, "hello", observe.String("who", "world"))
	if !strings.Contains(buf.String(), "who=world") {
		t.Fatalf("injected handler did not receive the log: %q", buf.String())
	}
}

// TestWithBindsFields checks a scoped logger carries its fields onto every record.
func TestWithBindsFields(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	o := observe.New(slog.NewTextHandler(&buf, nil), nil, nil)
	o.Log.With(observe.String("run_id", "r1")).Info(ctx, "step")
	if !strings.Contains(buf.String(), "run_id=r1") {
		t.Fatalf("scoped field missing: %q", buf.String())
	}
}

// TestContextRoundTrip checks Into/FromContext return the bound Observability and
// fall back to the no-op default when absent.
func TestContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	if got := observe.FromContext(ctx); got == nil {
		t.Fatal("FromContext on a bare context must return a usable default")
	}
	o := observe.Default()
	if got := observe.FromContext(observe.Into(ctx, o)); got != o {
		t.Fatal("FromContext must return the Observability bound by Into")
	}
}
