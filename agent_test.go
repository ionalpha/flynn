package agent_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	agent "github.com/ionalpha/flynn"
)

func TestNewFillsStandaloneDefaults(t *testing.T) {
	var buf bytes.Buffer
	a := agent.New(agent.Config{Model: "anthropic:claude-opus-4-8", Out: &buf})

	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// With no State or Obs supplied, New must wire the in-memory provider and the
	// no-op observability so the agent runs with zero setup.
	if got := buf.String(); !strings.Contains(got, "store=memory") {
		t.Fatalf("output %q does not report the default in-memory store", got)
	}
}

func TestRunHonoursCancelledContext(t *testing.T) {
	a := agent.New(agent.Config{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Run(ctx); err == nil {
		t.Fatal("Run should return the context error when the context is cancelled")
	}
}
