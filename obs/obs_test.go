package obs_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/obs"
)

func TestDefaultIsUsable(t *testing.T) {
	o := obs.Default()
	if o.Log == nil || o.Tracer == nil {
		t.Fatal("Default must populate Log and Tracer")
	}
	// The no-op tracer returns a usable span and the original context.
	ctx := context.Background()
	got, span := o.Tracer.Start(ctx, "unit")
	if got != ctx {
		t.Fatal("NopTracer should return the context unchanged")
	}
	span.SetAttr("k", "v")
	span.RecordError(nil)
	span.End()
}

func TestNewInjectsHandler(t *testing.T) {
	var buf bytes.Buffer
	o := obs.New(slog.NewTextHandler(&buf, nil), nil)
	if o.Tracer == nil {
		t.Fatal("nil tracer should fall back to NopTracer")
	}
	o.Log.Info("hello", slog.String("who", "world"))
	if !strings.Contains(buf.String(), "who=world") {
		t.Fatalf("injected handler did not receive the log: %q", buf.String())
	}
}
