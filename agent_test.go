package agent_test

import (
	"context"
	"strings"
	"testing"

	agent "github.com/ionalpha/flynn"
)

func TestRunRequiresAnObjective(t *testing.T) {
	// New is usable from zero config; Run reports a clear error when no objective is
	// set rather than silently doing nothing.
	err := agent.New(agent.Config{}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "objective") {
		t.Fatalf("Run with no objective = %v, want an objective error", err)
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
